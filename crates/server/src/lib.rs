use std::{collections::BTreeMap, sync::Arc};

use axum::{
  Router as AxumRouter,
  body::Body,
  extract::{Path, State},
  http::{HeaderMap, HeaderName, HeaderValue, Method, Request, StatusCode},
  response::{IntoResponse, Response},
  routing::get,
};
use bytes::Bytes;
use futures_util::TryStreamExt;
use ncro_config::UpstreamConfig;
use ncro_db::Db;
use ncro_health::{Prober, Status, UpstreamHealth};
use ncro_router::{Router, RouterError};
use serde::Serialize;

#[derive(Clone)]
pub struct AppState {
  router:         Router,
  prober:         Prober,
  db:             Db,
  upstreams:      Vec<UpstreamConfig>,
  client:         reqwest::Client,
  cache_priority: i32,
}

/// Build the HTTP application router.
///
/// # Errors
///
/// Returns an error if the proxy HTTP client cannot be constructed.
pub fn app(
  router: Router,
  prober: Prober,
  db: Db,
  upstreams: Vec<UpstreamConfig>,
  cache_priority: i32,
) -> Result<AxumRouter, reqwest::Error> {
  let state = AppState {
    router,
    prober,
    db,
    upstreams,
    client: reqwest::Client::builder()
      .timeout(std::time::Duration::from_secs(60))
      .build()?,
    cache_priority,
  };
  Ok(
    AxumRouter::new()
      .route("/nix-cache-info", get(cache_info).head(cache_info))
      .route("/health", get(health))
      .route("/metrics", get(metrics_endpoint))
      .route("/{hash}.narinfo", get(narinfo).head(narinfo))
      .route("/nar/{*path}", get(nar).head(nar))
      .with_state(Arc::new(state)),
  )
}

async fn cache_info(State(state): State<Arc<AppState>>) -> Response {
  (
    [("content-type", "text/plain")],
    format!(
      "StoreDir: /nix/store\nWantMassQuery: 1\nPriority: {}\n",
      state.cache_priority
    ),
  )
    .into_response()
}

#[derive(Serialize)]
struct HealthResponse {
  status:    String,
  upstreams: Vec<UpstreamStatus>,
}

#[derive(Serialize)]
struct UpstreamStatus {
  url:               String,
  status:            String,
  latency_ms:        f64,
  consecutive_fails: u32,
}

async fn health(State(state): State<Arc<AppState>>) -> Response {
  let sorted = state.prober.sorted_by_latency().await;
  let down_count = sorted.iter().filter(|h| h.status == Status::Down).count();
  let any_degraded = sorted.iter().any(|h| h.status == Status::Degraded);
  let status = if !sorted.is_empty() && down_count == sorted.len() {
    "down"
  } else if down_count > 0 || any_degraded {
    "degraded"
  } else {
    "ok"
  };
  axum::Json(HealthResponse {
    status:    status.to_string(),
    upstreams: sorted
      .into_iter()
      .map(|h| {
        UpstreamStatus {
          url:               h.url,
          status:            h.status.as_str().to_string(),
          latency_ms:        h.ema_latency,
          consecutive_fails: h.consecutive_fails,
        }
      })
      .collect(),
  })
  .into_response()
}

async fn metrics_endpoint() -> Response {
  (
    [("content-type", "text/plain; version=0.0.4")],
    ncro_metrics::gather(),
  )
    .into_response()
}

async fn narinfo(
  State(state): State<Arc<AppState>>,
  Path(hash): Path<String>,
  req: Request<Body>,
) -> Response {
  let candidates = upstream_urls(&state).await;
  match state.router.resolve(&hash, &candidates).await {
    Ok(result) => {
      tracing::info!(
        hash,
        upstream = result.url,
        cache_hit = result.cache_hit,
        latency_ms = result.latency_ms,
        "narinfo routed"
      );
      ncro_metrics::get()
        .narinfo_requests
        .with_label_values(&["200"])
        .inc();
      if let Some(bytes) = result.narinfo_bytes {
        return (
          StatusCode::OK,
          [("content-type", "text/x-nix-narinfo")],
          Bytes::from(bytes),
        )
          .into_response();
      }
      proxy(
        &state.client,
        req.method().clone(),
        req.headers(),
        format!("{}{}", result.url, req.uri().path()),
      )
      .await
    },
    Err(RouterError::NotFound) => {
      ncro_metrics::get()
        .narinfo_requests
        .with_label_values(&["error"])
        .inc();
      StatusCode::NOT_FOUND.into_response()
    },
    Err(err) => {
      tracing::warn!(hash, error = %err, "narinfo resolve failed");
      ncro_metrics::get()
        .narinfo_requests
        .with_label_values(&["error"])
        .inc();
      (StatusCode::BAD_GATEWAY, "upstream unavailable").into_response()
    },
  }
}

async fn nar(
  State(state): State<Arc<AppState>>,
  req: Request<Body>,
) -> Response {
  ncro_metrics::get().nar_requests.inc();
  let nar_url = req.uri().path().trim_start_matches('/').to_string();
  if let Ok(Some(entry)) = state.db.get_route_by_nar_url(&nar_url).await
    && entry.is_valid()
    && let Some(resp) = try_nar_upstream(
      &state.client,
      req.method().clone(),
      req.headers(),
      &entry.upstream_url,
      req.uri().path(),
    )
    .await
  {
    return resp;
  }

  // Try upstreams grouped by priority as a fallback (lower = preferred), within
  // each group sorted by EMA latency.
  let mut by_priority = BTreeMap::<i32, Vec<UpstreamHealth>>::new();
  for h in state.prober.sorted_by_latency().await {
    if h.status == Status::Down {
      continue;
    }
    by_priority.entry(h.priority).or_default().push(h);
  }
  for (_priority, group) in by_priority {
    for h in group {
      if let Some(resp) = try_nar_upstream(
        &state.client,
        req.method().clone(),
        req.headers(),
        &h.url,
        req.uri().path(),
      )
      .await
      {
        return resp;
      }
    }
  }
  StatusCode::NOT_FOUND.into_response()
}

async fn upstream_urls(state: &AppState) -> Vec<String> {
  let urls = state
    .prober
    .sorted_by_latency()
    .await
    .into_iter()
    .filter(|h| h.status != Status::Down)
    .map(|h| h.url)
    .collect::<Vec<_>>();
  if urls.is_empty() {
    state.upstreams.iter().map(|u| u.url.clone()).collect()
  } else {
    urls
  }
}

async fn try_nar_upstream(
  client: &reqwest::Client,
  method: Method,
  headers: &HeaderMap,
  upstream: &str,
  path: &str,
) -> Option<Response> {
  let resp =
    upstream_request(client, method, headers, format!("{upstream}{path}"))
      .await
      .ok()?;
  if resp.status() == reqwest::StatusCode::NOT_FOUND {
    return None;
  }
  Some(response_from_reqwest(resp))
}

async fn proxy(
  client: &reqwest::Client,
  method: Method,
  headers: &HeaderMap,
  url: String,
) -> Response {
  match upstream_request(client, method, headers, url).await {
    Ok(resp) => response_from_reqwest(resp),
    Err(err) => {
      tracing::warn!(error = %err, "upstream request failed");
      (StatusCode::BAD_GATEWAY, "upstream error").into_response()
    },
  }
}

async fn upstream_request(
  client: &reqwest::Client,
  method: Method,
  headers: &HeaderMap,
  url: String,
) -> reqwest::Result<reqwest::Response> {
  let mut req = client.request(method, url);
  for name in ["accept", "accept-encoding", "range"] {
    if let Some(value) = headers.get(name) {
      req = req.header(name, value);
    }
  }
  req.send().await
}

fn response_from_reqwest(resp: reqwest::Response) -> Response {
  let status = StatusCode::from_u16(resp.status().as_u16())
    .unwrap_or(StatusCode::BAD_GATEWAY);
  let headers = resp.headers().clone();
  let stream = resp.bytes_stream().map_err(std::io::Error::other);
  let mut out = Response::builder().status(status);
  for name in [
    "content-type",
    "content-length",
    "content-encoding",
    "x-nix-signature",
    "cache-control",
    "last-modified",
  ] {
    if let Some(value) = headers.get(name)
      && let (Ok(header_name), Ok(header_value)) = (
        HeaderName::from_bytes(name.as_bytes()),
        HeaderValue::from_bytes(value.as_bytes()),
      )
    {
      out = out.header(header_name, header_value);
    }
  }
  out
    .body(Body::from_stream(stream))
    .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

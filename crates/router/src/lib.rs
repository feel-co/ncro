use std::{
  collections::{BTreeMap, HashMap},
  sync::Arc,
  time::{Duration, Instant},
};

use chrono::Utc;
use dashmap::DashMap;
use futures_util::{StreamExt, stream::FuturesUnordered};
use moka::future::Cache as MokaCache;
use ncro_db::{Db, DbError, RouteEntry};
use ncro_health::{Prober, Status};
use ncro_narinfo::{NarInfo, NarInfoError, parse_public_key};
use thiserror::Error;
use tokio::sync::{Mutex, RwLock};

#[derive(Debug, Error)]
pub enum RouterError {
  #[error("not found in any upstream")]
  NotFound,
  #[error("all upstreams unavailable")]
  UpstreamUnavailable,
  #[error("no candidates for {0:?}")]
  NoCandidates(String),
  #[error("narinfo signature verification failed")]
  SignatureVerificationFailed,
  #[error("fetch narinfo: {0}")]
  FetchNarinfo(#[from] reqwest::Error),
  #[error("parse narinfo: {0}")]
  ParseNarinfo(#[from] NarInfoError),
  #[error(transparent)]
  Db(#[from] DbError),
}

#[derive(Debug, Clone)]
pub struct ResolveResult {
  pub url:           String,
  pub latency_ms:    f64,
  pub cache_hit:     bool,
  pub narinfo_bytes: Option<Vec<u8>>,
}

#[derive(Clone)]
pub struct Router {
  inner: Arc<RouterInner>,
}

struct RouterInner {
  db:            Db,
  prober:        Prober,
  route_ttl:     Duration,
  race_timeout:  Duration,
  negative_ttl:  Duration,
  client:        reqwest::Client,
  upstream_keys: RwLock<HashMap<String, String>>,
  inflight:      DashMap<String, Arc<Mutex<()>>>,
  lru:           MokaCache<String, Arc<ResolveResult>>,
}

#[derive(Debug)]
struct RaceResult {
  url:        String,
  latency_ms: f64,
}

/// Outcome of racing a single priority group.
#[derive(Debug)]
enum RaceGroupError {
  /// Every reachable upstream in the group returned a non-success status
  /// (i.e. the path is not present in this group).
  NotFound,
  /// Every upstream in the group hit a network-level error; the path may
  /// exist but the group is unreachable.
  NetworkError,
  /// The race deadline expired before any upstream responded.
  Timeout,
}

struct InflightGuard<'a> {
  map: &'a DashMap<String, Arc<Mutex<()>>>,
  key: String,
}

impl Drop for InflightGuard<'_> {
  fn drop(&mut self) {
    self.map.remove(&self.key);
  }
}

impl Router {
  /// Create a router backed by the database and health prober.
  ///
  /// # Errors
  ///
  /// Returns an error if the HTTP client cannot be constructed.
  pub fn new(
    db: Db,
    prober: Prober,
    route_ttl: Duration,
    race_timeout: Duration,
    negative_ttl: Duration,
  ) -> Result<Self, reqwest::Error> {
    Ok(Self {
      inner: Arc::new(RouterInner {
        db,
        prober,
        route_ttl,
        race_timeout,
        negative_ttl,
        client: reqwest::Client::builder().timeout(race_timeout).build()?,
        upstream_keys: RwLock::new(HashMap::new()),
        inflight: DashMap::new(),
        lru: MokaCache::builder()
          .max_capacity(1024)
          .time_to_live(route_ttl)
          .build(),
      }),
    })
  }

  pub async fn set_upstream_key(
    &self,
    url: String,
    public_key: String,
  ) -> Result<(), NarInfoError> {
    parse_public_key(&public_key)?;
    self
      .inner
      .upstream_keys
      .write()
      .await
      .insert(url, public_key);
    Ok(())
  }

  pub async fn resolve(
    &self,
    store_hash: &str,
    candidates: &[String],
  ) -> Result<ResolveResult, RouterError> {
    if self.inner.db.is_negative(store_hash).await? {
      return Err(RouterError::NotFound);
    }
    if let Some(result) = self.valid_cached_route(store_hash).await? {
      return Ok(result);
    }
    ncro_metrics::get().narinfo_cache_misses.inc();

    let lock = Arc::clone(
      self
        .inner
        .inflight
        .entry(store_hash.to_string())
        .or_insert_with(|| Arc::new(Mutex::new(())))
        .value(),
    );
    let _guard = lock.lock().await;
    let _cleanup = InflightGuard {
      map: &self.inner.inflight,
      key: store_hash.to_string(),
    };
    if let Some(result) = self.valid_cached_route(store_hash).await? {
      return Ok(result);
    }

    let result = self.race(store_hash, candidates).await;
    if matches!(result, Err(RouterError::NotFound)) {
      let _ = self
        .inner
        .db
        .set_negative(store_hash, self.inner.negative_ttl)
        .await;
    }
    result
  }

  async fn valid_cached_route(
    &self,
    store_hash: &str,
  ) -> Result<Option<ResolveResult>, RouterError> {
    if let Some(cached) = self.inner.lru.get(store_hash).await {
      ncro_metrics::get().narinfo_cache_hits.inc();
      return Ok(Some((*cached).clone()));
    }
    let Some(entry) = self.inner.db.get_route(store_hash).await? else {
      return Ok(None);
    };
    if !entry.is_valid() {
      return Ok(None);
    }
    let health = self.inner.prober.get_health(&entry.upstream_url).await;
    if health.as_ref().is_some_and(|h| h.status == Status::Down) {
      return Ok(None);
    }
    ncro_metrics::get().narinfo_cache_hits.inc();
    let result = ResolveResult {
      url:           entry.upstream_url,
      latency_ms:    entry.latency_ema,
      cache_hit:     true,
      narinfo_bytes: entry.narinfo_bytes,
    };
    let arc = Arc::new(result.clone());
    self.inner.lru.insert(store_hash.to_string(), arc).await;
    Ok(Some(result))
  }

  async fn race(
    &self,
    store_hash: &str,
    candidates: &[String],
  ) -> Result<ResolveResult, RouterError> {
    if candidates.is_empty() {
      return Err(RouterError::NoCandidates(store_hash.to_string()));
    }

    // Group candidates by priority. Lower number meanshigher priority, tried
    // first. Upstreams whose health entry is missing get i32::MAX so that they
    // fall into the lowest-priority group rather than being silently dropped.
    let mut groups: BTreeMap<i32, Vec<String>> = BTreeMap::new();
    for url in candidates {
      let priority = self
        .inner
        .prober
        .get_health(url)
        .await
        .map_or(i32::MAX, |h| h.priority);
      groups.entry(priority).or_default().push(url.clone());
    }

    let mut any_not_found = false;
    for (_priority, group) in groups {
      match self.race_group(store_hash, &group).await {
        Ok(winner) => return self.commit_winner(winner, store_hash).await,
        Err(RaceGroupError::NotFound) => any_not_found = true,
        // Try the next priority group on network error; those upstreams were
        // unreachable so we cannot conclude the path is absent.
        Err(RaceGroupError::NetworkError | RaceGroupError::Timeout) => {},
      }
    }

    if any_not_found {
      Err(RouterError::NotFound)
    } else {
      Err(RouterError::UpstreamUnavailable)
    }
  }

  /// Race all upstreams in `group` in parallel. Returns the first winner or
  /// a classification of the failure.
  async fn race_group(
    &self,
    store_hash: &str,
    group: &[String],
  ) -> Result<RaceResult, RaceGroupError> {
    let mut handles = FuturesUnordered::new();
    for upstream in group {
      let upstream = upstream.clone();
      let store_hash = store_hash.to_string();
      let client = self.inner.client.clone();
      handles.push(tokio::spawn(async move {
        let start = Instant::now();
        let res = client
          .head(format!("{upstream}/{store_hash}.narinfo"))
          .send()
          .await;
        match res {
          Ok(resp) if resp.status().is_success() => {
            Ok(RaceResult {
              url:        upstream,
              latency_ms: start.elapsed().as_secs_f64() * 1000.0,
            })
          },
          Ok(_) => Err(false), // 404 / non-success = not found
          Err(_) => Err(true), // network error
        }
      }));
    }

    let mut net_errs = 0usize;
    let mut not_founds = 0usize;
    let deadline = tokio::time::sleep(self.inner.race_timeout);
    tokio::pin!(deadline);

    let winner = loop {
      if handles.is_empty() {
        break None;
      }
      tokio::select! {
          () = &mut deadline => break None,
          joined = handles.next() => {
              match joined {
                  Some(Ok(Ok(res))) => break Some(res),
                  Some(Ok(Err(true)) | Err(_)) => net_errs += 1,
                  Some(Ok(Err(false))) => not_founds += 1,
                  None => break None,
              }
          }
      }
    };

    if let Some(winner) = winner {
      return Ok(winner);
    }

    // If there is no winner classify the failure so the caller can decide
    // whether to try the next priority group.
    if net_errs > 0 && not_founds == 0 {
      Err(RaceGroupError::NetworkError)
    } else if not_founds > 0 {
      Err(RaceGroupError::NotFound)
    } else {
      Err(RaceGroupError::Timeout)
    }
  }

  /// Fetch the full narinfo, then record metrics, update the prober and DB
  /// for a race winner.
  ///
  /// Metrics and side-effects are only committed once the fetch succeeds, so
  /// a failure does not inflate the win/latency counters.
  async fn commit_winner(
    &self,
    winner: RaceResult,
    store_hash: &str,
  ) -> Result<ResolveResult, RouterError> {
    let (body, nar_url, nar_hash, nar_size) =
      self.fetch_narinfo(&winner.url, store_hash).await?;

    ncro_metrics::get()
      .upstream_race_wins
      .with_label_values(&[&winner.url])
      .inc();
    ncro_metrics::get()
      .upstream_latency
      .with_label_values(&[&winner.url])
      .observe(winner.latency_ms / 1000.0);

    let ema = self
      .inner
      .prober
      .get_health(&winner.url)
      .await
      .map_or(winner.latency_ms, |h| {
        0.3f64.mul_add(winner.latency_ms, 0.7 * h.ema_latency)
      });
    self
      .inner
      .prober
      .record_latency(&winner.url, winner.latency_ms)
      .await;
    let now = Utc::now();
    self
      .inner
      .db
      .set_route(&RouteEntry {
        store_path: store_hash.to_string(),
        upstream_url: winner.url.clone(),
        latency_ms: winner.latency_ms,
        latency_ema: ema,
        last_verified: now,
        query_count: 1,
        failure_count: 0,
        ttl: now
          + chrono::Duration::from_std(self.inner.route_ttl)
            .unwrap_or_default(),
        nar_hash,
        nar_size,
        nar_url,
        narinfo_bytes: body.clone(),
      })
      .await?;
    let result = ResolveResult {
      url:           winner.url.clone(),
      latency_ms:    winner.latency_ms,
      cache_hit:     false,
      narinfo_bytes: body.clone(),
    };
    self
      .inner
      .lru
      .insert(store_hash.to_string(), Arc::new(result.clone()))
      .await;
    Ok(result)
  }

  async fn fetch_narinfo(
    &self,
    upstream: &str,
    store_hash: &str,
  ) -> Result<(Option<Vec<u8>>, String, String, u64), RouterError> {
    let resp = self
      .inner
      .client
      .get(format!("{upstream}/{store_hash}.narinfo"))
      .send()
      .await?;
    if !resp.status().is_success() {
      return Err(RouterError::NotFound);
    }
    let bytes = resp.bytes().await?;
    let body = bytes.to_vec();
    let parsed = NarInfo::parse(body.as_slice())?;
    if let Some(pubkey) = self.inner.upstream_keys.read().await.get(upstream)
      && !parsed.verify(pubkey).unwrap_or(false)
    {
      tracing::warn!(
        upstream,
        store = store_hash,
        "narinfo signature verification failed"
      );
      return Err(RouterError::SignatureVerificationFailed);
    }
    Ok((Some(body), parsed.url, parsed.nar_hash, parsed.nar_size))
  }
}

#[cfg(test)]
mod tests {
  use std::sync::Arc;

  use tokio::sync::Mutex;

  use super::InflightGuard;

  #[test]
  fn inflight_uses_dashmap() {
    use dashmap::DashMap;
    // Compile-time check that DashMap is in scope for router.
    let _: DashMap<String, std::sync::Arc<tokio::sync::Mutex<()>>> =
      DashMap::new();
  }

  #[test]
  fn inflight_guard_removes_entry_on_drop() {
    use dashmap::DashMap;
    let map: DashMap<String, Arc<Mutex<()>>> = DashMap::new();
    let key = "test_hash".to_string();
    map
      .entry(key.clone())
      .or_insert_with(|| Arc::new(Mutex::new(())));
    assert!(map.contains_key(&key));
    {
      let _guard = InflightGuard {
        map: &map,
        key: key.clone(),
      };
    }
    assert!(
      !map.contains_key(&key),
      "entry not removed after guard drop"
    );
  }
}

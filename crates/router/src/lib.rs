use std::{
  collections::{BTreeMap, HashMap},
  sync::Arc,
  time::{Duration, Instant},
};

use chrono::Utc;
use dashmap::{DashMap, mapref::entry::Entry};
use futures_util::{StreamExt, stream::FuturesUnordered};
use moka::future::Cache as MokaCache;
use ncro_db::{Db, DbError, RouteEntry};
use ncro_health::{Prober, Status};
use ncro_narinfo::{NarInfo, NarInfoError, parse_public_key};
use thiserror::Error;
use tokio::sync::{Mutex, RwLock, Semaphore};

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

#[derive(Debug, Clone)]
pub struct RouterTuning {
  pub max_concurrent_races:      u32,
  pub per_upstream_max_inflight: u32,
  pub in_memory_negative_ttl:    Duration,
  pub upstream_cooldown:         Duration,
}

struct RouterInner {
  db:                       Db,
  prober:                   Prober,
  route_ttl:                Duration,
  race_timeout:             Duration,
  negative_ttl:             Duration,
  client:                   reqwest::Client,
  upstream_keys:            RwLock<HashMap<String, String>>,
  inflight:                 DashMap<String, Arc<Mutex<()>>>,
  lru:                      MokaCache<String, Arc<ResolveResult>>,
  miss_lru:                 MokaCache<String, ()>,
  race_semaphore:           Arc<Semaphore>,
  per_upstream_limit:       u32,
  upstream_semaphores:      DashMap<String, Arc<Semaphore>>,
  upstream_cooldown:        DashMap<String, Instant>,
  upstream_cooldown_window: Duration,
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

enum RaceAttempt {
  Winner(RaceResult),
  NotFound,
  NetworkError { upstream: String },
}

struct InflightGuard<'a> {
  map: &'a DashMap<String, Arc<Mutex<()>>>,
  key: String,
  arc: Arc<Mutex<()>>,
}

impl Drop for InflightGuard<'_> {
  fn drop(&mut self) {
    self
      .map
      .remove_if(&self.key, |_, v| Arc::ptr_eq(v, &self.arc));
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
    tuning: RouterTuning,
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
        miss_lru: MokaCache::builder()
          .max_capacity(32_768)
          .time_to_live(tuning.in_memory_negative_ttl)
          .build(),
        race_semaphore: Arc::new(Semaphore::new(
          usize::try_from(tuning.max_concurrent_races).unwrap_or(64),
        )),
        per_upstream_limit: tuning.per_upstream_max_inflight,
        upstream_semaphores: DashMap::new(),
        upstream_cooldown: DashMap::new(),
        upstream_cooldown_window: tuning.upstream_cooldown,
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
    if self.inner.miss_lru.get(store_hash).await.is_some() {
      ncro_metrics::get().narinfo_memory_negative_hits.inc();
      return Err(RouterError::NotFound);
    }
    if self.inner.db.is_negative(store_hash).await? {
      return Err(RouterError::NotFound);
    }
    if let Some(result) = self.valid_cached_route(store_hash).await? {
      return Ok(result);
    }
    ncro_metrics::get().narinfo_cache_misses.inc();

    let lock = match self.inner.inflight.entry(store_hash.to_string()) {
      Entry::Occupied(entry) => {
        ncro_metrics::get().narinfo_singleflight_waiters.inc();
        Arc::clone(entry.get())
      },
      Entry::Vacant(entry) => {
        let inserted = entry.insert(Arc::new(Mutex::new(())));
        Arc::clone(&inserted)
      },
    };
    let _guard = lock.lock().await;
    let _cleanup = InflightGuard {
      map: &self.inner.inflight,
      key: store_hash.to_string(),
      arc: Arc::clone(&lock),
    };
    if let Some(result) = self.valid_cached_route(store_hash).await? {
      return Ok(result);
    }

    let result = self.race(store_hash, candidates).await;
    if matches!(result, Err(RouterError::NotFound)) {
      self.inner.miss_lru.insert(store_hash.to_string(), ()).await;
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
    let wait_start = Instant::now();
    let _race_permit = self
      .inner
      .race_semaphore
      .clone()
      .acquire_owned()
      .await
      .map_err(|_| RouterError::UpstreamUnavailable)?;
    ncro_metrics::get()
      .narinfo_race_wait_seconds
      .with_label_values(&["global"])
      .observe(wait_start.elapsed().as_secs_f64());

    let filtered = self.cooldown_filtered_candidates(candidates);
    let effective_candidates = if filtered.is_empty() {
      candidates.to_vec()
    } else {
      filtered
    };

    // Group candidates by priority. Lower number meanshigher priority, tried
    // first. Upstreams whose health entry is missing get i32::MAX so that they
    // fall into the lowest-priority group rather than being silently dropped.
    let mut groups: BTreeMap<i32, Vec<String>> = BTreeMap::new();
    for url in &effective_candidates {
      let priority = self
        .inner
        .prober
        .get_health(url)
        .await
        .map_or(i32::MAX, |h| h.priority);
      groups.entry(priority).or_default().push(url.clone());
    }

    let mut any_not_found = false;
    let mut attempts_total = 0_u64;
    for (_priority, group) in groups {
      let (group_result, attempts) = self.race_group(store_hash, &group).await;
      attempts_total += attempts;
      match group_result {
        Ok(winner) => {
          ncro_metrics::get()
            .narinfo_upstream_attempts_per_resolve
            .with_label_values(&["success"])
            .observe(attempts_total as f64);
          return self.commit_winner(winner, store_hash).await;
        },
        Err(RaceGroupError::NotFound) => any_not_found = true,
        // Try the next priority group on network error; those upstreams were
        // unreachable so we cannot conclude the path is absent.
        Err(RaceGroupError::NetworkError | RaceGroupError::Timeout) => {},
      }
    }
    ncro_metrics::get()
      .narinfo_upstream_attempts_per_resolve
      .with_label_values(&[if any_not_found {
        "not_found"
      } else {
        "unavailable"
      }])
      .observe(attempts_total as f64);

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
  ) -> (Result<RaceResult, RaceGroupError>, u64) {
    let mut handles = FuturesUnordered::new();
    for upstream in group {
      let upstream = upstream.clone();
      let store_hash = store_hash.to_string();
      let client = self.inner.client.clone();
      let gate = self.upstream_gate(&upstream);
      handles.push(tokio::spawn(async move {
        let Ok(_permit) = gate.acquire_owned().await else {
          return RaceAttempt::NetworkError { upstream };
        };
        let start = Instant::now();
        let res = client
          .head(format!("{upstream}/{store_hash}.narinfo"))
          .send()
          .await;
        match res {
          Ok(resp) if resp.status().is_success() => {
            RaceAttempt::Winner(RaceResult {
              url:        upstream,
              latency_ms: start.elapsed().as_secs_f64() * 1000.0,
            })
          },
          Ok(_) => RaceAttempt::NotFound, // 404 / non-success = not found
          Err(_) => RaceAttempt::NetworkError { upstream }, // network error
        }
      }));
    }

    let mut net_errs = 0usize;
    let mut not_founds = 0usize;
    let mut attempts = 0_u64;
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
                  Some(Ok(RaceAttempt::Winner(res))) => {
                      attempts += 1;
                      ncro_metrics::get().narinfo_upstream_attempts.inc();
                      break Some(res)
                  },
                  Some(Ok(RaceAttempt::NetworkError { upstream })) => {
                      attempts += 1;
                      ncro_metrics::get().narinfo_upstream_attempts.inc();
                      net_errs += 1;
                      self.mark_cooldown(&upstream);
                  },
                  Some(Ok(RaceAttempt::NotFound)) => {
                      attempts += 1;
                      ncro_metrics::get().narinfo_upstream_attempts.inc();
                      not_founds += 1;
                  },
                  Some(Err(_)) => {
                      attempts += 1;
                      ncro_metrics::get().narinfo_upstream_attempts.inc();
                      net_errs += 1;
                  },
                  None => break None,
              }
          }
      }
    };

    if let Some(winner) = winner {
      return (Ok(winner), attempts);
    }

    // If there is no winner classify the failure so the caller can decide
    // whether to try the next priority group.
    if net_errs > 0 && not_founds == 0 {
      (Err(RaceGroupError::NetworkError), attempts)
    } else if not_founds > 0 {
      (Err(RaceGroupError::NotFound), attempts)
    } else {
      (Err(RaceGroupError::Timeout), attempts)
    }
  }

  fn cooldown_filtered_candidates(&self, candidates: &[String]) -> Vec<String> {
    candidates
      .iter()
      .filter(|url| !self.in_cooldown(url))
      .cloned()
      .collect()
  }

  fn in_cooldown(&self, url: &str) -> bool {
    if let Some(until) = self.inner.upstream_cooldown.get(url) {
      if *until > Instant::now() {
        return true;
      }
    }
    self.inner.upstream_cooldown.remove(url);
    false
  }

  fn mark_cooldown(&self, url: &str) {
    self.inner.upstream_cooldown.insert(
      url.to_string(),
      Instant::now() + self.inner.upstream_cooldown_window,
    );
  }

  fn upstream_gate(&self, upstream: &str) -> Arc<Semaphore> {
    match self.inner.upstream_semaphores.entry(upstream.to_string()) {
      Entry::Occupied(entry) => Arc::clone(entry.get()),
      Entry::Vacant(entry) => {
        Arc::clone(&entry.insert(Arc::new(Semaphore::new(
          usize::try_from(self.inner.per_upstream_limit).unwrap_or(8),
        ))))
      },
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
    let (body, raw_nar_url, nar_hash, nar_size) =
      self.fetch_narinfo(&winner.url, store_hash).await?;
    let nar_url = raw_nar_url.trim_start_matches('/').to_string();

    ncro_metrics::get()
      .upstream_race_wins
      .with_label_values(&[&winner.url])
      .inc();
    ncro_metrics::get()
      .upstream_latency
      .with_label_values(&[&winner.url])
      .observe(winner.latency_ms / 1000.0);

    let ema = self.inner.prober.get_health(&winner.url).await.map_or(
      winner.latency_ms,
      |h| {
        self.inner.prober.alpha().mul_add(
          winner.latency_ms,
          (1.0 - self.inner.prober.alpha()) * h.ema_latency,
        )
      },
    );
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
  #![expect(clippy::unwrap_used, reason = "Fine in tests")]
  use std::{sync::Arc, time::Duration};

  use ncro_db::Db;
  use ncro_health::Prober;
  use tokio::sync::Mutex;

  use super::{InflightGuard, Router, RouterTuning};

  async fn make_router(cooldown: Duration) -> Router {
    let db = Db::open(":memory:", 100).await.unwrap();
    let prober = Prober::new(0.3).unwrap();
    Router::new(
      db,
      prober,
      Duration::from_secs(3600),
      Duration::from_secs(5),
      Duration::from_secs(600),
      RouterTuning {
        max_concurrent_races:      4,
        per_upstream_max_inflight: 2,
        in_memory_negative_ttl:    Duration::from_secs(300),
        upstream_cooldown:         cooldown,
      },
    )
    .unwrap()
  }

  #[test]
  fn inflight_guard_removes_entry_on_drop() {
    use dashmap::DashMap;
    let map: DashMap<String, Arc<Mutex<()>>> = DashMap::new();
    let key = "test_hash".to_string();
    let arc = Arc::new(Mutex::new(()));
    map.insert(key.clone(), Arc::clone(&arc));
    assert!(map.contains_key(&key));
    {
      let _guard = InflightGuard {
        map: &map,
        key: key.clone(),
        arc: Arc::clone(&arc),
      };
    }
    assert!(
      !map.contains_key(&key),
      "entry not removed after guard drop"
    );
  }

  #[tokio::test]
  async fn mark_cooldown_makes_upstream_unavailable() {
    let router = make_router(Duration::from_secs(60)).await;
    let url = "https://cache.example.com";
    assert!(!router.in_cooldown(url));
    router.mark_cooldown(url);
    assert!(router.in_cooldown(url));
  }

  #[tokio::test]
  async fn cooldown_expires_with_zero_window() {
    let router = make_router(Duration::ZERO).await;
    let url = "https://cache.example.com";
    // Deadline is Instant::now() + 0, already not in the future.
    router.mark_cooldown(url);
    assert!(!router.in_cooldown(url));
  }

  #[tokio::test]
  async fn cooldown_filter_excludes_cooled_down_upstream() {
    let router = make_router(Duration::from_secs(60)).await;
    let hot = "https://hot.example.com".to_string();
    let cold = "https://cold.example.com".to_string();
    router.mark_cooldown(&cold);
    let result = router.cooldown_filtered_candidates(&[hot.clone(), cold]);
    assert_eq!(result, vec![hot]);
  }

  #[tokio::test]
  async fn cooldown_filter_passes_all_when_none_cooled() {
    let router = make_router(Duration::from_secs(60)).await;
    let candidates = vec![
      "https://a.example.com".to_string(),
      "https://b.example.com".to_string(),
    ];
    assert_eq!(router.cooldown_filtered_candidates(&candidates), candidates);
  }

  #[tokio::test]
  async fn upstream_gate_is_stable_per_key() {
    let router = make_router(Duration::from_secs(60)).await;
    let url = "https://cache.example.com";
    let gate1 = router.upstream_gate(url);
    let gate2 = router.upstream_gate(url);
    assert!(Arc::ptr_eq(&gate1, &gate2));
  }

  #[tokio::test]
  async fn upstream_gate_is_distinct_per_upstream() {
    let router = make_router(Duration::from_secs(60)).await;
    let gate_a = router.upstream_gate("https://a.example.com");
    let gate_b = router.upstream_gate("https://b.example.com");
    assert!(!Arc::ptr_eq(&gate_a, &gate_b));
  }

  #[tokio::test]
  async fn upstream_gate_semaphore_capacity_matches_tuning() {
    let router = make_router(Duration::from_secs(60)).await;
    let gate = router.upstream_gate("https://cache.example.com");
    assert_eq!(gate.available_permits(), 2);
  }
}

use std::{
  cmp::Ordering,
  collections::HashMap,
  sync::Arc,
  time::{Duration, Instant},
};

/// Compute the probe interval for an upstream based on how many consecutive
/// failures it has accumulated. Healthy upstreams probe at the base interval;
/// degraded and down ones back off to reduce noise on dead hosts.
///
/// | State    | consecutive_fails | multiplier |
/// |----------|-------------------|------------|
/// | Active   | 0–2               | x1 (base)  |
/// | Degraded | 3–9               | x4         |
/// | Down     | 10+               | x10        |
#[must_use]
fn backoff_interval(base: Duration, consecutive_fails: u32) -> Duration {
  let multiplier = match consecutive_fails {
    0..=2 => 1,
    3..=9 => 4,
    10.. => 10,
  };
  base.saturating_mul(multiplier)
}

use ncro_config::UpstreamConfig;
use tokio::sync::RwLock;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
  Active,
  Degraded,
  Down,
}

impl Status {
  #[must_use]
  pub const fn as_str(self) -> &'static str {
    match self {
      Self::Active => "ACTIVE",
      Self::Degraded => "DEGRADED",
      Self::Down => "DOWN",
    }
  }
}

#[derive(Debug, Clone)]
pub struct UpstreamHealth {
  pub url:               String,
  pub priority:          i32,
  pub ema_latency:       f64,
  pub last_probe:        Option<Instant>,
  pub consecutive_fails: u32,
  pub total_queries:     u64,
  pub status:            Status,
}

impl UpstreamHealth {
  const fn new(url: String, priority: i32) -> Self {
    Self {
      url,
      priority,
      ema_latency: 0.0,
      last_probe: None,
      consecutive_fails: 0,
      total_queries: 0,
      status: Status::Active,
    }
  }
}

type PersistHealth = Arc<dyn Fn(String, f64, u32, u64) + Send + Sync>;

#[derive(Clone)]
pub struct Prober {
  inner: Arc<ProberInner>,
}

struct ProberInner {
  alpha:          f64,
  table:          RwLock<HashMap<String, UpstreamHealth>>,
  client:         reqwest::Client,
  persist_health: RwLock<Option<PersistHealth>>,
}

impl Prober {
  /// Create a prober with the given exponential moving average alpha.
  ///
  /// # Errors
  ///
  /// Returns an error if the HTTP client cannot be constructed.
  pub fn new(alpha: f64) -> Result<Self, reqwest::Error> {
    Ok(Self {
      inner: Arc::new(ProberInner {
        alpha,
        table: RwLock::new(HashMap::new()),
        client: reqwest::Client::builder()
          .timeout(Duration::from_secs(10))
          .build()?,
        persist_health: RwLock::new(None),
      }),
    })
  }

  pub async fn init_upstreams(&self, upstreams: &[UpstreamConfig]) {
    let mut table = self.inner.table.write().await;
    for upstream in upstreams {
      table.entry(upstream.url.clone()).or_insert_with(|| {
        UpstreamHealth::new(upstream.url.clone(), upstream.priority)
      });
    }
  }

  #[allow(clippy::significant_drop_tightening)]
  pub async fn seed(
    &self,
    url: &str,
    ema_latency: f64,
    consecutive_fails: i64,
    total_queries: i64,
  ) {
    {
      let mut table = self.inner.table.write().await;
      let Some(health) = table.get_mut(url) else {
        return;
      };
      health.ema_latency = ema_latency;
      health.total_queries =
        u64::try_from(total_queries.max(0)).unwrap_or_default();
      health.consecutive_fails =
        u32::try_from(consecutive_fails.max(0)).unwrap_or(u32::MAX);
      health.status = compute_status(health.consecutive_fails);
    }
  }

  pub async fn set_health_persistence<F>(&self, f: F)
  where
    F: Fn(String, f64, u32, u64) + Send + Sync + 'static,
  {
    *self.inner.persist_health.write().await = Some(Arc::new(f));
  }

  #[allow(clippy::significant_drop_tightening)]
  pub async fn record_latency(&self, url: &str, ms: f64) {
    let snapshot = {
      let mut table = self.inner.table.write().await;
      let Some(health) = table.get_mut(url) else {
        return;
      };
      if health.total_queries == 0 {
        health.ema_latency = ms;
      } else {
        health.ema_latency = self
          .inner
          .alpha
          .mul_add(ms, (1.0 - self.inner.alpha) * health.ema_latency);
      }
      health.consecutive_fails = 0;
      health.total_queries += 1;
      health.status = Status::Active;
      health.last_probe = Some(Instant::now());
      (
        health.url.clone(),
        health.ema_latency,
        health.consecutive_fails,
        health.total_queries,
      )
    };
    let callback = self.inner.persist_health.read().await.clone();
    if let Some(callback) = callback {
      callback(snapshot.0, snapshot.1, snapshot.2, snapshot.3);
    }
  }

  #[allow(clippy::significant_drop_tightening)]
  pub async fn record_failure(&self, url: &str) {
    let snapshot = {
      let mut table = self.inner.table.write().await;
      let Some(health) = table.get_mut(url) else {
        return;
      };
      health.consecutive_fails += 1;
      health.status = compute_status(health.consecutive_fails);
      (
        health.url.clone(),
        health.ema_latency,
        health.consecutive_fails,
        health.total_queries,
      )
    };
    let callback = self.inner.persist_health.read().await.clone();
    if let Some(callback) = callback {
      callback(snapshot.0, snapshot.1, snapshot.2, snapshot.3);
    }
  }

  pub async fn get_health(&self, url: &str) -> Option<UpstreamHealth> {
    self.inner.table.read().await.get(url).cloned()
  }

  pub async fn sorted_by_latency(&self) -> Vec<UpstreamHealth> {
    let mut result = self
      .inner
      .table
      .read()
      .await
      .values()
      .cloned()
      .collect::<Vec<_>>();
    result.sort_by(|a, b| {
      match (a.status == Status::Down, b.status == Status::Down) {
        (true, false) => return Ordering::Greater,
        (false, true) => return Ordering::Less,
        _ => {},
      }
      if b.ema_latency > 0.0
        && ((a.ema_latency - b.ema_latency).abs() / b.ema_latency) < 0.10
        && a.priority != b.priority
      {
        return a.priority.cmp(&b.priority);
      }
      match (a.ema_latency == 0.0, b.ema_latency == 0.0) {
        (true, false) => return Ordering::Greater,
        (false, true) => return Ordering::Less,
        _ => {},
      }
      a.ema_latency
        .partial_cmp(&b.ema_latency)
        .unwrap_or(Ordering::Equal)
    });
    result
  }

  pub async fn probe_upstream(&self, url: String) {
    if !self.inner.table.read().await.contains_key(&url) {
      return;
    }
    let start = Instant::now();
    let ok = self
      .inner
      .client
      .head(format!("{url}/nix-cache-info"))
      .send()
      .await
      .map(|resp| resp.status().as_u16() == 200)
      .unwrap_or(false);
    if ok {
      self
        .record_latency(&url, start.elapsed().as_secs_f64() * 1000.0)
        .await;
    } else {
      self.record_failure(&url).await;
    }
  }

  pub async fn run_probe_loop(
    &self,
    interval: Duration,
    mut stop: tokio::sync::watch::Receiver<bool>,
  ) {
    // Check for due probes at a fraction of the base interval so newly-added
    // upstreams and backoff expirations are picked up promptly.
    let check_tick = (interval / 4).max(Duration::from_secs(1));
    let mut ticker = tokio::time::interval(check_tick);

    // When was it last probed. None means never, probe immediately.
    let mut last_probed: HashMap<String, Instant> = HashMap::new();
    loop {
      tokio::select! {
          _ = stop.changed() => return,
          _ = ticker.tick() => {
              let now = Instant::now();

              // Snapshot url + consecutive_fails
              let entries: Vec<(String, u32)> = self
                  .inner
                  .table
                  .read()
                  .await
                  .values()
                  .map(|h| (h.url.clone(), h.consecutive_fails))
                  .collect();
              for (url, consecutive_fails) in entries {
                  let due = backoff_interval(interval, consecutive_fails);
                  let should_probe = match last_probed.get(&url) {
                      None => true,
                      Some(&last) => now.saturating_duration_since(last) >= due,
                  };
                  if should_probe {
                      last_probed.insert(url.clone(), now);
                      let prober = self.clone();
                      tokio::spawn(async move { prober.probe_upstream(url).await; });
                  }
              }
          }
      }
    }
  }

  pub async fn add_upstream(&self, url: String, priority: i32) {
    let inserted = self
      .inner
      .table
      .write()
      .await
      .insert(url.clone(), UpstreamHealth::new(url.clone(), priority))
      .is_none();
    if inserted {
      let prober = self.clone();
      tokio::spawn(async move {
        prober.probe_upstream(url).await;
      });
    }
  }

  pub async fn remove_upstream(&self, url: &str) {
    self.inner.table.write().await.remove(url);
  }
}

const fn compute_status(consecutive_fails: u32) -> Status {
  match consecutive_fails {
    10.. => Status::Down,
    3.. => Status::Degraded,
    _ => Status::Active,
  }
}

#[cfg(test)]
mod tests {
  use super::*;

  #[tokio::test]
  async fn ema_and_status_progression() -> Result<(), Box<dyn std::error::Error>>
  {
    let p = Prober::new(0.3)?;
    p.add_upstream("https://example.com".into(), 1).await;
    p.record_latency("https://example.com", 100.0).await;
    p.record_latency("https://example.com", 50.0).await;
    let h = p
      .get_health("https://example.com")
      .await
      .ok_or("missing health")?;
    assert!((84.0..=86.0).contains(&h.ema_latency));
    for _ in 0..10 {
      p.record_failure("https://example.com").await;
    }
    assert_eq!(
      p.get_health("https://example.com")
        .await
        .ok_or("missing health")?
        .status,
      Status::Down
    );
    Ok(())
  }
}

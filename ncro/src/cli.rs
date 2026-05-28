use clap::Parser;
use ncro_config::Config;
use ncro_db::Db;
use ncro_discovery::Discovery;
use ncro_health::Prober;
use ncro_router::{Router, RouterTuning};
use tokio::net::TcpListener;
use tracing_subscriber::{EnvFilter, fmt};

#[derive(Debug, Parser)]
#[command(name = "ncro", version, about = "Nix Cache Route Optimizer")]
pub struct Args {
  #[arg(short, long, env = "NCRO_CONFIG")]
  pub config: Option<String>,
}

pub async fn run() -> anyhow::Result<()> {
  let args = Args::parse();
  let cfg = Config::load(args.config.as_deref())?;
  cfg.validate()?;

  init_logging(&cfg.logging.level, &cfg.logging.format);
  let _ = ncro_metrics::get();

  let db = Db::open(&cfg.cache.db_path, cfg.cache.max_entries).await?;
  let prober = Prober::new(cfg.cache.latency_alpha)?;
  prober.init_upstreams(&cfg.upstreams).await;
  for row in db.load_all_health().await.unwrap_or_default() {
    prober
      .seed(
        &row.url,
        row.ema_latency,
        row.consecutive_fails,
        row.total_queries,
      )
      .await;
  }
  let db_for_health = db.clone();
  prober
    .set_health_persistence(move |url, ema, fails, queries| {
      let db = db_for_health.clone();
      tokio::spawn(async move {
        let _ = db
          .save_health(
            &url,
            ema,
            i64::from(fails),
            i64::try_from(queries).unwrap_or(i64::MAX),
          )
          .await;
      });
    })
    .await;
  for upstream in &cfg.upstreams {
    let prober = prober.clone();
    let url = upstream.url.clone();
    tokio::spawn(async move {
      prober.probe_upstream(url).await;
    });
  }

  let router = Router::new(
    db.clone(),
    prober.clone(),
    cfg.cache.ttl.0,
    std::time::Duration::from_secs(5),
    cfg.cache.negative_ttl.0,
    RouterTuning {
      max_concurrent_races:      cfg.cache.mass_query.max_concurrent_races,
      per_upstream_max_inflight: cfg.cache.mass_query.per_upstream_max_inflight,
      in_memory_negative_ttl:    cfg.cache.mass_query.in_memory_negative_ttl.0,
      upstream_cooldown:         cfg.cache.mass_query.upstream_cooldown.0,
    },
  )?;

  for upstream in &cfg.upstreams {
    if let Some(s3) = &upstream.s3 {
      router.register_s3_upstream(upstream.url.clone(), s3.clone());
    }
    if !upstream.public_key.is_empty() {
      router
        .set_upstream_key(upstream.url.clone(), upstream.public_key.clone())
        .await?;
    }
    if !upstream.username.is_empty() {
      router
        .set_upstream_auth(
          upstream.url.clone(),
          upstream.username.clone(),
          upstream.password.clone(),
        )
        .await;
    }
  }

  let (stop_tx, stop_rx) = tokio::sync::watch::channel(false);
  let probe_prober = prober.clone();
  let probe_stop = stop_rx.clone();
  tokio::spawn(async move {
    probe_prober
      .run_probe_loop(std::time::Duration::from_secs(30), probe_stop)
      .await;
  });

  let db_for_expiry = db.clone();
  let mut expiry_stop = stop_rx.clone();
  tokio::spawn(async move {
    let mut ticker = tokio::time::interval(std::time::Duration::from_secs(300));
    loop {
      tokio::select! {
          _ = expiry_stop.changed() => return,
          _ = ticker.tick() => {
              let _ = db_for_expiry.expire_old_routes().await;
              let _ = db_for_expiry.expire_negatives().await;
              if let Ok(count) = db_for_expiry.route_count().await { ncro_metrics::get().route_entries.set(count); }
          }
      }
    }
  });

  if cfg.discovery.enabled {
    let discovery = Discovery::new(cfg.discovery.clone(), prober.clone())?;
    let discovery_stop = stop_rx.clone();
    tokio::spawn(async move {
      let _ = discovery.run(discovery_stop).await;
    });
  }

  if cfg.mesh.enabled {
    let node = ncro_mesh::Node::new(&cfg.mesh.private_key_path).await?;
    tracing::info!(
      node_id = node.id(),
      public_key = hex::encode(node.public_key()),
      "mesh node identity"
    );
    let allowed = cfg
      .mesh
      .peers
      .iter()
      .filter_map(|p| hex::decode(&p.public_key).ok()?.try_into().ok())
      .collect::<Vec<[u8; 32]>>();
    ncro_mesh::listen_and_serve(
      &cfg.mesh.bind_addr,
      db.clone(),
      allowed,
      stop_rx.clone(),
    )
    .await?;
    let peers = cfg
      .mesh
      .peers
      .iter()
      .map(|p| p.addr.clone())
      .collect::<Vec<_>>();
    tokio::spawn(ncro_mesh::run_gossip_loop(
      node,
      db.clone(),
      peers,
      cfg.mesh.gossip_interval.0,
      stop_rx.clone(),
    ));
  }

  let app = ncro_server::app(
    router,
    prober,
    db,
    cfg.upstreams.clone(),
    cfg.server.cache_priority,
    cfg.server.read_timeout.0,
    cfg.server.write_timeout.0,
  )?;
  let listener =
    TcpListener::bind(normalize_listen(&cfg.server.listen)).await?;
  tracing::info!(
    addr = cfg.server.listen,
    upstreams = cfg.upstreams.len(),
    version = env!("CARGO_PKG_VERSION"),
    "ncro listening"
  );
  let server = axum::serve(listener, app).with_graceful_shutdown(async move {
    let _ = tokio::signal::ctrl_c().await;
  });
  let result = server.await;
  let _ = stop_tx.send(true);
  result?;
  Ok(())
}

fn init_logging(level: &str, format_name: &str) {
  let filter =
    EnvFilter::try_new(level).unwrap_or_else(|_| EnvFilter::new("info"));
  if format_name == "text" {
    fmt().with_env_filter(filter).init();
  } else {
    fmt().json().with_env_filter(filter).init();
  }
}

fn normalize_listen(listen: &str) -> String {
  if listen.starts_with(':') {
    format!("0.0.0.0{listen}")
  } else {
    listen.to_string()
  }
}

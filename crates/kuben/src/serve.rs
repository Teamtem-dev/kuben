//! Process composition: runtime, shared state, supervised subsystems, ordered
//! shutdown (Invariant I-15).

use std::{future::Future, net::SocketAddr, sync::Arc, time::Duration};

use anyhow::Context as _;

use kuben_core::{
    config::{Config, Role, RuntimeCfg},
    traits::StaticPolicy,
};
use kuben_platform::{
    health::Health,
    leader::{self, Election},
    projection::Projections,
    registry::{ClusterRegistry, own_namespace},
    supervise::supervise,
};
use tokio_util::sync::CancellationToken;

use crate::cli::ServeOpts;

pub fn build_runtime(rt: &RuntimeCfg) -> anyhow::Result<tokio::runtime::Runtime> {
    let workers = rt
        .worker_threads
        .unwrap_or_else(|| std::thread::available_parallelism().map_or(2, |n| n.get().clamp(2, 4)));
    Ok(tokio::runtime::Builder::new_multi_thread()
        .worker_threads(workers)
        .max_blocking_threads(rt.max_blocking_threads.max(4))
        .thread_name("kuben")
        .enable_all()
        .build()?)
}

pub fn block_on<F: Future<Output = anyhow::Result<()>>>(rt: &RuntimeCfg, f: F) -> anyhow::Result<()> {
    build_runtime(rt)?.block_on(f)
}

pub fn run(mut cfg: Config, opts: &ServeOpts) -> anyhow::Result<()> {
    if opts.dev && cfg.database.url == kuben_core::config::DatabaseCfg::default().url {
        cfg.database.url = "sqlite://./.dev/kuben.db".into();
        std::fs::create_dir_all("./.dev").ok();
    }
    // ADR-013: one runtime in phase 0; `runtime.bulkhead` reserves the second.
    let rt = build_runtime(&cfg.runtime)?;
    rt.block_on(serve(cfg))
}

async fn serve(cfg: Config) -> anyhow::Result<()> {
    let shutdown = CancellationToken::new();
    tokio::spawn(signals(shutdown.clone()));
    crate::telemetry::install_metrics(&cfg);

    // ---- shared state ----
    let health = Health::new();
    tokio::spawn(watchdog(health.clone(), shutdown.child_token()));

    let store = kuben_store::Store::connect(&cfg.database).await?;
    tracing::info!(backend = store.backend(), "database ready");
    let cluster = ClusterRegistry::from_config(&cfg.kube).await?;
    let election = election(&cfg)?;

    let hasher = Arc::new(kuben_api::auth::password::Hasher::from_config(&cfg.security));
    if let Some(password) = crate::bootstrap::ensure_admin(&cfg, &store, &hasher).await? {
        crate::bootstrap::hand_over_password(&cfg, cluster.as_ref(), &password).await;
    }

    let projections = Arc::new(Projections::new());
    let tasks = if let Some(registry) = &cluster {
        health.ok("cluster");
        spawn_cluster_tasks(
            &cfg,
            registry,
            &projections,
            &health,
            election.as_ref(),
            &shutdown,
        )
    } else {
        health.degraded("cluster", "no kubernetes cluster configured");
        // Nothing to sync: serve setup and diagnostics right away.
        health.set_ready(true);
        Vec::new()
    };

    #[cfg(not(feature = "activator"))]
    if cfg.server.roles.contains(&Role::Activator) {
        tracing::warn!(
            "the activator role is not part of this build (scale-to-zero lands in phase 2); ignoring it"
        );
    }

    if cfg.has_role(Role::Api) {
        let state = kuben_api::ApiState::new(
            cfg.clone(),
            store.clone(),
            cluster.clone(),
            projections.clone(),
            health.clone(),
            Arc::new(StaticPolicy),
        );
        let app = kuben_api::router(state);
        let listener = tokio::net::TcpListener::bind(&cfg.server.bind)
            .await
            .with_context(|| format!("cannot listen on {}", cfg.server.bind))?;
        tracing::info!(bind = %cfg.server.bind, roles = ?cfg.server.roles, version = crate::cli::VERSION, "kuben listening");
        let t = shutdown.clone();
        axum::serve(listener, app.into_make_service_with_connect_info::<SocketAddr>())
            .with_graceful_shutdown(async move { t.cancelled().await })
            .await?;
    } else {
        shutdown.cancelled().await;
    }

    // ---- ordered shutdown ----
    tracing::info!("shutting down");
    health.set_ready(false);
    tokio::time::sleep(Duration::from_secs(1)).await; // let load balancers observe readyz=503
    shutdown.cancel();
    for t in tasks {
        let _ = tokio::time::timeout(Duration::from_secs(10), t).await;
    }
    store.checkpoint_and_close().await?;
    tracing::info!("bye");
    Ok(())
}

/// Cluster-backed subsystems: informers, the readiness gate on their first
/// sync, controllers (behind leader election when enabled) and, with the
/// `activator` feature, the activator.
fn spawn_cluster_tasks(
    cfg: &Config,
    registry: &ClusterRegistry,
    projections: &Arc<Projections>,
    health: &Health,
    election: Option<&Election>,
    shutdown: &CancellationToken,
) -> Vec<tokio::task::JoinHandle<()>> {
    let mut tasks = Vec::new();
    let (r, p, h, t) = (
        registry.clone(),
        projections.clone(),
        health.clone(),
        shutdown.child_token(),
    );
    tasks.push(tokio::spawn(supervise("informers", t, h, move |tok| {
        kuben_platform::projection::informer::run(r.clone(), p.clone(), tok)
    })));

    // Readiness waits for every informer's first LIST: until then the
    // projections are incomplete and lookups would answer 404 for
    // objects that exist.
    let (p, h, t) = (projections.clone(), health.clone(), shutdown.child_token());
    tokio::spawn(async move {
        tokio::select! {
            () = p.wait_synced() => {
                tracing::info!("informers synced; ready");
                h.set_ready(true);
            }
            () = t.cancelled() => {}
        }
    });

    if cfg.has_role(Role::Controller) {
        let (r, p, h, t) = (
            registry.clone(),
            projections.clone(),
            health.clone(),
            shutdown.child_token(),
        );
        let election = election.cloned();
        tasks.push(tokio::spawn(supervise("controllers", t, h.clone(), move |tok| {
            let (r, p, h, election) = (r.clone(), p.clone(), h.clone(), election.clone());
            async move {
                match election {
                    // Several replicas: reconcile only while holding the Lease.
                    Some(election) => {
                        let client = r.primary();
                        leader::run_as_leader(client, &election, h.clone(), tok, move |tok| {
                            kuben_platform::controller::run_all(r, p, h, tok)
                        })
                        .await
                    }
                    None => kuben_platform::controller::run_all(r, p, h, tok).await,
                }
            }
        })));
    }

    #[cfg(feature = "activator")]
    if cfg.has_role(Role::Activator) {
        let (r, p, h, t) = (
            registry.clone(),
            projections.clone(),
            health.clone(),
            shutdown.child_token(),
        );
        let bind = cfg.server.activator_bind.clone();
        tasks.push(tokio::spawn(supervise("activator", t, h.clone(), move |tok| {
            kuben_platform::activator::run(r.clone(), p.clone(), h.clone(), tok, bind.clone())
        })));
    }
    tasks
}

/// Leader-election settings, or `None` when `kube.leader_election` is off.
fn election(cfg: &Config) -> anyhow::Result<Option<Election>> {
    if !cfg.kube.leader_election {
        return Ok(None);
    }
    let namespace = own_namespace(cfg.kube.namespace.as_deref()).context(
        "kube.leader_election needs a namespace for its Lease: set KUBEN_KUBE__NAMESPACE (automatic inside a pod)",
    )?;
    let host = std::env::var("HOSTNAME")
        .ok()
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "kuben".into());
    let suffix: u32 = rand::random();
    Ok(Some(Election {
        namespace,
        identity: format!("{host}_{suffix:08x}"),
    }))
}

/// Heartbeat for `/livez`: if the runtime is wedged this stops ticking.
async fn watchdog(health: Health, token: CancellationToken) {
    let mut tick = tokio::time::interval(Duration::from_secs(1));
    loop {
        tokio::select! {
            _ = tick.tick() => health.heartbeat(),
            () = token.cancelled() => return,
        }
    }
}

async fn signals(token: CancellationToken) {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut term = match signal(SignalKind::terminate()) {
            Ok(s) => s,
            Err(e) => {
                tracing::error!(error = %e, "failed to install SIGTERM handler");
                let _ = tokio::signal::ctrl_c().await;
                token.cancel();
                return;
            }
        };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = term.recv() => {},
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
    tracing::info!("shutdown signal received");
    token.cancel();
}

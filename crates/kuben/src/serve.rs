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
    let mut tasks = Vec::new();

    match &cluster {
        Some(registry) => {
            health.ok("cluster");
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
                let election = election.clone();
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

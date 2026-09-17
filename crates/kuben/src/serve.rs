//! Process composition: runtime, shared state, supervised subsystems, ordered
//! shutdown (Invariant I-15).

use std::{future::Future, net::SocketAddr, sync::Arc, time::Duration};

use anyhow::Context as _;

use kuben_core::{
    config::{Config, Role, RuntimeCfg},
    traits::StaticPolicy,
};
use kuben_platform::{
    discovery::{self, Facts},
    health::Health,
    leader::{self, Election},
    local_agent,
    projection::Projections,
    registry::{ClusterRegistry, own_namespace, redact_credentials},
    supervise::supervise,
};
use tokio_util::sync::CancellationToken;

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

pub fn run(cfg: Config) -> anyhow::Result<()> {
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

    let store = kuben_store::Store::connect_unmigrated(&cfg.database).await?;
    crate::cli::upgrade::migrate(&cfg, &store).await?;
    database_ready(&cfg, &store, &health, &shutdown).await;
    let cluster = ClusterRegistry::from_config(&cfg.kube).await?;
    let election = election(&cfg)?;

    let hasher = Arc::new(kuben_api::auth::password::Hasher::from_config(&cfg.security));
    if cfg.setup_wizard() {
        // The first admin is created from the console; nothing is seeded.
        if store.count_users().await? == 0 {
            crate::bootstrap::announce_setup(&cfg);
        }
    } else if let Some(password) = crate::bootstrap::ensure_admin(&cfg, &store, &hasher).await? {
        crate::bootstrap::hand_over_password(&cfg, cluster.as_ref(), &password).await;
    }

    // AgentLink: the hub's endpoint for cluster agents (ADR-027). It needs no
    // kubeconfig of its own; the materializer hands it envelopes.
    let agent_link = agent_link(&cfg, &store, cluster.as_ref()).await?;
    let github = github_app(&cfg)?;
    let sso = sso_client(&cfg)?;
    let keyring = secret_keyring(&cfg, &store).await?;

    let projections = Arc::new(Projections::new());
    let mut tasks = if let Some(registry) = &cluster {
        health.ok("cluster");
        spawn_cluster_tasks(
            &cfg,
            &store,
            registry,
            &projections,
            &health,
            election.as_ref(),
            agent_link
                .as_ref()
                .map(kuben_platform::agentlink::AgentLink::dispatch),
            &keyring,
            &shutdown,
        )
    } else {
        health.degraded("cluster", "no kubernetes cluster configured");
        // Nothing to sync: serve setup and diagnostics right away.
        health.set_ready(true);
        Vec::new()
    };

    if let (true, Some(_), Some(registry)) = (cfg.agent.local, &agent_link, &cluster) {
        tasks.push(spawn_local_agent(&cfg, &store, registry, &health, &shutdown));
    }

    tasks.extend(spawn_builds(&cfg, &store, cluster.as_ref(), github.clone(), &health, &shutdown).await?);
    tasks.push(spawn_notifier(
        &cfg,
        &store,
        (github.as_ref(), &keyring),
        &health,
        &shutdown,
    ));

    if let Some(link) = agent_link {
        let (h, t) = (health.clone(), shutdown.child_token());
        tasks.push(tokio::spawn(supervise("agentlink", t, h, move |tok| {
            let link = link.clone();
            async move { link.serve(tok).await }
        })));
    }

    #[cfg(not(feature = "activator"))]
    if cfg.server.roles.contains(&Role::Activator) {
        tracing::warn!(
            "the activator role is not part of this build (scale-to-zero lands in phase 2); ignoring it"
        );
    }

    if cfg.has_role(Role::Api) {
        let parts = (&store, cluster.as_ref(), &projections, &health);
        let app = kuben_api::router(api_state(&cfg, parts, (github.clone(), sso, keyring))?);
        let listener = tokio::net::TcpListener::bind(&cfg.server.bind)
            .await
            .with_context(|| format!("cannot listen on {}", cfg.server.bind))?;
        tracing::info!(bind = %cfg.server.bind, roles = ?cfg.server.roles, version = crate::cli::VERSION, "kuben listening");
        if let Some(warning) = cfg.insecure_cookie_warning(kuben_core::config::in_cluster()) {
            tracing::warn!("{warning}");
        }
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
    store.close().await?;
    tracing::info!("bye");
    Ok(())
}

/// The keyring of managed secrets (M4.4), checked against the keys this
/// installation has used; older revisions are resealed under its current
/// key. A keyring that is not the installation's stops the server.
async fn secret_keyring(
    cfg: &Config,
    store: &kuben_store::Store,
) -> anyhow::Result<Arc<kuben_platform::secrets::Keyring>> {
    let file = cfg.secret_keyring_file();
    let keyring = kuben_platform::secrets::Keyring::load_or_create(&file).context("secret keyring")?;
    kuben_platform::secrets::prepare(store, &keyring)
        .await
        .context("secret keyring")?;
    tracing::info!(file = %file.display(), key_version = keyring.current(), "secret keyring ready");
    Ok(Arc::new(keyring))
}

/// The GitHub App of Git sources (M3), when configured. A configured App
/// whose key cannot be read stops the server instead of silently building
/// nothing.
fn github_app(cfg: &Config) -> anyhow::Result<Option<kuben_api::github::GithubApp>> {
    let git = &cfg.git;
    if git.github_app_id.is_none() && git.github_private_key_file.is_none() {
        return Ok(None);
    }
    let app = kuben_api::github::GithubApp::from_config(git).context("Git sources")?;
    tracing::info!(api = %git.github_api_url, "GitHub App ready for Git sources");
    Ok(Some(app))
}

/// The API state, with the Git, CI and single sign-on integrations the
/// configuration enables and the secret keyring. A quota that is not one
/// stops the server instead of admitting everything.
fn api_state(
    cfg: &Config,
    (store, cluster, projections, health): (
        &kuben_store::Store,
        Option<&ClusterRegistry>,
        &Arc<Projections>,
        &Health,
    ),
    (github, sso, keyring): (
        Option<kuben_api::github::GithubApp>,
        Option<Arc<kuben_api::sso::SsoClient>>,
        Arc<kuben_platform::secrets::Keyring>,
    ),
) -> anyhow::Result<kuben_api::ApiState> {
    cfg.quota.org_limits().map_err(anyhow::Error::msg)?;
    let mut state = kuben_api::ApiState::new(
        cfg.clone(),
        store.clone(),
        cluster.cloned(),
        projections.clone(),
        health.clone(),
        Arc::new(StaticPolicy),
    )
    .with_github(github)
    .with_github_oidc(github_oidc(cfg))
    .with_keyring(keyring);
    state.sso = sso;
    Ok(state)
}

/// Single sign-on (M4.3), when enabled. A broken configuration stops the
/// server instead of silently offering password sign-in only.
fn sso_client(cfg: &Config) -> anyhow::Result<Option<Arc<kuben_api::sso::SsoClient>>> {
    if !cfg.sso.enabled {
        return Ok(None);
    }
    let client = kuben_api::sso::SsoClient::from_config(
        &cfg.sso,
        cfg.server.public_url.as_deref(),
        &cfg.bootstrap.org_slug,
    )
    .context("single sign-on")?;
    tracing::info!(issuer = %client.issuer(), org = %client.org_slug(), "single sign-on enabled");
    Ok(Some(Arc::new(client)))
}

/// The GitHub Actions OIDC verifier (M4.2), when CI trust is on and its
/// audience is known.
fn github_oidc(cfg: &Config) -> Option<kuben_api::oidc::GithubOidc> {
    let Some(audience) = cfg.github_oidc_audience() else {
        if cfg.ci.github_actions {
            tracing::warn!(
                "ci.github_actions is set without an audience (ci.github_oidc_audience or server.public_url): CI tokens are refused"
            );
        }
        return None;
    };
    tracing::info!(issuer = %cfg.ci.github_oidc_issuer, %audience, "GitHub Actions OIDC exchange enabled");
    Some(kuben_api::oidc::GithubOidc::new(
        &cfg.ci.github_oidc_issuer,
        &audience,
    ))
}

/// Namespace of build Jobs unless `build.namespace` names one: never Kuben's
/// own, whose Secrets build pods must not share.
const BUILD_NAMESPACE: &str = "kuben-builds";

/// The build worker (M3, ADR-028), when builds are enabled and the GitHub App
/// is configured. Its namespace is created when missing; rootless BuildKit
/// needs the `privileged` Pod Security level there (for its own seccomp and
/// AppArmor profile, never a privileged container).
async fn spawn_builds(
    cfg: &Config,
    store: &kuben_store::Store,
    cluster: Option<&ClusterRegistry>,
    github: Option<kuben_api::github::GithubApp>,
    health: &Health,
    shutdown: &CancellationToken,
) -> anyhow::Result<Vec<tokio::task::JoinHandle<()>>> {
    let build = &cfg.build;
    let (true, true, Some(registry)) = (build.enabled, cfg.has_role(Role::Controller), cluster) else {
        return Ok(Vec::new());
    };
    let Some(github) = github else {
        tracing::warn!(
            "build.enabled is set, but Git sources are not configured (git.github_app_id): no builds run"
        );
        return Ok(Vec::new());
    };
    for image in build.unpinned_images() {
        tracing::warn!(%image, "a build image is not pinned by digest; pin it as image@sha256:… in production");
    }
    let namespace = build
        .namespace
        .clone()
        .filter(|n| !n.is_empty())
        .unwrap_or_else(|| BUILD_NAMESPACE.to_owned());
    ensure_build_namespace(registry.primary(), &namespace)
        .await
        .with_context(|| format!("preparing the build namespace {namespace}"))?;
    let credentials = match build.registry_auth_file.as_deref().filter(|f| !f.is_empty()) {
        Some(file) => Some(
            std::fs::read_to_string(file)
                .with_context(|| format!("reading build.registry_auth_file {file}"))?,
        ),
        None => None,
    };
    let verifier = kuben_api::oci::RegistryVerifier::new(build.insecure_registry, credentials.as_deref());
    let settings = kuben_platform::build::job::BuildSettings::from_config(build, namespace.clone());
    let limits = kuben_store::repo::SlotLimits {
        total: build.max_concurrent,
        per_org: build.max_concurrent_per_org,
    };
    let mut tasks = Vec::new();
    if settings.scanner_image.is_some() {
        let rescanner = kuben_platform::build::rescan::Rescanner::new(
            store.clone(),
            registry.primary(),
            settings.clone(),
            Duration::from_secs(u64::from(build.rescan_hours.max(1)) * 3600),
        );
        let (h, t) = (health.clone(), shutdown.child_token());
        tasks.push(tokio::spawn(supervise("rescans", t, h.clone(), move |tok| {
            kuben_platform::build::rescan::run(rescanner.clone(), h.clone(), tok)
        })));
    } else {
        tracing::warn!("build.scanner_image is empty: images are not scanned and scans are unavailable");
    }
    let worker = kuben_platform::build::BuildWorker::new(
        store.clone(),
        registry.primary(),
        instance_identity(),
        Arc::new(github),
        Arc::new(verifier),
        settings,
        limits,
    );
    tracing::info!(%namespace, max_concurrent = build.max_concurrent, "build worker ready");
    let (h, t) = (health.clone(), shutdown.child_token());
    tasks.push(tokio::spawn(supervise("builds", t, h.clone(), move |tok| {
        kuben_platform::build::run(worker.clone(), h.clone(), tok)
    })));
    Ok(tasks)
}

async fn ensure_build_namespace(client: kube::Client, name: &str) -> anyhow::Result<()> {
    use k8s_openapi::api::core::v1::Namespace;
    let namespaces: kube::Api<Namespace> = kube::Api::all(client);
    if namespaces.get_opt(name).await?.is_some() {
        return Ok(());
    }
    let namespace: Namespace = serde_json::from_value(serde_json::json!({
        "metadata": {
            "name": name,
            "labels": {
                "app.kubernetes.io/managed-by": "kuben",
                "pod-security.kubernetes.io/enforce": "privileged",
                "pod-security.kubernetes.io/warn": "baseline",
            },
        },
    }))?;
    match namespaces
        .create(&kube::api::PostParams::default(), &namespace)
        .await
    {
        Ok(_) => Ok(()),
        Err(kube::Error::Api(s)) if s.code == 409 => Ok(()),
        Err(e) => Err(e.into()),
    }
}

/// What runs on one replica at a time: the controllers and the materializer's
/// drift watch.
async fn leading(
    registry: ClusterRegistry,
    projections: Arc<Projections>,
    facts: Facts,
    health: Health,
    worker: kuben_platform::materializer::Worker,
    token: CancellationToken,
) -> anyhow::Result<()> {
    tokio::try_join!(
        kuben_platform::controller::run_all(registry, projections, facts, health, token.clone()),
        kuben_platform::materializer::drift::watch(worker, token),
    )?;
    Ok(())
}

/// Cluster-backed subsystems: informers, the readiness gate on their first
/// sync, controllers and the materializer's drift watch (behind leader
/// election when enabled), the materializer's worker (whose claims are
/// fenced in SQL, so every replica runs one) and, with the `activator`
/// feature, the activator.
#[allow(clippy::too_many_arguments)] // the process's shared parts, passed once at startup
fn spawn_cluster_tasks(
    cfg: &Config,
    store: &kuben_store::Store,
    registry: &ClusterRegistry,
    projections: &Arc<Projections>,
    health: &Health,
    election: Option<&Election>,
    agents: Option<Arc<dyn kuben_platform::materializer::AgentDispatch>>,
    keyring: &Arc<kuben_platform::secrets::Keyring>,
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
        // What the cluster can do (ADR-031), discovered on every controller
        // replica: rendering and the gateway are gated on it.
        let (discovery_task, facts) = spawn_discovery(registry, store, health, shutdown);
        tasks.push(discovery_task);

        // SQL is the only desired-state writer; the materializer writes its
        // resources (ADR-032).
        let mut worker =
            kuben_platform::materializer::Worker::new(store.clone(), registry.primary(), instance_identity())
                .with_facts(facts.clone())
                .with_keyring(keyring.clone());
        if let Some(agents) = agents {
            // Targets delivered by their cluster's agent go through the hub.
            worker = worker.with_agents(agents);
        }
        let (r, p, h, watcher, t) = (
            registry.clone(),
            projections.clone(),
            health.clone(),
            worker.clone(),
            shutdown.child_token(),
        );
        let election = election.cloned();
        tasks.push(tokio::spawn(supervise("controllers", t, h.clone(), move |tok| {
            let (r, p, capabilities, h, watcher, election) = (
                r.clone(),
                p.clone(),
                facts.clone(),
                h.clone(),
                watcher.clone(),
                election.clone(),
            );
            async move {
                match election {
                    // Several replicas: reconcile only while holding the Lease.
                    Some(election) => {
                        let client = r.primary();
                        leader::run_as_leader(client, &election, h.clone(), tok, move |tok| {
                            leading(r, p, capabilities, h, watcher, tok)
                        })
                        .await
                    }
                    None => Box::pin(leading(r, p, capabilities, h, watcher, tok)).await,
                }
            }
        })));

        let (h, t) = (health.clone(), shutdown.child_token());
        tasks.push(tokio::spawn(supervise(
            "materializer",
            t,
            h.clone(),
            move |tok| kuben_platform::materializer::run(worker.clone(), h.clone(), tok),
        )));
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

/// AgentLink of a controller replica (ADR-027), when `agent.bind` is set. In
/// a pod the state directory is not kept: the CA agents pin lives in a
/// Secret (M2.8).
async fn agent_link(
    cfg: &Config,
    store: &kuben_store::Store,
    cluster: Option<&ClusterRegistry>,
) -> anyhow::Result<Option<kuben_platform::agentlink::AgentLink>> {
    if !cfg.has_role(Role::Controller) {
        return Ok(None);
    }
    if let (Some(_), Some(registry), true) = (&cfg.agent.bind, cluster, kuben_core::config::in_cluster()) {
        let namespace = local_agent::namespace(&cfg.agent, cfg.kube.namespace.as_deref());
        local_agent::sync_ca(registry.primary(), &namespace, &cfg.state_dir())
            .await
            .context("keeping the AgentLink CA in its Secret")?;
    }
    kuben_platform::agentlink::AgentLink::build(&cfg.agent, &cfg.state_dir(), store.clone())
}

/// Publish the enrollment of the agent in Kuben's own cluster (M2.8).
fn spawn_local_agent(
    cfg: &Config,
    store: &kuben_store::Store,
    registry: &ClusterRegistry,
    health: &Health,
    shutdown: &CancellationToken,
) -> tokio::task::JoinHandle<()> {
    let (store, client, agent, slug, dir) = (
        store.clone(),
        registry.primary(),
        cfg.agent.clone(),
        cfg.bootstrap.org_slug.clone(),
        cfg.state_dir(),
    );
    let namespace = local_agent::namespace(&cfg.agent, cfg.kube.namespace.as_deref());
    tokio::spawn(supervise(
        "local-agent",
        shutdown.child_token(),
        health.clone(),
        move |token| {
            local_agent::run(
                store.clone(),
                client.clone(),
                agent.clone(),
                slug.clone(),
                dir.clone(),
                namespace.clone(),
                token,
            )
        },
    ))
}

/// Supervised discovery of the primary cluster's capabilities (ADR-031),
/// and the channel it publishes them on.
fn spawn_discovery(
    registry: &ClusterRegistry,
    store: &kuben_store::Store,
    health: &Health,
    shutdown: &CancellationToken,
) -> (tokio::task::JoinHandle<()>, Facts) {
    let (sender, facts) = discovery::channel();
    let sender = Arc::new(sender);
    let (registry, store) = (registry.clone(), store.clone());
    let task = tokio::spawn(supervise(
        "discovery",
        shutdown.child_token(),
        health.clone(),
        move |token| discovery::run(registry.clone(), store.clone(), sender.clone(), token),
    ));
    (task, facts)
}

/// Copy the installer's journal into SQL (M2.6), when `kuben setup` left one
/// on this host. Best effort: the host file stays the installer's record.
async fn record_install_journal(cfg: &Config, store: &kuben_store::Store) {
    use crate::cli::setup::journal::{JOURNAL, Journal};

    let Some(journal) = Journal::peek(&cfg.state_dir().join(JOURNAL)) else {
        return;
    };
    let host = std::fs::read_to_string("/proc/sys/kernel/hostname")
        .ok()
        .map(|h| h.trim().to_owned())
        .or_else(|| std::env::var("HOSTNAME").ok())
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "kuben".into());
    let recorded = match serde_json::to_value(&journal) {
        Ok(value) => store.record_install_journal(&host, &value).await,
        Err(e) => {
            tracing::warn!(error = %e, "cannot serialize the install journal");
            return;
        }
    };
    match recorded {
        Ok(true) => tracing::info!(%host, runs = journal.runs.len(), "install journal recorded"),
        Ok(false) => {}
        Err(e) => tracing::warn!(error = %e, "cannot record the install journal"),
    }
}

/// Leader-election settings, or `None` when `kube.leader_election` is off.
fn election(cfg: &Config) -> anyhow::Result<Option<Election>> {
    if !cfg.kube.leader_election {
        return Ok(None);
    }
    let namespace = own_namespace(cfg.kube.namespace.as_deref()).context(
        "kube.leader_election needs a namespace for its Lease: set KUBEN_KUBE__NAMESPACE (automatic inside a pod)",
    )?;
    Ok(Some(Election {
        namespace,
        identity: instance_identity(),
    }))
}

/// This process among the replicas: the host name plus a random suffix,
/// for the leader Lease and the materializer's claims.
fn instance_identity() -> String {
    let host = std::env::var("HOSTNAME")
        .ok()
        .filter(|h| !h.is_empty())
        .unwrap_or_else(|| "kuben".into());
    let suffix: u32 = rand::random();
    format!("{host}_{suffix:08x}")
}

/// Heartbeat for `/livez`: if the runtime is wedged this stops ticking.
/// Log what the database is, record the install journal and watch the
/// backups.
async fn database_ready(
    cfg: &Config,
    store: &kuben_store::Store,
    health: &Health,
    shutdown: &CancellationToken,
) {
    tracing::info!(backend = store.backend(), url = %redact_credentials(&cfg.database.url), "database ready");
    if let Ok(Some(role)) = store.role_bypassing_row_security().await {
        tracing::warn!(
            %role,
            "the database role is a superuser or has BYPASSRLS: row-level security does not apply; connect as an ordinary role"
        );
    }
    record_install_journal(cfg, store).await;
    tokio::spawn(watch_backups(
        cfg.clone(),
        store.clone(),
        health.clone(),
        shutdown.child_token(),
    ));
    tokio::spawn(keep_budgets(cfg.clone(), store.clone(), shutdown.child_token()));
}

/// Remove rows older than their retention budget (M4.12), hourly, and more
/// often while a pass still finds a full batch.
async fn keep_budgets(cfg: Config, store: kuben_store::Store, token: CancellationToken) {
    let mut wait = Duration::from_mins(1);
    loop {
        tokio::select! {
            () = tokio::time::sleep(wait) => {}
            () = token.cancelled() => return,
        }
        wait = match store.apply_retention_now(&cfg.retention).await {
            Ok(done) => {
                if done.total() > 0 {
                    tracing::info!(?done, "old rows removed");
                }
                if done.more {
                    Duration::from_secs(5)
                } else {
                    Duration::from_hours(1)
                }
            }
            Err(e) => {
                tracing::warn!(error = %e, "retention pass failed");
                Duration::from_mins(10)
            }
        };
    }
}

/// Report `backups` degraded while the newest good backup is older than
/// `backup.max_age_hours` (M4.7): the alert an operator acts on.
async fn watch_backups(cfg: Config, store: kuben_store::Store, health: Health, token: CancellationToken) {
    if cfg.backup.max_age_hours == 0 {
        return;
    }
    let mut tick = tokio::time::interval(Duration::from_mins(10));
    loop {
        tokio::select! {
            _ = tick.tick() => {}
            () = token.cancelled() => return,
        }
        let state = match store.last_backup().await {
            Ok(last) => crate::cli::backup::freshness(
                last.map(|b| b.finished_at),
                kuben_core::time::now_ms(),
                cfg.backup.max_age_hours,
            ),
            Err(e) => Err(format!("cannot read the backups: {e}")),
        };
        match &state {
            Ok(_) => health.ok("backups"),
            Err(problem) => health.degraded("backups", problem),
        }
        if let Err(e) = backup_incident(&cfg, &store, state.err()).await {
            tracing::warn!(error = %e, "the backup incident was not updated");
        }
    }
}

/// Open an incident of the installation's organization while backups are
/// stale (`problem`), and resolve it once they are not.
async fn backup_incident(
    cfg: &Config,
    store: &kuben_store::Store,
    problem: Option<String>,
) -> anyhow::Result<()> {
    const KEY: &str = "backup:stale";
    let Some(org) = store.installation_org(&cfg.bootstrap.org_slug).await? else {
        return Ok(());
    };
    let mut tenant = store.tenant(org).await?;
    match problem {
        Some(detail) => {
            let incident = kuben_store::repo::NewIncident {
                project: None,
                environment: None,
                target: None,
                kind: "backup.stale".into(),
                severity: "critical",
                dedupe_key: KEY.into(),
                title: "Database backups are stale".into(),
                detail: Some(detail),
            };
            tenant.open_incident(&incident).await?;
        }
        None => {
            tenant.resolve_incident_key(KEY, "system:backups").await?;
        }
    }
    tenant.commit().await?;
    Ok(())
}

/// The notifier (M4.10): incidents, webhooks and commit statuses from the
/// outbox. Every replica runs one; they share the work.
fn spawn_notifier(
    cfg: &Config,
    store: &kuben_store::Store,
    (github, keyring): (
        Option<&kuben_api::github::GithubApp>,
        &Arc<kuben_platform::secrets::Keyring>,
    ),
    health: &Health,
    shutdown: &CancellationToken,
) -> tokio::task::JoinHandle<()> {
    let notifier = kuben_api::notify::Notifier::new(
        store.clone(),
        keyring.clone(),
        github.cloned().map(Arc::new),
        cfg.notify.clone(),
        cfg.server.public_url.clone(),
    );
    let (h, t) = (health.clone(), shutdown.child_token());
    tokio::spawn(supervise("notifications", t, h.clone(), move |tok| {
        kuben_api::notify::run(notifier.clone(), h.clone(), tok)
    }))
}

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

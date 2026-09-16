//! `kuben-agent` — the Kuben cluster agent (ADR-027). Runs inside the
//! cluster, dials the hub over outbound mTLS and keeps that link up.
//!
//! On the first start it makes its device key, reads a bootstrap token
//! (`--token-file` or `--token-stdin`; never an argument, so it stays out of
//! process lists and shell history) and enrolls. Later starts use the stored
//! certificate, which the agent renews over the link before it expires.

use std::{path::PathBuf, sync::Arc, time::Duration};

use clap::Parser;
use kuben_agent::{
    bootstrap::{Enrollment, IdentitySecret, own_namespace},
    link::{Credentials, Lifetime, LinkConfig, Renewal, TcpConnector, run},
    protocol::APPLICATION_RUNTIME,
    runtime::KubeExecutor,
    state::{HubAddress, State, StateError, TokenSource, ensure_identity, pinned_ca, read_token},
    tls::{HUB_NAME, server_name},
};
use time::OffsetDateTime;
use tokio_util::sync::CancellationToken;
use tracing_subscriber::{EnvFilter, fmt, prelude::*};

#[derive(Debug, Parser)]
#[command(
    name = "kuben-agent",
    version,
    about = "The Kuben cluster agent: links this cluster to its hub over outbound mTLS"
)]
struct Args {
    /// The hub's AgentLink address, `host:port`.
    #[arg(long, env = "KUBEN_AGENT_HUB", required_unless_present = "enrollment_dir")]
    hub: Option<String>,
    /// The name the hub's certificate carries.
    #[arg(long, env = "KUBEN_AGENT_HUB_NAME", default_value = HUB_NAME)]
    hub_name: String,
    /// The hub CA to pin, in PEM.
    #[arg(long, env = "KUBEN_AGENT_HUB_CA", required_unless_present = "enrollment_dir")]
    hub_ca: Option<PathBuf>,
    /// This cluster's id.
    #[arg(long, env = "KUBEN_AGENT_CLUSTER", required_unless_present = "enrollment_dir")]
    cluster: Option<String>,
    /// Inside Kuben's own cluster: the directory where the hub publishes its
    /// address, CA, this cluster's id and a bootstrap token (a mounted
    /// Secret). Replaces --hub, --hub-ca, --cluster and --token-file.
    #[arg(
        long,
        env = "KUBEN_AGENT_ENROLLMENT_DIR",
        conflicts_with_all = ["hub", "hub_ca", "cluster", "token_file", "token_stdin"]
    )]
    enrollment_dir: Option<PathBuf>,
    /// Keep the device key and certificate in this Secret of the pod's own
    /// namespace too, so a new pod keeps the identity.
    #[arg(long, env = "KUBEN_AGENT_IDENTITY_SECRET")]
    identity_secret: Option<String>,
    /// Where the device key and the certificate live.
    #[arg(long, env = "KUBEN_AGENT_STATE_DIR", default_value = "/var/lib/kuben-agent")]
    state_dir: PathBuf,
    /// A file holding the bootstrap token, read only to enroll.
    #[arg(long, env = "KUBEN_AGENT_TOKEN_FILE", conflicts_with = "token_stdin")]
    token_file: Option<PathBuf>,
    /// Read the bootstrap token from stdin, only to enroll.
    #[arg(long)]
    token_stdin: bool,
    /// `json` or `pretty`.
    #[arg(long, env = "KUBEN_AGENT_LOG_FORMAT", default_value = "json")]
    log_format: String,
}

impl Args {
    fn token_source(&self) -> TokenSource {
        match (&self.token_file, self.token_stdin) {
            (Some(path), _) => TokenSource::File(path.clone()),
            (None, true) => TokenSource::Stdin,
            (None, false) => TokenSource::None,
        }
    }
}

fn init_logging(format: &str) -> anyhow::Result<()> {
    let filter = EnvFilter::try_from_default_env().or_else(|_| EnvFilter::try_new("info"))?;
    let registry = tracing_subscriber::registry().with(filter);
    if format == "pretty" {
        registry
            .with(fmt::layer().with_target(true).compact())
            .try_init()?;
    } else {
        registry
            .with(fmt::layer().json().flatten_event(true).with_current_span(true))
            .try_init()?;
    }
    Ok(())
}

/// Where the agent links to, and how it enrolls.
struct Target {
    hub: String,
    hub_ca: PathBuf,
    cluster: String,
    token: TokenSource,
    /// Published by the hub: wait for a token instead of failing without one.
    published: bool,
}

impl Target {
    async fn resolve(args: &Args, stop: &CancellationToken) -> anyhow::Result<Option<Self>> {
        if let Some(dir) = &args.enrollment_dir {
            let Some(e) = Enrollment::wait(dir, stop).await else {
                return Ok(None);
            };
            return Ok(Some(Self {
                hub: e.hub,
                hub_ca: e.hub_ca,
                cluster: e.cluster,
                token: TokenSource::File(e.token_file),
                published: true,
            }));
        }
        let missing = |what: &str| anyhow::anyhow!("{what} is required");
        Ok(Some(Self {
            hub: args.hub.clone().ok_or_else(|| missing("--hub"))?,
            hub_ca: args.hub_ca.clone().ok_or_else(|| missing("--hub-ca"))?,
            cluster: args.cluster.clone().ok_or_else(|| missing("--cluster"))?,
            token: args.token_source(),
            published: false,
        }))
    }

    /// The token; a published one that is not there yet counts as none.
    fn token(&self) -> Result<Option<String>, StateError> {
        match (&self.token, self.published) {
            (TokenSource::File(path), true) if !path.exists() => Ok(None),
            (source, _) => read_token(source),
        }
    }
}

fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    init_logging(&args.log_format)?;
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?
        .block_on(agent(args))
}

/// How long to wait before enrolling again with a published token.
const ENROLL_RETRY: Duration = Duration::from_secs(10);

async fn agent(args: Args) -> anyhow::Result<()> {
    let stop = CancellationToken::new();
    tokio::spawn(stop_on_signal(stop.clone()));
    // The cluster's apiserver, with the agent's credentials (in-cluster or
    // the kubeconfig): the only place they are used.
    let client = kube::Client::try_default().await?;
    let secret = match &args.identity_secret {
        Some(name) => {
            let namespace =
                own_namespace().ok_or_else(|| anyhow::anyhow!("--identity-secret needs to run in a pod"))?;
            Some(IdentitySecret::new(client.clone(), &namespace, name))
        }
        None => None,
    };
    let state = State::open(&args.state_dir)?;
    if let Some(secret) = &secret
        && secret.restore(&args.state_dir).await?
    {
        tracing::info!("identity restored from its Secret");
    }
    let key = state.device_key()?;
    if let Some(secret) = &secret {
        // Kept before enrolling: a pod that dies after redeeming the token
        // resumes the enrollment with the same key.
        secret.save(&args.state_dir).await?;
    }
    let Some(mut target) = Target::resolve(&args, &stop).await? else {
        return Ok(());
    };
    let hub_name = server_name(&args.hub_name)?;
    let identity = loop {
        let pinned = pinned_ca(&target.hub_ca)?;
        let connector = TcpConnector {
            address: target.hub.clone(),
        };
        let hub = HubAddress {
            connector: &connector,
            pinned: &pinned,
            name: &hub_name,
        };
        let attempt = ensure_identity(
            &state,
            &key,
            &hub,
            &target.cluster,
            || target.token(),
            OffsetDateTime::now_utc(),
        )
        .await;
        match attempt {
            Ok(identity) => break identity,
            // Published enrollment: the hub issues a fresh token when this
            // one is missing, spent or expired.
            Err(e) if target.published => {
                tracing::warn!(error = %e, retry_in_s = ENROLL_RETRY.as_secs(), "not enrolled yet");
                tokio::select! {
                    () = stop.cancelled() => return Ok(()),
                    () = tokio::time::sleep(ENROLL_RETRY) => {}
                }
                if let Some(again) = Target::resolve(&args, &stop).await? {
                    target = again;
                }
            }
            Err(e) => return Err(e.into()),
        }
    };
    if let Some(secret) = &secret {
        secret.save(&args.state_dir).await?;
    }
    tracing::info!(device = %key.device_id(), cluster = %target.cluster, hub = %target.hub, "kuben-agent starting");
    let pinned = pinned_ca(&target.hub_ca)?;
    let lifetime = state.certificate()?.map(|c| Lifetime {
        not_before: c.not_before,
        not_after: c.not_after,
    });
    let credentials = Arc::new(Credentials::new(pinned, identity, lifetime)?);
    let mut config = LinkConfig::new(target.cluster.clone(), hub_name.clone(), credentials);
    let keep = state.clone();
    config.kubernetes_version = client.apiserver_version().await.ok().map(|v| v.git_version);
    config.capabilities.insert(APPLICATION_RUNTIME.to_owned());
    config.executor = Some(Arc::new(KubeExecutor::new(client)));
    let dir = args.state_dir.clone();
    config.renewal = Some(Renewal {
        key: Arc::new(key),
        store: Arc::new(move |pem: &str| {
            keep.save_certificate(pem).map_err(|e| e.to_string())?;
            if let (Some(secret), Ok(runtime)) = (secret.clone(), tokio::runtime::Handle::try_current()) {
                let dir = dir.clone();
                runtime.spawn(async move {
                    if let Err(e) = secret.save(&dir).await {
                        tracing::warn!(error = %e, "the renewed certificate is not in its Secret yet");
                    }
                });
            }
            Ok(())
        }),
    });
    let connector = TcpConnector { address: target.hub };
    run(&connector, &config, &stop).await;
    tracing::info!("kuben-agent stopped");
    Ok(())
}

/// Cancel `token` on ctrl-c or, on Unix, SIGTERM.
async fn stop_on_signal(token: CancellationToken) {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let Ok(mut term) = signal(SignalKind::terminate()) else {
            let _ = tokio::signal::ctrl_c().await;
            token.cancel();
            return;
        };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
    token.cancel();
}

#[cfg(test)]
mod tests {
    use super::*;

    const BASE: [&str; 7] = [
        "kuben-agent",
        "--hub",
        "hub.example.com:7443",
        "--hub-ca",
        "ca.pem",
        "--cluster",
        "primary",
    ];

    #[test]
    fn the_token_is_never_an_argument() {
        let mut with_token = BASE.to_vec();
        with_token.extend(["--token", "kbt_secret"]);
        assert!(Args::try_parse_from(with_token).is_err());
    }

    #[test]
    fn an_enrollment_directory_replaces_the_hub_flags() {
        let args = Args::try_parse_from(["kuben-agent", "--enrollment-dir", "/etc/kuben-agent/enrollment"])
            .expect("args");
        assert_eq!(
            args.enrollment_dir.as_deref(),
            Some(std::path::Path::new("/etc/kuben-agent/enrollment"))
        );
        assert!(Args::try_parse_from(["kuben-agent"]).is_err(), "a hub is needed");
        let mut both = BASE.to_vec();
        both.extend(["--enrollment-dir", "/x"]);
        assert!(Args::try_parse_from(both).is_err(), "one or the other");
    }

    #[test]
    fn the_token_comes_from_one_file_or_stdin() {
        let args = Args::try_parse_from(BASE).expect("args");
        assert_eq!(args.token_source(), TokenSource::None);
        assert_eq!(args.hub_name, HUB_NAME);

        let mut file = BASE.to_vec();
        file.extend(["--token-file", "/run/secrets/token"]);
        let args = Args::try_parse_from(file).expect("args");
        assert_eq!(
            args.token_source(),
            TokenSource::File("/run/secrets/token".into())
        );

        let mut both = BASE.to_vec();
        both.extend(["--token-file", "t", "--token-stdin"]);
        assert!(Args::try_parse_from(both).is_err(), "one source only");
    }
}

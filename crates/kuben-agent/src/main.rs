//! `kuben-agent` — the Kuben cluster agent (ADR-027). Runs inside the
//! cluster, dials the hub over outbound mTLS and keeps that link up.
//!
//! On the first start it makes its device key, reads a bootstrap token
//! (`--token-file` or `--token-stdin`; never an argument, so it stays out of
//! process lists and shell history) and enrolls. Later starts use the stored
//! certificate, which the agent renews over the link before it expires.

use std::{path::PathBuf, sync::Arc};

use clap::Parser;
use kuben_agent::{
    link::{Credentials, Lifetime, LinkConfig, Renewal, TcpConnector, run},
    state::{HubAddress, State, TokenSource, ensure_identity, pinned_ca, read_token},
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
    #[arg(long, env = "KUBEN_AGENT_HUB")]
    hub: String,
    /// The name the hub's certificate carries.
    #[arg(long, env = "KUBEN_AGENT_HUB_NAME", default_value = HUB_NAME)]
    hub_name: String,
    /// The hub CA to pin, in PEM.
    #[arg(long, env = "KUBEN_AGENT_HUB_CA")]
    hub_ca: PathBuf,
    /// This cluster's id.
    #[arg(long, env = "KUBEN_AGENT_CLUSTER")]
    cluster: String,
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

fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    init_logging(&args.log_format)?;
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?
        .block_on(agent(args))
}

async fn agent(args: Args) -> anyhow::Result<()> {
    let pinned = pinned_ca(&args.hub_ca)?;
    let hub_name = server_name(&args.hub_name)?;
    let state = State::open(&args.state_dir)?;
    let key = state.device_key()?;
    tracing::info!(device = %key.device_id(), cluster = %args.cluster, hub = %args.hub, "kuben-agent starting");
    let connector = TcpConnector {
        address: args.hub.clone(),
    };
    let source = args.token_source();
    let hub = HubAddress {
        connector: &connector,
        pinned: &pinned,
        name: &hub_name,
    };
    let identity = ensure_identity(
        &state,
        &key,
        &hub,
        &args.cluster,
        || read_token(&source),
        OffsetDateTime::now_utc(),
    )
    .await?;
    let lifetime = state.certificate()?.map(|c| Lifetime {
        not_before: c.not_before,
        not_after: c.not_after,
    });
    let credentials = Arc::new(Credentials::new(pinned.clone(), identity, lifetime)?);
    let mut config = LinkConfig::new(args.cluster, hub_name.clone(), credentials);
    let keep = state.clone();
    config.renewal = Some(Renewal {
        key: Arc::new(key),
        store: Arc::new(move |pem: &str| keep.save_certificate(pem).map_err(|e| e.to_string())),
    });
    let token = CancellationToken::new();
    tokio::spawn(stop_on_signal(token.clone()));
    run(&connector, &config, &token).await;
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

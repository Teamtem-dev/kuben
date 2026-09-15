//! AgentLink in `kuben serve` (ADR-027, M1.9 integration): the hub's
//! endpoint for cluster agents, on the records of `kuben_store::repo::agents`.
//!
//! * [`SqlTokens`]: bootstrap tokens redeemed in SQL, by the database clock.
//! * [`SqlRegistry`]: who may link, and what is recorded of each agent; a
//!   database that cannot answer refuses the agent.
//! * [`cluster_ca`]: the cluster CA kept under `<state dir>/agentlink`. Its
//!   certificate is what agents pin; its private key is readable by the
//!   hub's user alone and never lives in the database (ADR-030).
//! * [`run`]: the listener; enrollments and linked agents share one port.

use std::{
    fs,
    io::Write as _,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use anyhow::Context as _;
use kuben_agent::{
    enroll::{ClusterCa, Enrollment, RESUME_GRACE, Redeemed, TokenRefused, TokenStore},
    hub::{Hub, HubSettings, Registry, SessionInfo},
    protocol::{APPLICATION_RUNTIME, Observation},
    tls::{ClientAuth, hub_config},
};
use kuben_core::{config::AgentCfg, ids::ClusterId};
use kuben_store::{
    Store,
    repo::{TokenRedemption, TokenRefusal},
};
use time::OffsetDateTime;
use tokio::net::TcpListener;
use tokio_rustls::TlsAcceptor;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

/// The cluster CA's certificate, the file agents pin (`--hub-ca`).
pub const CA_CERTIFICATE: &str = "ca.crt";
/// The cluster CA's private key.
pub const CA_KEY: &str = "ca.key";
/// How long a TLS handshake may take before the connection is dropped.
const HANDSHAKE: Duration = Duration::from_secs(10);
/// Lifetime of the hub's own server certificate, made at each start.
const HUB_CERTIFICATE: Duration = Duration::from_hours(24 * 30);

pub use kuben_agent::enroll::{new_token, token_hash};

/// The directory of the hub's AgentLink files.
#[must_use]
pub fn directory(state_dir: &Path) -> PathBuf {
    state_dir.join("agentlink")
}

fn write_owner_only(path: &Path, content: &str) -> std::io::Result<()> {
    fs::remove_file(path).ok();
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options.open(path)?.write_all(content.as_bytes())
}

/// The cluster CA kept in `dir`, made on first use.
pub fn cluster_ca(dir: &Path) -> anyhow::Result<ClusterCa> {
    let (certificate, key) = (dir.join(CA_CERTIFICATE), dir.join(CA_KEY));
    if certificate.exists() && key.exists() {
        let ca = ClusterCa::from_pem(
            &fs::read_to_string(&certificate).with_context(|| certificate.display().to_string())?,
            &fs::read_to_string(&key).with_context(|| key.display().to_string())?,
        )?;
        return Ok(ca);
    }
    fs::create_dir_all(dir).with_context(|| dir.display().to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }
    let ca = ClusterCa::generate("Kuben AgentLink CA")?;
    write_owner_only(&key, ca.key_pem()).with_context(|| key.display().to_string())?;
    fs::write(&certificate, ca.certificate_pem()).with_context(|| certificate.display().to_string())?;
    tracing::info!(dir = %dir.display(), "AgentLink CA made");
    Ok(ca)
}

/// A cluster id as the agent protocol carries it.
fn cluster_id(text: &str) -> Option<ClusterId> {
    Uuid::parse_str(text).ok().map(ClusterId::from_uuid)
}

/// Bootstrap tokens in SQL.
#[derive(Clone, Debug)]
pub struct SqlTokens {
    store: Store,
}

impl SqlTokens {
    #[must_use]
    pub const fn new(store: Store) -> Self {
        Self { store }
    }
}

impl TokenStore for SqlTokens {
    fn redeem(
        &self,
        hash: &[u8; 32],
        cluster: &str,
        device_id: &str,
        _now: OffsetDateTime,
    ) -> impl Future<Output = Result<Redeemed, TokenRefused>> + Send {
        let (store, hash, cluster, device) = (
            self.store.clone(),
            *hash,
            cluster_id(cluster),
            device_id.to_owned(),
        );
        async move {
            let Some(cluster) = cluster else {
                return Err(TokenRefused::Unknown);
            };
            match store
                .redeem_agent_token(&hash, cluster, &device, RESUME_GRACE)
                .await
            {
                Ok(Ok(redeemed)) => Ok(match redeemed.kind {
                    TokenRedemption::First => Redeemed::First,
                    TokenRedemption::Resumed => Redeemed::Resumed,
                }),
                Ok(Err(refusal)) => Err(match refusal {
                    TokenRefusal::Unknown => TokenRefused::Unknown,
                    TokenRefusal::Expired => TokenRefused::Expired,
                    TokenRefusal::OtherCluster => TokenRefused::OtherCluster,
                    TokenRefusal::OtherDevice => TokenRefused::OtherDevice,
                }),
                Err(error) => {
                    tracing::warn!(%error, "cannot redeem an agent token");
                    Err(TokenRefused::Unknown)
                }
            }
        }
    }
}

/// What the hub knows of each cluster's agent, in SQL.
#[derive(Clone, Debug)]
pub struct SqlRegistry {
    store: Store,
}

impl SqlRegistry {
    #[must_use]
    pub const fn new(store: Store) -> Self {
        Self { store }
    }
}

impl Registry for SqlRegistry {
    fn admits(&self, cluster: &str, device: &str) -> impl Future<Output = bool> + Send {
        let (store, cluster, device) = (self.store.clone(), cluster_id(cluster), device.to_owned());
        async move {
            let Some(cluster) = cluster else {
                return false;
            };
            match store.cluster_agent(cluster).await {
                Ok(agent) => agent.is_some_and(|a| a.accepts(&device)),
                Err(error) => {
                    tracing::warn!(%error, %cluster, "cannot read the cluster's agent; refusing");
                    false
                }
            }
        }
    }

    fn certified(
        &self,
        cluster: &str,
        device: &str,
        not_after: OffsetDateTime,
    ) -> impl Future<Output = ()> + Send {
        let (store, cluster, device) = (self.store.clone(), cluster_id(cluster), device.to_owned());
        async move {
            let Some(cluster) = cluster else {
                return;
            };
            let org = match store.cluster_agent(cluster).await {
                Ok(Some(agent)) => Some(agent.org),
                Ok(None) => store.agent_token_org(cluster, &device).await.ok().flatten(),
                Err(error) => {
                    tracing::warn!(%error, %cluster, "cannot read the cluster's agent");
                    None
                }
            };
            let Some(org) = org else {
                tracing::warn!(%cluster, "a certificate for a cluster no redeemed token names");
                return;
            };
            let not_after = i64::try_from(not_after.unix_timestamp_nanos() / 1_000_000).unwrap_or(i64::MAX);
            if let Err(error) = store
                .record_agent_certificate(org, cluster, &device, not_after)
                .await
            {
                tracing::warn!(%error, %cluster, "cannot record the agent's certificate");
            }
        }
    }

    fn linked(&self, cluster: &str, device: &str, session: &SessionInfo) -> impl Future<Output = ()> + Send {
        let (store, cluster, device) = (self.store.clone(), cluster_id(cluster), device.to_owned());
        let features: Vec<String> = session.features.iter().cloned().collect();
        let (version, agent_version) = (session.version, session.agent_version.clone());
        async move {
            let Some(cluster) = cluster else {
                return;
            };
            match store
                .record_agent_link(cluster, &device, version, &features, &agent_version)
                .await
            {
                Ok(true) => {
                    tracing::info!(%cluster, protocol = version, agent = %agent_version, "agent linked");
                }
                Ok(false) => tracing::warn!(%cluster, "a link of a device that is not the cluster's agent"),
                Err(error) => tracing::warn!(%error, %cluster, "cannot record the agent's link"),
            }
        }
    }

    fn observed(
        &self,
        cluster: &str,
        _device: &str,
        observation: &Observation,
    ) -> impl Future<Output = ()> + Send {
        // The materializer follows envelopes from here on (M1.9, next step).
        tracing::info!(
            cluster,
            target = %observation.target,
            generation = observation.generation,
            phase = ?observation.phase,
            reason = observation.reason.as_deref().unwrap_or_default(),
            "agent observation"
        );
        std::future::ready(())
    }

    fn heard(&self, cluster: &str, device: &str) -> impl Future<Output = ()> + Send {
        let (store, cluster, device) = (self.store.clone(), cluster_id(cluster), device.to_owned());
        async move {
            let Some(cluster) = cluster else {
                return;
            };
            if let Err(error) = store.touch_agent(cluster, &device).await {
                tracing::debug!(%error, %cluster, "cannot record the agent's heartbeat");
            }
        }
    }
}

/// Serve AgentLink on `config.bind` until `token` is cancelled; nothing
/// when it is unset.
pub async fn run(
    config: AgentCfg,
    state_dir: PathBuf,
    store: Store,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let Some(bind) = config.bind.clone() else {
        return Ok(());
    };
    let ca = cluster_ca(&directory(&state_dir))?;
    let trust = vec![ca.certificate().clone()];
    let identity = ca.server_identity(&config.hub_name, HUB_CERTIFICATE, OffsetDateTime::now_utc())?;
    let acceptor = TlsAcceptor::from(hub_config(&trust, identity, ClientAuth::EnrollmentAllowed)?);
    let lifetime = Duration::from_hours(config.certificate_hours.max(1));
    let hub = Arc::new(Hub::new(
        Enrollment::new(ca, SqlTokens::new(store.clone()), lifetime),
        SqlRegistry::new(store),
        HubSettings {
            heartbeat: Duration::from_secs(config.heartbeat_secs.max(1)),
            features: [APPLICATION_RUNTIME.to_owned()].into(),
            ..HubSettings::default()
        },
    ));
    let listener = TcpListener::bind(&bind)
        .await
        .with_context(|| format!("cannot listen for agents on {bind}"))?;
    tracing::info!(%bind, hub = %config.hub_name, "AgentLink listening");
    loop {
        let (tcp, peer) = tokio::select! {
            () = token.cancelled() => return Ok(()),
            accepted = listener.accept() => match accepted {
                Ok(accepted) => accepted,
                Err(error) => {
                    tracing::warn!(%error, "AgentLink accept failed");
                    continue;
                }
            },
        };
        let (acceptor, hub) = (acceptor.clone(), hub.clone());
        tokio::spawn(async move {
            match tokio::time::timeout(HANDSHAKE, acceptor.accept(tcp)).await {
                Ok(Ok(tls)) => {
                    if let Err(error) = hub.serve(tls).await {
                        tracing::debug!(%peer, %error, "AgentLink connection ended");
                    }
                }
                Ok(Err(error)) => tracing::debug!(%peer, %error, "AgentLink TLS refused"),
                Err(_) => tracing::debug!(%peer, "AgentLink TLS handshake timed out"),
            }
        });
    }
}

#[cfg(test)]
mod tests {
    use kuben_store::testing::{pg_store, skip};

    use super::*;

    fn temp_dir(name: &str) -> PathBuf {
        std::env::temp_dir().join(format!(
            "kuben-agentlink-{name}-{}-{}",
            std::process::id(),
            OffsetDateTime::now_utc().unix_timestamp_nanos()
        ))
    }

    #[test]
    fn the_cluster_ca_is_made_once_and_its_key_kept_private() {
        let dir = temp_dir("ca");
        let first = cluster_ca(&dir).expect("made");
        let kept = cluster_ca(&dir).expect("reloaded");
        assert_eq!(
            kept.certificate(),
            first.certificate(),
            "the same trust after a restart"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = |p: &Path| fs::metadata(p).expect("metadata").permissions().mode() & 0o777;
            assert_eq!(mode(&dir.join(CA_KEY)), 0o600);
            assert_eq!(mode(&dir), 0o700);
        }
        fs::remove_dir_all(&dir).ok();
    }

    #[tokio::test]
    async fn sql_admits_only_the_device_a_token_enrolled_until_it_is_revoked() {
        let Some(store) = pg_store().await else {
            skip("AgentLink registry");
            return;
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let cluster = t.create_cluster("primary").await.expect("cluster");
        let token = new_token();
        assert!(
            t.create_agent_token(cluster, &token_hash(&token), Duration::from_mins(30), "test")
                .await
                .expect("token")
        );
        t.commit().await.expect("commit");

        let (tokens, registry) = (SqlTokens::new(store.clone()), SqlRegistry::new(store.clone()));
        let id = cluster.to_string();
        let now = OffsetDateTime::now_utc();
        assert_eq!(
            tokens.redeem(&token_hash(&token), &id, "sha256:a", now).await,
            Ok(Redeemed::First)
        );
        assert!(!registry.admits(&id, "sha256:a").await, "nothing certified yet");
        registry
            .certified(&id, "sha256:a", now + Duration::from_hours(24))
            .await;
        assert!(registry.admits(&id, "sha256:a").await);
        assert!(!registry.admits(&id, "sha256:b").await, "another device");
        assert!(!registry.admits("not-a-uuid", "sha256:a").await);

        let mut t = store.tenant(org).await.expect("tenant");
        assert!(t.revoke_cluster_agent(cluster).await.expect("revoke"));
        t.commit().await.expect("commit");
        assert!(!registry.admits(&id, "sha256:a").await, "revoked");
        assert_eq!(
            tokens.redeem(&token_hash(&token), &id, "sha256:b", now).await,
            Err(TokenRefused::OtherDevice)
        );
    }
}

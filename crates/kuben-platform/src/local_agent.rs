//! The agent inside Kuben's own cluster (M2.8, plan §11: the agent is a
//! separate process from the first real install).
//!
//! * [`sync_ca`]: in a pod, the AgentLink CA lives in a Secret, so a new pod
//!   keeps the CA its agents pin (ADR-030: never in the database).
//! * [`run`]: the hub publishes the local agent's enrollment in a Secret the
//!   agent's pod mounts: its own address, the CA, the installation's
//!   `primary` cluster and, while that cluster has no agent with a valid
//!   certificate, a short-lived bootstrap token, renewed before it expires.
//!   Once the agent enrolled, the token is taken out again.

use std::{collections::BTreeMap, path::Path, time::Duration};

use anyhow::Context as _;
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api, Client,
    api::{Patch, PatchParams, PostParams},
};
use kuben_core::{config::AgentCfg, time::now_ms};
use kuben_store::Store;
use serde_json::json;
use tokio_util::sync::CancellationToken;

use crate::{
    agentlink::{CA_CERTIFICATE, CA_KEY, cluster_ca, directory, new_token, token_hash},
    registry::ClusterId,
};

pub const ENROLLMENT_SECRET: &str = "kuben-agent-enrollment";
pub const CA_SECRET: &str = "kuben-agentlink-ca";
const EXPIRES: &str = "kuben.dev/token-expires-at";
const MANAGER: &str = "kuben-hub";
const TOKEN_TTL: Duration = Duration::from_hours(1);
/// A token closer than this to its expiry is replaced.
const RENEW_BEFORE: Duration = Duration::from_mins(10);
const INTERVAL: Duration = Duration::from_secs(30);

/// The namespace of the local agent and its Secrets.
#[must_use]
pub fn namespace(config: &AgentCfg, kube_namespace: Option<&str>) -> String {
    config
        .namespace
        .clone()
        .filter(|n| !n.is_empty())
        .or_else(|| crate::registry::own_namespace(kube_namespace))
        .unwrap_or_else(|| "kuben-system".to_owned())
}

fn bytes(secret: &Secret, key: &str) -> Option<Vec<u8>> {
    secret.data.as_ref()?.get(key).map(|b| b.0.clone())
}

/// Keep the AgentLink CA of `state_dir` and the Secret [`CA_SECRET`] the
/// same: the Secret wins when it exists; a CA made here is stored in it.
pub async fn sync_ca(client: Client, namespace: &str, state_dir: &Path) -> anyhow::Result<()> {
    let api: Api<Secret> = Api::namespaced(client, namespace);
    let dir = directory(state_dir);
    let restore = |secret: &Secret| -> anyhow::Result<bool> {
        let (Some(cert), Some(key)) = (bytes(secret, CA_CERTIFICATE), bytes(secret, CA_KEY)) else {
            return Ok(false);
        };
        std::fs::create_dir_all(&dir)?;
        std::fs::write(dir.join(CA_CERTIFICATE), cert)?;
        write_private(&dir.join(CA_KEY), &key)?;
        Ok(true)
    };
    if let Some(secret) = api.get_opt(CA_SECRET).await?
        && restore(&secret)?
    {
        return Ok(());
    }
    let ca = cluster_ca(&dir)?;
    let secret = Secret {
        metadata: kube::api::ObjectMeta {
            name: Some(CA_SECRET.into()),
            labels: Some(BTreeMap::from([(
                "app.kubernetes.io/managed-by".into(),
                "kuben".into(),
            )])),
            ..kube::api::ObjectMeta::default()
        },
        data: Some(BTreeMap::from([
            (
                CA_CERTIFICATE.into(),
                ByteString(ca.certificate_pem().as_bytes().to_vec()),
            ),
            (CA_KEY.into(), ByteString(ca.key_pem().as_bytes().to_vec())),
        ])),
        type_: Some("Opaque".into()),
        ..Secret::default()
    };
    match api.create(&PostParams::default(), &secret).await {
        Ok(_) => {
            tracing::info!(%namespace, secret = CA_SECRET, "AgentLink CA stored");
            Ok(())
        }
        // Another replica stored its CA first: use that one.
        Err(kube::Error::Api(s)) if s.code == 409 => {
            let secret = api.get(CA_SECRET).await?;
            anyhow::ensure!(restore(&secret)?, "{CA_SECRET} holds no CA");
            Ok(())
        }
        Err(e) => Err(e.into()),
    }
}

fn write_private(path: &Path, content: &[u8]) -> std::io::Result<()> {
    use std::io::Write as _;
    std::fs::remove_file(path).ok();
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options.open(path)?.write_all(content)
}

/// What the enrollment Secret should hold.
#[derive(Debug, PartialEq, Eq)]
struct Published {
    hub: String,
    ca: String,
    cluster: String,
    /// `(token, expires at ms)` while the agent is not enrolled.
    token: Option<(String, i64)>,
}

/// The token to publish: the live one while it is fresh, a new one when
/// the agent still needs one, none once it enrolled. `issue` makes a token.
fn token_to_publish(
    enrolled: bool,
    live: Option<(String, i64)>,
    now: i64,
    issue: impl FnOnce() -> (String, i64),
) -> Option<(String, i64)> {
    if enrolled {
        return None;
    }
    let renew_before = i64::try_from(RENEW_BEFORE.as_millis()).unwrap_or(i64::MAX);
    match live {
        Some((token, expires)) if expires - renew_before > now => Some((token, expires)),
        _ => Some(issue()),
    }
}

fn secret_body(published: &Published) -> serde_json::Value {
    let mut data = BTreeMap::from([
        ("hub", ByteString(published.hub.clone().into_bytes())),
        ("hub-ca.crt", ByteString(published.ca.clone().into_bytes())),
        ("cluster", ByteString(published.cluster.clone().into_bytes())),
    ]);
    let mut annotations = serde_json::Map::new();
    if let Some((token, expires)) = &published.token {
        data.insert("token", ByteString(token.clone().into_bytes()));
        annotations.insert(EXPIRES.into(), json!(expires.to_string()));
    }
    json!({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": ENROLLMENT_SECRET,
            "labels": { "app.kubernetes.io/managed-by": "kuben" },
            "annotations": annotations,
        },
        "type": "Opaque",
        "data": data,
    })
}

fn live_published(secret: &Secret) -> Option<Published> {
    let text = |key: &str| bytes(secret, key).and_then(|b| String::from_utf8(b).ok());
    let token = text("token").and_then(|t| {
        let expires = secret.metadata.annotations.as_ref()?.get(EXPIRES)?.parse().ok()?;
        Some((t, expires))
    });
    Some(Published {
        hub: text("hub")?,
        ca: text("hub-ca.crt")?,
        cluster: text("cluster")?,
        token,
    })
}

struct Publisher {
    store: Store,
    secrets: Api<Secret>,
    hub: String,
    ca: String,
    org_slug: String,
}

impl Publisher {
    async fn once(&self) -> anyhow::Result<()> {
        let Some(org) = self.store.installation_org(&self.org_slug).await? else {
            return Ok(()); // No organization yet: the setup page makes one.
        };
        let mut tenant = self.store.tenant(org).await?;
        let cluster = tenant.ensure_cluster(ClusterId::PRIMARY).await?;
        tenant.commit().await?;
        let now = now_ms();
        let enrolled = self
            .store
            .cluster_agent(cluster)
            .await?
            .is_some_and(|a| a.revoked_at.is_none() && a.certificate_not_after > now);
        let live = self.secrets.get_opt(ENROLLMENT_SECRET).await?;
        let live = live.as_ref().and_then(live_published);
        let cluster_text = cluster.to_string();
        let same_cluster = live.as_ref().is_some_and(|l| l.cluster == cluster_text);
        let live_token = live
            .as_ref()
            .filter(|_| same_cluster)
            .and_then(|l| l.token.clone());
        let mut issued = None;
        let token = token_to_publish(enrolled, live_token, now, || {
            let token = new_token();
            let ttl = i64::try_from(TOKEN_TTL.as_millis()).unwrap_or(i64::MAX);
            issued = Some(token.clone());
            (token, now + ttl)
        });
        if let Some(token) = &issued {
            let mut tenant = self.store.tenant(org).await?;
            let created = tenant
                .create_agent_token(cluster, &token_hash(token), TOKEN_TTL, "hub:local-agent")
                .await?;
            anyhow::ensure!(created, "the primary cluster is not the installation's");
            tenant.commit().await?;
        }
        let desired = Published {
            hub: self.hub.clone(),
            ca: self.ca.clone(),
            cluster: cluster_text,
            token,
        };
        if live.as_ref() == Some(&desired) {
            return Ok(());
        }
        self.secrets
            .patch(
                ENROLLMENT_SECRET,
                &PatchParams::apply(MANAGER).force(),
                &Patch::Apply(secret_body(&desired)),
            )
            .await?;
        tracing::info!(
            %cluster,
            token = desired.token.is_some(),
            "local agent enrollment published"
        );
        Ok(())
    }
}

/// Publish the local agent's enrollment until cancelled.
pub async fn run(
    store: Store,
    client: Client,
    config: AgentCfg,
    org_slug: String,
    state_dir: std::path::PathBuf,
    namespace: String,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let hub = config
        .advertise
        .clone()
        .filter(|a| !a.is_empty())
        .context("agent.local needs agent.advertise, the address agents dial")?;
    let ca = std::fs::read_to_string(directory(&state_dir).join(CA_CERTIFICATE))
        .context("the AgentLink CA is not there yet")?;
    let publisher = Publisher {
        store,
        secrets: Api::namespaced(client, &namespace),
        hub,
        ca,
        org_slug,
    };
    loop {
        if let Err(e) = publisher.once().await {
            tracing::warn!(error = %e, "cannot publish the local agent's enrollment; retrying");
        }
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(INTERVAL) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_token_is_published_only_until_the_agent_enrolled() {
        let now = 1_000_000_000;
        let hour = 3_600_000;
        let issue = || ("new".to_owned(), now + hour);
        assert_eq!(
            token_to_publish(true, Some(("old".into(), now + hour)), now, issue),
            None
        );
        assert_eq!(
            token_to_publish(false, Some(("old".into(), now + hour)), now, issue),
            Some(("old".into(), now + hour)),
            "a fresh token stays"
        );
        assert_eq!(
            token_to_publish(false, Some(("old".into(), now + 60_000)), now, issue),
            Some(("new".into(), now + hour)),
            "one close to its expiry is replaced"
        );
        assert_eq!(
            token_to_publish(false, None, now, issue),
            Some(("new".into(), now + hour))
        );
    }

    #[test]
    fn the_secret_carries_the_enrollment_and_reads_back() {
        let published = Published {
            hub: "kuben.kuben-system.svc:7443".into(),
            ca: "-----BEGIN CERTIFICATE-----".into(),
            cluster: "0190".into(),
            token: Some(("kbt_x".into(), 42)),
        };
        let secret: Secret = serde_json::from_value(secret_body(&published)).expect("secret");
        assert_eq!(live_published(&secret), Some(published));
        let enrolled = Published {
            hub: "h:1".into(),
            ca: "c".into(),
            cluster: "0190".into(),
            token: None,
        };
        let secret: Secret = serde_json::from_value(secret_body(&enrolled)).expect("secret");
        assert!(bytes(&secret, "token").is_none(), "no token once enrolled");
        assert_eq!(live_published(&secret), Some(enrolled));
    }
}

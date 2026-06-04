//! Cluster registry. Immutable after construction; every operation names its
//! cluster explicitly (Invariant I-11). Phase 0 supports a single cluster;
//! the type is already keyed so multi-cluster (phase 3) is additive.

use std::{collections::BTreeMap, fmt, sync::Arc};

use kube::{Client, Config, config::KubeConfigOptions};
use kuben_core::config::KubeCfg;

#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct ClusterId(Arc<str>);

impl ClusterId {
    pub const PRIMARY: &'static str = "primary";

    #[must_use]
    pub fn primary() -> Self {
        Self(Arc::from(Self::PRIMARY))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for ClusterId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// Replace the `user:password@` part of every URL inside `text` with `***@`.
/// Cluster errors quote proxy and server URLs; their credentials must never
/// reach logs, health details or terminal output.
#[must_use]
pub fn redact_credentials(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(i) = rest.find("://") {
        let (head, tail) = rest.split_at(i + 3);
        out.push_str(head);
        let end = tail
            .find(|c: char| matches!(c, '/' | '?' | '#' | '"' | '\'' | '<' | '>') || c.is_whitespace())
            .unwrap_or(tail.len());
        let authority = &tail[..end];
        match authority.rfind('@') {
            Some(at) => {
                out.push_str("***@");
                out.push_str(&authority[at + 1..]);
            }
            None => out.push_str(authority),
        }
        rest = &tail[end..];
    }
    out.push_str(rest);
    out
}

/// Mounted into every pod that has a service-account token.
const POD_NAMESPACE_FILE: &str = "/var/run/secrets/kubernetes.io/serviceaccount/namespace";

/// The namespace Kuben itself runs in: `configured` (`kube.namespace`) when
/// set, else the pod's service-account namespace, else `None` (a binary
/// running outside the cluster).
#[must_use]
pub fn own_namespace(configured: Option<&str>) -> Option<String> {
    let non_empty = |s: &str| {
        let s = s.trim();
        (!s.is_empty()).then(|| s.to_owned())
    };
    configured.and_then(non_empty).or_else(|| {
        std::fs::read_to_string(POD_NAMESPACE_FILE)
            .ok()
            .and_then(|s| non_empty(&s))
    })
}

#[derive(Clone)]
pub struct ClusterRegistry {
    clients: Arc<BTreeMap<ClusterId, Client>>,
}

impl fmt::Debug for ClusterRegistry {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ClusterRegistry")
            .field("clusters", &self.clients.keys().collect::<Vec<_>>())
            .finish()
    }
}

impl ClusterRegistry {
    /// Build the registry for the server. When `kube.required` is false, any
    /// failure (no kubeconfig, unreachable proxy, bad credentials) yields
    /// `Ok(None)` so the API/UI still start degraded for setup and diagnosis.
    pub async fn from_config(cfg: &KubeCfg) -> anyhow::Result<Option<Self>> {
        match Self::connect(cfg).await {
            Ok(registry) => Ok(Some(registry)),
            Err(e) if !cfg.required => {
                tracing::warn!(error = %e, "kubernetes cluster unavailable; starting without cluster");
                Ok(None)
            }
            Err(e) => Err(e),
        }
    }

    /// Connect to the configured cluster. Error messages are redacted.
    pub async fn connect(cfg: &KubeCfg) -> anyhow::Result<Self> {
        Self::try_connect(cfg)
            .await
            .map_err(|e| anyhow::anyhow!(redact_credentials(&format!("{e:#}"))))
    }

    async fn try_connect(cfg: &KubeCfg) -> anyhow::Result<Self> {
        let options = KubeConfigOptions {
            context: cfg.context.clone(),
            ..KubeConfigOptions::default()
        };
        let config = if let Some(path) = &cfg.kubeconfig {
            let kubeconfig = kube::config::Kubeconfig::read_from(path)?;
            Config::from_custom_kubeconfig(kubeconfig, &options).await?
        } else if cfg.context.is_some() {
            Config::from_kubeconfig(&options).await?
        } else {
            Config::infer().await?
        };
        Ok(Self::single(Client::try_from(config)?))
    }

    /// Registry with exactly one cluster.
    #[must_use]
    pub fn single(client: Client) -> Self {
        let mut m = BTreeMap::new();
        m.insert(ClusterId::primary(), client);
        Self { clients: Arc::new(m) }
    }

    #[must_use]
    pub fn get(&self, id: &ClusterId) -> Option<Client> {
        self.clients.get(id).cloned()
    }

    /// The primary cluster (always present).
    ///
    /// # Panics
    /// Never: the registry cannot be constructed without a primary cluster.
    #[must_use]
    pub fn primary(&self) -> Client {
        self.clients
            .get(&ClusterId::primary())
            .cloned()
            .expect("registry always has a primary cluster")
    }

    pub fn ids(&self) -> impl Iterator<Item = &ClusterId> {
        self.clients.keys()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn credentials_are_redacted_from_urls() {
        assert_eq!(
            redact_credentials("configured proxy http://user:s3cret@proxy.local:3128/ requires a feature"),
            "configured proxy http://***@proxy.local:3128/ requires a feature"
        );
        assert_eq!(
            redact_credentials("a postgres://kuben:pw@db:5432/kuben b https://token@api.example.com"),
            "a postgres://***@db:5432/kuben b https://***@api.example.com"
        );
    }

    #[test]
    fn text_without_credentials_is_untouched() {
        for s in [
            "https://10.0.0.1:6443/api",
            "no url here",
            "https://h/path/a@b",
            "trailing ://",
            "",
        ] {
            assert_eq!(redact_credentials(s), s);
        }
    }

    #[test]
    fn configured_namespace_wins() {
        assert_eq!(
            own_namespace(Some(" kuben-system ")).as_deref(),
            Some("kuben-system")
        );
    }

    #[tokio::test]
    async fn unusable_config_degrades_unless_required() {
        let mut cfg = KubeCfg {
            kubeconfig: Some("/nonexistent/kubeconfig".into()),
            ..KubeCfg::default()
        };
        assert!(
            ClusterRegistry::from_config(&cfg)
                .await
                .expect("degrades")
                .is_none()
        );
        cfg.required = true;
        assert!(ClusterRegistry::from_config(&cfg).await.is_err());
    }
}

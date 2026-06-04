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


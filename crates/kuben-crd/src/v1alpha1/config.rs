//! `KubenConfig` — cluster-scoped singleton holding platform settings.
//! One resource replaces both the platform CR **and** a `config.yaml` file:
//! settings live in the cluster, not on disk (Invariant I-17).

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::{Condition, SizePreset};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "KubenConfig",
    plural = "kubenconfigs",
    status = "KubenConfigStatus",
    shortname = "kcfg",
    printcolumn = r#"{"name":"BaseDomain","type":"string","jsonPath":".spec.baseDomain"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct KubenConfigSpec {
    /// Base domain for generated app hostnames, e.g. `apps.example.com`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub base_domain: Option<String>,
    /// Gateway API `Gateway` used for HTTPRoutes (`namespace/name`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gateway: Option<String>,
    /// cert-manager ClusterIssuer name.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cluster_issuer: Option<String>,
    /// Secret in the Gateway's namespace holding a certificate for
    /// `*.<baseDomain>` (DNS-01). When set, generated hostnames share one
    /// wildcard HTTPS listener instead of one listener per host.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub wildcard_tls_secret: Option<String>,
    /// Container registry used for build outputs.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub registry: Option<RegistryCfg>,
    /// Compute size presets.
    #[serde(default = "default_sizes")]
    pub sizes: Vec<SizePreset>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct RegistryCfg {
    /// e.g. `ghcr.io/acme`.
    pub prefix: String,
    /// Secret (in `kuben-system`) holding a `.dockerconfigjson`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credentials_secret: Option<String>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct KubenConfigStatus {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_generation: Option<i64>,
    #[serde(default)]
    pub conditions: Vec<Condition>,
}

fn default_sizes() -> Vec<SizePreset> {
    vec![
        SizePreset {
            name: "nano".into(),
            cpu_request: "50m".into(),
            cpu_limit: None,
            memory_request: "64Mi".into(),
            memory_limit: "128Mi".into(),
        },
        SizePreset {
            name: "small".into(),
            cpu_request: "100m".into(),
            cpu_limit: None,
            memory_request: "128Mi".into(),
            memory_limit: "256Mi".into(),
        },
        SizePreset {
            name: "medium".into(),
            cpu_request: "250m".into(),
            cpu_limit: None,
            memory_request: "512Mi".into(),
            memory_limit: "1Gi".into(),
        },
        SizePreset {
            name: "large".into(),
            cpu_request: "1".into(),
            cpu_limit: None,
            memory_request: "2Gi".into(),
            memory_limit: "4Gi".into(),
        },
    ]
}

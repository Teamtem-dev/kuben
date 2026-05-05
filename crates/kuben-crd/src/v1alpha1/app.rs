//! `App` — the unit of deployment. Namespaced (lives in its Environment's
//! namespace). Secrets are referenced, never inlined (ADR: Release snapshots
//! hold Secret references, not values).

use std::collections::BTreeMap;

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::{Condition, KeyRef};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "App",
    namespaced,
    status = "AppStatus",
    shortname = "kapp",
    printcolumn = r#"{"name":"Ready","type":"string","jsonPath":".status.conditions[?(@.type==\"Ready\")].status"}"#,
    printcolumn = r#"{"name":"Release","type":"string","jsonPath":".status.currentRelease"}"#,
    printcolumn = r#"{"name":"URL","type":"string","jsonPath":".status.url"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct AppSpec {
    pub source: Source,
    pub runtime: Runtime,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<EnvVar>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub domains: Vec<Domain>,
    /// Persistent volumes (scenario 6). An app with volumes runs a single
    /// process with at most one replica (ReadWriteOnce).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub volumes: Vec<Volume>,
}

/// A persistent volume mounted into the app's process. Backed by a PVC named
/// `<app>-<name>` that is **retained** when the app is deleted.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Volume {
    pub name: String,
    /// Absolute path inside the container, e.g. `/data`.
    pub mount_path: String,
    /// Requested capacity, e.g. `5Gi`. Can grow, never shrink.
    #[serde(default = "default_volume_size")]
    pub size: String,
    /// StorageClass; the cluster default when omitted.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub storage_class: Option<String>,
}

fn default_volume_size() -> String {
    "1Gi".into()
}

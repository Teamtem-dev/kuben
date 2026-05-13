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

/// How a process's port is exposed.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum Protocol {
    /// Routed through the Gateway (HTTPRoute) on the app's hostnames.
    #[default]
    Http,
    /// Cluster-internal TCP (databases, caches): a Service on the real port,
    /// never a public route.
    Tcp,
}

impl Protocol {
    #[must_use]
    pub const fn is_http(&self) -> bool {
        matches!(self, Self::Http)
    }
}

/// Where the image comes from. Set exactly one of `image` or `git`
/// (Kubernetes one-of style: structural CRD schemas cannot express tagged enums).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Source {
    /// Prebuilt image, e.g. `ghcr.io/acme/api:1.2.3`. Pinned to a digest at `Release` time.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image: Option<String>,
    /// Build from a git repository.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub git: Option<GitSource>,
}

impl Source {
    /// A prebuilt-image source.
    #[must_use]
    pub fn from_image(image: impl Into<String>) -> Self {
        Self {
            image: Some(image.into()),
            git: None,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct GitSource {
    pub repo: String,
    #[serde(default = "default_branch")]
    pub branch: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
    #[serde(default)]
    pub build: Build,
}

fn default_branch() -> String {
    "main".into()
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Build {
    #[serde(default)]
    pub strategy: BuildStrategy,
    /// Dockerfile path relative to `path` (strategy `dockerfile`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub dockerfile: Option<String>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum BuildStrategy {
    /// Dockerfile if present, otherwise Railpack.
    #[default]
    Auto,
    Dockerfile,
    Railpack,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Runtime {
    /// Named processes (`web`, `worker`, ...). Exactly one may expose a port.
    pub processes: BTreeMap<String, Process>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub health_check: Option<HealthCheck>,
    /// Group id that owns the volumes (`fsGroup`), for images running as a
    /// non-root user, e.g. `1000`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fs_group: Option<i64>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Process {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub command: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub port: Option<u16>,
    /// Size preset name from `KubenConfig.spec.sizes`.
    #[serde(default = "default_size")]
    pub size: String,
    #[serde(default)]
    pub replicas: Replicas,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub idle: Option<Idle>,
    /// Cron expression (scenario 7). A scheduled process runs as a CronJob
    /// instead of a Deployment and may not expose a port.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schedule: Option<String>,
    /// IANA time zone for `schedule`, e.g. `Europe/Berlin` (default UTC).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub time_zone: Option<String>,
    #[serde(default, skip_serializing_if = "Protocol::is_http")]
    pub protocol: Protocol,
}

fn default_size() -> String {
    "small".into()
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Replicas {
    #[serde(default = "one")]
    pub min: u32,
    #[serde(default = "one")]
    pub max: u32,
}

impl Default for Replicas {
    fn default() -> Self {
        Self { min: 1, max: 1 }
    }
}

const fn one() -> u32 {
    1
}

/// Scale-to-zero / throttling configuration (Master Blueprint §3.3).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Idle {
    #[serde(default)]
    pub mode: IdleMode,
    /// Inactivity before sleeping, e.g. `15m`.
    #[serde(default = "default_idle_after")]
    pub after: String,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum IdleMode {
    #[default]
    Off,
    Zero,
    Throttle,
}

fn default_idle_after() -> String {
    "15m".into()
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct HealthCheck {
    pub path: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub port: Option<u16>,
}

/// Environment variable. Exactly one of `value`, `fromSecret`, `fromService`.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct EnvVar {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub value: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub from_secret: Option<KeyRef>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub from_service: Option<KeyRef>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Domain {
    pub host: String,
    /// `auto` (cert-manager), `none`, or a Secret name.
    #[serde(default = "default_tls")]
    pub tls: String,
}

fn default_tls() -> String {
    "auto".into()
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct AppStatus {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_generation: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub current_release: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(default)]
    pub conditions: Vec<Condition>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn app_spec_roundtrips_yaml() {
        let yaml = r"
source:
  git:
    repo: https://github.com/acme/shop
    branch: main
runtime:
  processes:

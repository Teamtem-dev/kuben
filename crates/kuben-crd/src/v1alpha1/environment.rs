//! `Environment` — cluster-scoped; owns exactly one Namespace
//! (`kb-<project>-<env>`) with PSA, ResourceQuota and NetworkPolicy.
//! Deletion is soft by default (ADR-018).

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::Condition;

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "Environment",
    status = "EnvironmentStatus",
    shortname = "kenv",
    printcolumn = r#"{"name":"Project","type":"string","jsonPath":".spec.project"}"#,
    printcolumn = r#"{"name":"Type","type":"string","jsonPath":".spec.type"}"#,
    printcolumn = r#"{"name":"Namespace","type":"string","jsonPath":".status.namespace"}"#,
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentSpec {
    /// Name of the owning `Project`.
    pub project: String,
    #[serde(default, rename = "type")]
    pub type_: EnvironmentType,
    /// What happens to the namespace when this resource is deleted.
    #[serde(default)]
    pub deletion_policy: DeletionPolicy,
    /// Production protection rules (approvals, windows).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub protection: Option<Protection>,
    /// Resource quota applied to the namespace.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub quota: Option<Quota>,
    /// Preview-only: time-to-live settings.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ttl: Option<Ttl>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum EnvironmentType {
    #[default]
    Standard,
    Production,
    Preview,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub enum DeletionPolicy {
    /// Keep the namespace and its data; only remove Kuben ownership.
    #[default]
    Retain,
    /// Delete the namespace after the grace period.
    Delete,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Protection {
    /// Distinct approvers required to promote a release here.
    #[serde(default)]
    pub require_approvals: u32,
    /// Grace period before a soft-deleted environment is purged, e.g. `168h`.

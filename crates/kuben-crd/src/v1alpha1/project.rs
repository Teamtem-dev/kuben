//! `Project` — cluster-scoped grouping of environments. Owned by an Org
//! (label `kuben.dev/org`), referenced from SQL only by `uid` (ADR-015).

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::Condition;

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "Project",
    status = "ProjectStatus",
    shortname = "kproj",
    printcolumn = r#"{"name":"Display","type":"string","jsonPath":".spec.displayName"}"#,
    printcolumn = r#"{"name":"Environments","type":"integer","jsonPath":".status.environments"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct ProjectSpec {
    /// Human readable name.
    pub display_name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,

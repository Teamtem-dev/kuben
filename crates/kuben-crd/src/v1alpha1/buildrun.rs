//! `BuildRun` — one image build. Doubles as a durable queue: the controller
//! dispatches at most `maxConcurrent` builds per org (Master Blueprint §4.4).
//! Build pods never receive a ServiceAccount token (Invariant I-7).

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::{app::BuildStrategy, common::Condition};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "BuildRun",
    namespaced,
    status = "BuildRunStatus",
    shortname = "kbr",
    printcolumn = r#"{"name":"App","type":"string","jsonPath":".spec.app"}"#,
    printcolumn = r#"{"name":"Ref","type":"string","jsonPath":".spec.gitRef"}"#,
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct BuildRunSpec {
    /// `namespace/name` of the App being built.
    pub app: String,
    pub repo: String,
    /// Commit SHA (preferred) or ref.
    pub git_ref: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
    #[serde(default)]
    pub strategy: BuildStrategy,
    /// Target image (tag form); the resulting digest lands in status.
    pub image: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct BuildRunStatus {

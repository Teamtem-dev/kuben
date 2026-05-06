//! `Release` — immutable record of *what* was deployed: image digest plus a
//! snapshot of the non-secret App spec and Secret **references**.

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::{app::AppSpec, common::Condition};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "Release",
    namespaced,
    status = "ReleaseStatus",
    shortname = "krel",
    printcolumn = r#"{"name":"App","type":"string","jsonPath":".spec.app"}"#,
    printcolumn = r#"{"name":"Digest","type":"string","jsonPath":".spec.imageDigest"}"#,
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct ReleaseSpec {
    /// Name of the App this release belongs to.
    pub app: String,
    /// Fully qualified image reference pinned by digest (`repo@sha256:...`).
    pub image_digest: String,

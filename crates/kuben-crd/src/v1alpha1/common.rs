//! Types shared by several CRDs.

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// kstatus-style condition. Mirrors `metav1.Condition` without pulling the
/// `schemars` feature of `k8s-openapi`.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct Condition {
    #[serde(rename = "type")]
    pub type_: String,
    /// `"True"`, `"False"` or `"Unknown"`.
    pub status: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,

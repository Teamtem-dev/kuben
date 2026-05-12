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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_transition_time: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_generation: Option<i64>,
}

impl Condition {
    #[must_use]
    pub fn new(type_: impl Into<String>, status: bool, reason: impl Into<String>) -> Self {
        Self {
            type_: type_.into(),
            status: if status { "True".into() } else { "False".into() },
            reason: Some(reason.into()),
            message: None,
            last_transition_time: None,
            observed_generation: None,
        }
    }
}

/// Standard condition types used across Kuben resources.
pub mod condition {
    pub const READY: &str = "Ready";
    pub const PROGRESSING: &str = "Progressing";
    pub const DEGRADED: &str = "Degraded";
}


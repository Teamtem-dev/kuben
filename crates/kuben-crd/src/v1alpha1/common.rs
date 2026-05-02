//! Types shared by several CRDs.

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// kstatus-style condition. Mirrors `metav1.Condition` without pulling the
/// `schemars` feature of `k8s-openapi`.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]

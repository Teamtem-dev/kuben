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

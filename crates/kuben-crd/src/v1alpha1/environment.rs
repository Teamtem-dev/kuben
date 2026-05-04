//! `Environment` — cluster-scoped; owns exactly one Namespace
//! (`kb-<project>-<env>`) with PSA, ResourceQuota and NetworkPolicy.
//! Deletion is soft by default (ADR-018).

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::Condition;

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]

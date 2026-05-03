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

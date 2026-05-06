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

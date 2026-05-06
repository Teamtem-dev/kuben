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

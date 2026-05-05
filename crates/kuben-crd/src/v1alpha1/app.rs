//! `App` — the unit of deployment. Namespaced (lives in its Environment's
//! namespace). Secrets are referenced, never inlined (ADR: Release snapshots
//! hold Secret references, not values).

use std::collections::BTreeMap;

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use super::common::{Condition, KeyRef};

#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kuben.dev",
    version = "v1alpha1",
    kind = "App",
    namespaced,
    status = "AppStatus",

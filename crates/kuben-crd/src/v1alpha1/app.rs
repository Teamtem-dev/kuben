//! `App` — the unit of deployment. Namespaced (lives in its Environment's
//! namespace). Secrets are referenced, never inlined (ADR: Release snapshots
//! hold Secret references, not values).

use std::collections::BTreeMap;

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

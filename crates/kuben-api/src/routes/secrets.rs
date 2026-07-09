//! Secrets of an environment. Values are write-only through the API: they
//! can be set and replaced, never read back. Only Secrets Kuben created are
//! listed or touched (service-account tokens etc. are invisible).

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::{ByteString, api::core::v1::Secret};
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, ListParams, ObjectMeta, Patch, PatchParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{FIELD_MANAGER, labels};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Upper bound for the sum of all values of one secret.
const MAX_SECRET_BYTES: usize = 256 * 1024;

#[derive(Debug, Serialize, ToSchema)]
pub struct SecretDto {
    pub name: String,
    /// Key names only; values are never returned.
    pub keys: Vec<String>,
    pub created_at: Option<String>,
}

impl From<&Secret> for SecretDto {
    fn from(s: &Secret) -> Self {
        Self {
            name: s.name_any(),
            keys: s
                .data
                .as_ref()
                .map(|d| d.keys().cloned().collect())
                .unwrap_or_default(),
            created_at: s.creation_timestamp().map(|t| t.0.to_string()),
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct PutSecret {
    /// Key → UTF-8 value. Replaces the whole secret.
    pub data: BTreeMap<String, String>,
}

fn is_managed(s: &Secret) -> bool {
    s.labels()
        .get(labels::MANAGED_BY)
        .is_some_and(|v| v == labels::MANAGER)
}

/// List the secrets of an environment (names and keys).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/secrets", operation_id = "listSecrets",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<SecretDto>), (status = 503, body = crate::error::Problem))
)]
pub async fn list(

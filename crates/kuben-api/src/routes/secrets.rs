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
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<SecretDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _proof = authz.require(&state, Perm::SecretRead, &e.chain())?;
    let api = Api::<Secret>::namespaced(scope::cluster(&state)?, &e.view.namespace);
    let list = api
        .list(&ListParams::default().labels(labels::MANAGED_SELECTOR))
        .await
        .map_err(|err| scope::kube_error(err, "secrets"))?;
    Ok(Json(list.items.iter().map(SecretDto::from).collect()))
}

/// Create or replace a secret.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/secrets/{secret}", operation_id = "putSecret",
    tag = "secrets",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("secret" = String, Path, description = "Secret name"),
    ),
    request_body = PutSecret,
    responses(
        (status = 200, body = SecretDto),
        (status = 409, description = "A secret with this name exists and is not managed by Kuben", body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, secret)): Path<(String, String, String)>,
    Json(body): Json<PutSecret>,
) -> ApiResult<Json<SecretDto>> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _proof = authz.require(&state, Perm::SecretWrite, &e.chain())?;
    validate::dns_label("secret name", &secret, 63)?;
    if body.data.is_empty() {
        return Err(Error::Validation("a secret needs at least one key".into()).into());
    }
    let mut total = 0usize;
    for (k, v) in &body.data {
        validate::secret_key(k)?;
        total = total.saturating_add(v.len());
    }
    if total > MAX_SECRET_BYTES {
        return Err(Error::Validation(format!("secret values exceed {MAX_SECRET_BYTES} bytes")).into());
    }
    let api = Api::<Secret>::namespaced(scope::cluster(&state)?, &e.view.namespace);
    if let Some(existing) = api
        .get_opt(&secret)
        .await
        .map_err(|err| scope::kube_error(err, &secret))?
        && !is_managed(&existing)
    {
        return Err(Error::Conflict(format!("secret `{secret}` exists and is not managed by Kuben")).into());
    }
    let object = Secret {
        metadata: ObjectMeta {
            name: Some(secret.clone()),
            namespace: Some(e.view.namespace.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ENVIRONMENT.to_owned(), e.view.name.clone()),
            ])),
            ..ObjectMeta::default()
        },
        type_: Some("Opaque".into()),
        // `data` (not `stringData`): server-side apply then owns exactly these
        // keys, so keys removed from the request are removed from the Secret.
        data: Some(

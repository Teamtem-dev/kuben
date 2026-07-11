//! Environments of a project. Writes go to the `Environment` CRD (the
//! controller provisions the namespace); reads come from the projection.

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference;
use kube::{
    Api,
    api::{DeleteParams, ObjectMeta, PostParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::{DeletionPolicy, Environment, EnvironmentSpec, EnvironmentType, Protection, Quota, labels};
use kuben_platform::{controller::resources::namespace_name, projection::EnvironmentView};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Grace period before a deleted production environment is purged.
pub const PRODUCTION_DELETION_GRACE: &str = "168h";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum EnvType {
    #[default]
    Standard,
    Production,
    Preview,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct EnvironmentDto {
    /// Short name used in URLs, e.g. `prod`.
    pub name: String,
    /// Kubernetes object name, e.g. `shop-prod`.
    pub resource_name: String,
    pub project: String,
    /// `standard`, `production` or `preview`.
    pub env_type: String,
    pub namespace: String,
    /// `Pending`, `Ready`, `Terminating` or `Degraded`.
    pub phase: Option<String>,
    pub ready: bool,
    pub message: Option<String>,
    pub deleting: bool,
    /// When a soft-deleted environment will be purged.
    pub deletion_scheduled_at: Option<String>,
    pub created_at: Option<String>,
}

impl EnvironmentDto {
    #[must_use]
    pub fn from_view(v: &EnvironmentView) -> Self {
        Self {
            name: scope::environment_short_name(&v.project, &v.name).to_owned(),
            resource_name: v.name.clone(),
            project: v.project.clone(),
            env_type: v.env_type.to_owned(),
            namespace: v.namespace.clone(),
            phase: v.phase.clone(),
            ready: v.ready,
            message: v.message.clone(),
            deleting: v.deleting,
            deletion_scheduled_at: v.deletion_scheduled_at.clone(),
            created_at: v.created_at.clone(),
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct QuotaInput {
    #[schema(example = "4")]
    pub cpu: Option<String>,
    #[schema(example = "8Gi")]
    pub memory: Option<String>,
    #[schema(example = 50)]
    pub pods: Option<u32>,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateEnvironment {
    /// Short name, e.g. `prod` (the object is named `<project>-<name>`).
    #[schema(example = "prod")]
    pub name: String,
    #[serde(default)]
    pub env_type: EnvType,
    pub quota: Option<QuotaInput>,
}

/// List a project's environments.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments", operation_id = "listEnvironments",
    tag = "environments",
    params(("project" = String, Path, description = "Project name")),
    responses(
        (status = 200, body = Vec<EnvironmentDto>),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<Vec<EnvironmentDto>>> {
    let p = scope::project(&state, &authz, &project)?;
    let _proof = authz.require(&state, Perm::EnvRead, &p.chain())?;
    let items = state
        .projections
        .environments()
        .iter()
        .filter(|e| e.project == p.view.name)
        .map(|e| EnvironmentDto::from_view(e))
        .collect();
    Ok(Json(items))
}

/// One environment.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}", operation_id = "getEnvironment",
    tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = EnvironmentDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,

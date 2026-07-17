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
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<EnvironmentDto>> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _proof = authz.require(&state, Perm::EnvRead, &e.chain())?;
    Ok(Json(EnvironmentDto::from_view(&e.view)))
}

/// Create an environment (the controller provisions its namespace).
#[utoipa::path(
    post,
    path = "/projects/{project}/environments", operation_id = "createEnvironment",
    tag = "environments",
    params(("project" = String, Path, description = "Project name")),
    request_body = CreateEnvironment,
    responses(
        (status = 201, body = EnvironmentDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, description = "No cluster configured", body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Json(body): Json<CreateEnvironment>,
) -> ApiResult<(StatusCode, Json<EnvironmentDto>)> {
    let p = scope::project(&state, &authz, &project)?;
    let _proof = authz.require(&state, Perm::EnvWrite, &p.chain())?;
    validate::dns_label("name", &body.name, 20)?;
    let name = scope::environment_resource_name(&p.view.name, &body.name);
    if namespace_name(&name).len() > 63 {
        return Err(Error::Validation("project and environment names are too long together".into()).into());
    }
    if let Some(q) = &body.quota {
        if let Some(cpu) = &q.cpu {
            validate::quantity("quota.cpu", cpu)?;
        }
        if let Some(mem) = &q.memory {
            validate::quantity("quota.memory", mem)?;
        }
    }
    if p.view.deleting {
        return Err(Error::Conflict(format!("project `{}` is being deleted", p.view.name)).into());
    }
    let client = scope::cluster(&state)?;

    let (type_, protection) = match body.env_type {
        EnvType::Standard => (EnvironmentType::Standard, None),
        EnvType::Preview => (EnvironmentType::Preview, None),
        // Deleting production is a soft delete with a 7-day grace period.
        EnvType::Production => (
            EnvironmentType::Production,
            Some(Protection {
                require_approvals: 0,
                deletion_grace: PRODUCTION_DELETION_GRACE.into(),
            }),
        ),
    };
    let env = Environment {
        metadata: ObjectMeta {
            name: Some(name.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ORG.to_owned(), p.org.to_string()),
                (labels::PROJECT.to_owned(), p.view.name.clone()),
            ])),
            // Deleting the project deletes its environments (each one still
            // honours its own deletion policy through the finalizer).
            owner_references: Some(vec![OwnerReference {
                api_version: "kuben.dev/v1alpha1".into(),
                kind: "Project".into(),
                name: p.view.name.clone(),
                uid: p.uid.to_string(),
                ..OwnerReference::default()
            }]),
            ..ObjectMeta::default()
        },
        spec: EnvironmentSpec {
            project: p.view.name.clone(),
            type_,
            deletion_policy: DeletionPolicy::Delete,
            protection,
            quota: body.quota.map(|q| Quota {
                cpu: q.cpu,
                memory: q.memory,
                pods: q.pods,
            }),
            ttl: None,
        },
        status: None,
    };
    let created = Api::<Environment>::all(client)
        .create(&PostParams::default(), &env)
        .await
        .map_err(|e| scope::kube_error(e, &name))?;
    Ok((
        StatusCode::CREATED,
        Json(EnvironmentDto::from_view(&EnvironmentView::from(&created))),
    ))
}


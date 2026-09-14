//! Environments of a project, on the SQL model (ADR-032). SQL holds them
//! and their placement; the materializer writes the `Environment` resource
//! right away (its controller provisions the namespace) and removes it on
//! deletion. The projection adds live status.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::Quota;
use kuben_platform::{controller::resources::namespace_name, projection::EnvironmentView};
use kuben_store::repo::{ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, EnvironmentKind, EnvironmentRecord, Subject};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{request, scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Grace period before a deleted production environment is purged.
pub const PRODUCTION_DELETION_GRACE: &str = "168h";
/// The cluster every placement is on until multi-cluster (M1.9 and later).
const PRIMARY_CLUSTER: &str = "primary";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum EnvType {
    #[default]
    Standard,
    Production,
    Preview,
}

impl From<EnvType> for EnvironmentKind {
    fn from(t: EnvType) -> Self {
        match t {
            EnvType::Standard => Self::Standard,
            EnvType::Production => Self::Production,
            EnvType::Preview => Self::Preview,
        }
    }
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
    pub fn of(project: &str, e: &EnvironmentRecord, view: Option<&EnvironmentView>) -> Self {
        let resource_name = scope::environment_resource_name(project, &e.slug);
        let phase = view
            .and_then(|v| v.phase.clone())
            .or_else(|| Some(if e.deleting { "Terminating" } else { "Pending" }.to_owned()));
        Self {
            name: e.slug.clone(),
            namespace: e
                .namespace
                .clone()
                .unwrap_or_else(|| namespace_name(&resource_name)),
            resource_name,
            project: project.to_owned(),
            env_type: e.env_type.clone(),
            phase,
            ready: view.is_some_and(|v| v.ready) && !e.deleting,
            message: view.and_then(|v| v.message.clone()),
            deleting: e.deleting,
            deletion_scheduled_at: view.and_then(|v| v.deletion_scheduled_at.clone()),
            created_at: Some(request::timestamp(e.created_at)),
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
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let items = tenant
        .environments(p.id())
        .await?
        .iter()
        .map(|e| {
            let view = state
                .projections
                .environment(&scope::environment_resource_name(p.slug(), &e.slug));
            EnvironmentDto::of(p.slug(), e, view.as_deref())
        })
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
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &e.chain())?;
    Ok(Json(EnvironmentDto::of(
        e.project.slug(),
        &e.env,
        e.view.as_deref(),
    )))
}

fn quota(input: Option<QuotaInput>) -> ApiResult<Option<serde_json::Value>> {
    let Some(q) = input else {
        return Ok(None);
    };
    if let Some(cpu) = &q.cpu {
        validate::quantity("quota.cpu", cpu)?;
    }
    if let Some(mem) = &q.memory {
        validate::quantity("quota.memory", mem)?;
    }
    let quota = Quota {
        cpu: q.cpu,
        memory: q.memory,
        pods: q.pods,
    };
    Ok(Some(serde_json::to_value(quota).map_err(Error::internal)?))
}

/// Create an environment: its namespace follows.
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
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Json(body): Json<CreateEnvironment>,
) -> ApiResult<(StatusCode, Json<EnvironmentDto>)> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::EnvWrite, &p.chain())?;
    validate::dns_label("name", &body.name, 20)?;
    let resource = scope::environment_resource_name(p.slug(), &body.name);
    let namespace = namespace_name(&resource);
    if namespace.len() > 63 {
        return Err(Error::Validation("project and environment names are too long together".into()).into());
    }
    let quota = quota(body.quota)?;
    if p.project.deleting {
        return Err(Error::Conflict(format!("project `{}` is being deleted", p.slug())).into());
    }
    // Environment resources are cluster-wide.
    let taken = || Error::Conflict(format!("environment `{}` already exists", body.name));
    if state
        .projections
        .environment(&resource)
        .is_some_and(|v| v.org.as_deref() != Some(p.org.to_string().as_str()))
    {
        return Err(taken().into());
    }
    let (_, actor) = request::actor(&authz);
    let what = format!("environment `{}`", body.name);
    let mut tenant = state.store.tenant(p.org).await?;
    let id = tenant
        .create_environment_typed(
            p.id(),
            &body.name,
            &body.name,
            body.env_type.into(),
            quota.as_ref(),
        )
        .await
        .map_err(|e| request::duplicate(e, &what))?;
    let cluster = tenant.ensure_cluster(PRIMARY_CLUSTER).await?;
    tenant
        .create_placement(p.id(), id, cluster, &namespace)
        .await
        .map_err(|e| request::duplicate(e, &what))?;
    tenant
        .request(
            ENVIRONMENT_APPLY,
            Subject::environment(p.id(), id),
            &actor,
            request::audit(&authz, ENVIRONMENT_APPLY, "environment", resource),
        )
        .await?;
    let env = tenant.environment(p.id(), &body.name).await?.ok_or_else(taken)?;
    tenant.commit().await?;
    Ok((
        StatusCode::CREATED,
        Json(EnvironmentDto::of(p.slug(), &env, None)),
    ))
}

/// Delete an environment. Production environments need the
/// `env-delete-protected` permission and are purged after a grace period.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}", operation_id = "deleteEnvironment",
    tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses(
        (status = 202, description = "Deletion accepted"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let perm = if e.env.env_type == "production" {
        Perm::EnvDeleteProtected
    } else {
        Perm::EnvWrite
    };
    let _proof = authz.require(&state, perm, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    if !tenant.mark_environment_deleting(e.id()).await? {
        return Err(Error::Conflict(format!("environment `{environment}` is being deleted")).into());
    }
    let (_, actor) = request::actor(&authz);
    tenant
        .request(
            ENVIRONMENT_DELETE,
            Subject::environment(e.project.id(), e.id()),
            &actor,
            request::audit(&authz, ENVIRONMENT_DELETE, "environment", e.resource_name()),
        )
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::ACCEPTED)
}

//! A project's preview settings and previews (M5.1).

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use kuben_core::{Error, perm::Perm, preview, time::now_ms};
use kuben_store::repo::{CloseReason, Preview, PreviewPolicy};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use super::{request, scope};
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    state::ApiState,
};

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PreviewPolicyDto {
    pub enabled: bool,
    /// The environment whose Git-built apps previews copy.
    pub source_environment: Option<String>,
    pub ttl_hours: u32,
    pub max_active: u32,
    /// Pull requests from forks get (untrusted) previews too.
    pub allow_forks: bool,
    pub updated_by: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PutPreviewPolicy {
    pub enabled: bool,
    #[schema(example = "staging")]
    pub source_environment: String,
    #[serde(default = "default_ttl")]
    pub ttl_hours: u32,
    #[serde(default = "default_max")]
    pub max_active: u32,
    #[serde(default)]
    pub allow_forks: bool,
}

const fn default_ttl() -> u32 {
    72
}

const fn default_max() -> u32 {
    10
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PreviewDto {
    /// The preview's environment (`pr<n>-<epoch>`).
    pub environment: String,
    pub repository: String,
    pub pull_request: i64,
    pub epoch: i64,
    pub head_repository: String,
    pub branch: String,
    pub commit: String,
    /// False for a fork's preview: it gets no secrets.
    pub trusted: bool,
    /// `active` or `closed`.
    pub state: String,
    pub auto_delete: bool,
    pub expires_at: String,
    /// Seconds until it expires (0 once it has).
    pub remaining_seconds: i64,
    pub created_at: String,
    pub closed_at: Option<String>,
    /// `closed`, `expired`, `manual` or `deleted`.
    pub close_reason: Option<String>,
}

impl PreviewDto {
    fn of(p: Preview, now: i64) -> Self {
        Self {
            remaining_seconds: if p.active() {
                (p.expires_at - now).max(0) / 1000
            } else {
                0
            },
            environment: p.environment,
            repository: p.repository,
            pull_request: p.pr_number,
            epoch: p.preview_epoch,
            head_repository: p.head_repository,
            branch: p.branch,
            commit: p.commit_sha,
            trusted: p.trusted,
            state: p.state,
            auto_delete: p.auto_delete,
            expires_at: request::timestamp(p.expires_at),
            created_at: request::timestamp(p.created_at),
            closed_at: p.closed_at.map(request::timestamp),
            close_reason: p.close_reason,
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct PreviewQuery {
    /// Include closed previews.
    #[serde(default)]
    pub all: bool,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ExtendPreview {
    /// Hours added to the lifetime (at most 30 days from now in total).
    pub hours: u32,
    /// Keep the preview past its expiry until someone destroys it.
    #[serde(default)]
    pub keep: bool,
}

/// A project's preview settings.
#[utoipa::path(
    get, path = "/projects/{project}/previews/policy", operation_id = "getPreviewPolicy", tag = "previews",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 200, body = PreviewPolicyDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get_policy(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<PreviewPolicyDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let policy = tenant.preview_policy(p.id()).await?;
    let environments = tenant.environments(p.id()).await?;
    let name = |id| environments.iter().find(|e| e.id == id).map(|e| e.slug.clone());
    Ok(Json(match policy {
        Some(policy) => PreviewPolicyDto {
            enabled: policy.enabled,
            source_environment: name(policy.source_environment),
            ttl_hours: policy.ttl_hours,
            max_active: policy.max_active,
            allow_forks: policy.allow_forks,
            updated_by: Some(policy.updated_by),
            updated_at: Some(request::timestamp(policy.updated_at)),
        },
        None => PreviewPolicyDto {
            enabled: false,
            source_environment: None,
            ttl_hours: default_ttl(),
            max_active: default_max(),
            allow_forks: false,
            updated_by: None,
            updated_at: None,
        },
    }))
}

/// Set a project's preview settings.
#[utoipa::path(
    put, path = "/projects/{project}/previews/policy", operation_id = "putPreviewPolicy", tag = "previews",
    params(("project" = String, Path, description = "Project name")),
    request_body = PutPreviewPolicy,
    responses(
        (status = 200, body = PreviewPolicyDto),
        (status = 403, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put_policy(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Json(body): Json<PutPreviewPolicy>,
) -> ApiResult<Json<PreviewPolicyDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    if body.ttl_hours == 0 || body.ttl_hours > preview::MAX_TTL_HOURS {
        return Err(Error::Validation(format!("ttlHours must be 1 to {}", preview::MAX_TTL_HOURS)).into());
    }
    if !(1..=100).contains(&body.max_active) {
        return Err(Error::Validation("maxActive must be 1 to 100".into()).into());
    }
    let mut tenant = state.store.tenant(p.org).await?;
    let source = tenant
        .environment(p.id(), &body.source_environment)
        .await?
        .filter(|e| !e.deleting)
        .ok_or_else(|| Error::Validation(format!("no environment `{}`", body.source_environment)))?;
    if source.env_type == "preview" {
        return Err(Error::Validation("a preview cannot be the source of previews".into()).into());
    }
    let (_, actor) = request::actor(&authz);
    tenant
        .set_preview_policy(&PreviewPolicy {
            project: p.id(),
            enabled: body.enabled,
            source_environment: source.id,
            ttl_hours: body.ttl_hours,
            max_active: body.max_active,
            allow_forks: body.allow_forks,
            updated_by: actor.clone(),
            updated_at: now_ms(),
        })
        .await?;
    tenant
        .append_audit(request::audit(
            &authz,
            "previews.policy.updated",
            "project",
            project.clone(),
        ))
        .await?;
    tenant.commit().await?;
    Ok(Json(PreviewPolicyDto {
        enabled: body.enabled,
        source_environment: Some(source.slug),
        ttl_hours: body.ttl_hours,
        max_active: body.max_active,
        allow_forks: body.allow_forks,
        updated_by: Some(actor),
        updated_at: Some(request::timestamp(now_ms())),
    }))
}

/// A project's previews, newest first.
#[utoipa::path(
    get, path = "/projects/{project}/previews", operation_id = "listPreviews", tag = "previews",
    params(("project" = String, Path, description = "Project name"), PreviewQuery),
    responses((status = 200, body = Vec<PreviewDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Query(q): Query<PreviewQuery>,
) -> ApiResult<Json<Vec<PreviewDto>>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let now = now_ms();
    let found = tenant.previews(p.id(), q.all).await?;
    Ok(Json(found.into_iter().map(|x| PreviewDto::of(x, now)).collect()))
}

async fn active(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    environment: &str,
) -> Result<(scope::EnvScope, Preview), ApiError> {
    let e = scope::environment(state, authz, project, environment).await?;
    let _proof = authz.require(state, Perm::EnvWrite, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let preview = tenant
        .preview_of(e.id())
        .await?
        .filter(Preview::active)
        .ok_or_else(|| Error::NotFound(format!("active preview `{environment}`")))?;
    Ok((e, preview))
}

/// Give a preview more time.
#[utoipa::path(
    post, path = "/projects/{project}/previews/{environment}/extend", operation_id = "extendPreview",
    tag = "previews",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "The preview's environment"),
    ),
    request_body = ExtendPreview,
    responses((status = 200, body = PreviewDto), (status = 404, body = crate::error::Problem))
)]
pub async fn extend(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<ExtendPreview>,
) -> ApiResult<Json<PreviewDto>> {
    if body.hours == 0 || body.hours > preview::MAX_TTL_HOURS {
        return Err(Error::Validation(format!("hours must be 1 to {}", preview::MAX_TTL_HOURS)).into());
    }
    let (e, current) = active(&state, &authz, &project, &environment).await?;
    let now = now_ms();
    let expires_at = preview::extend(now, current.expires_at, body.hours);
    let mut tenant = state.store.tenant(e.project.org).await?;
    tenant.extend_preview(e.id(), expires_at, !body.keep).await?;
    let mut audit = request::audit(&authz, "preview.extended", "environment", e.resource_name());
    audit.data = Some(serde_json::json!({ "hours": body.hours, "keep": body.keep }));
    tenant.append_audit(audit).await?;
    let updated = tenant
        .preview_of(e.id())
        .await?
        .ok_or_else(|| Error::Internal("the preview is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(PreviewDto::of(updated, now)))
}

/// Destroy a preview now: its environment is deleted.
#[utoipa::path(
    delete, path = "/projects/{project}/previews/{environment}", operation_id = "destroyPreview",
    tag = "previews",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "The preview's environment"),
    ),
    responses((status = 202, description = "Deletion accepted"), (status = 404, body = crate::error::Problem))
)]
pub async fn destroy(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<StatusCode> {
    let (e, current) = active(&state, &authz, &project, &environment).await?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let audit = request::audit(&authz, "preview.destroyed", "environment", e.resource_name());
    crate::previews::destroy(&mut tenant, &current, CloseReason::Manual, now_ms(), audit).await?;
    tenant.commit().await?;
    Ok(StatusCode::ACCEPTED)
}

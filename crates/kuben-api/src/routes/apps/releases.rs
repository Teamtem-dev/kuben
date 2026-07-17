//! Release history and rollback (scenario 5).

use std::collections::HashMap;

use axum::{
    Json,
    extract::{Path, State},
};
use kube::api::PostParams;
use kuben_core::{Error, perm::Perm};
use kuben_crd::AppSpec;
use kuben_platform::projection::AppView;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{AppDto, app_api, app_dto, record_release, spec::validate_spec};
use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

const RELEASE_PAGE: i64 = 50;

#[derive(Debug, Serialize, ToSchema)]
pub struct ReleaseDto {
    pub revision: i64,
    pub image: Option<String>,
    /// `create`, `deploy`, `config`, `rollback`, `promote` or `template`.
    pub reason: String,
    pub note: Option<String>,
    /// Email of whoever made the change.
    pub actor: Option<String>,
    pub created_at: i64,
    /// The newest revision (what should be running).
    pub current: bool,
}

/// Release history, newest first (50 revisions).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/releases", operation_id = "listReleases",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = Vec<ReleaseDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn releases(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<Vec<ReleaseDto>>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let rows = state
        .store
        .list_releases(&a.view.namespace, &a.view.name, RELEASE_PAGE)
        .await?;
    let emails: HashMap<String, String> = state
        .store
        .list_users()
        .await?
        .into_iter()
        .map(|u| (u.id.to_string(), u.email))
        .collect();
    Ok(Json(
        rows.into_iter()
            .enumerate()
            .map(|(i, r)| ReleaseDto {
                revision: r.revision,
                image: r.image,
                reason: r.reason,
                note: r.note,
                actor: r.actor_id.map(|id| emails.get(&id).cloned().unwrap_or(id)),
                created_at: r.created_at,
                current: i == 0,
            })
            .collect(),
    ))
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct Rollback {
    /// Revision to restore.
    pub revision: i64,
}

/// What a rollback restores: image, processes and env of the revision;
/// domains and volumes stay as they are now (data layout never moves back).
#[must_use]
pub fn rollback_spec(current: &AppSpec, revision: AppSpec) -> AppSpec {
    AppSpec {
        source: revision.source,
        runtime: revision.runtime,
        env: revision.env,
        domains: current.domains.clone(),
        volumes: current.volumes.clone(),
    }
}

/// Roll back to an earlier revision (recorded as a new revision).
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/rollback", operation_id = "rollbackApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = Rollback,
    responses(
        (status = 200, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn rollback(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<Rollback>,
) -> ApiResult<Json<AppDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let release = state
        .store
        .find_release(&a.view.namespace, &a.view.name, body.revision)
        .await?
        .ok_or_else(|| Error::NotFound(format!("revision {}", body.revision)))?;
    let restored: AppSpec = serde_json::from_value(release.spec)
        .map_err(|e| Error::Validation(format!("revision {} cannot be restored: {e}", body.revision)))?;
    let api = app_api(&state, &a.env)?;
    let mut live = api
        .get(&a.view.name)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    live.spec = rollback_spec(&live.spec, restored);
    validate_spec(&live.spec)?;
    let updated = api
        .replace(&a.view.name, &PostParams::default(), &live)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    record_release(
        &state,
        &authz,
        a.env.project.org,
        &a.view.namespace,
        &a.view.name,
        &updated.spec,

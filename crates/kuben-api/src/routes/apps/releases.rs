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

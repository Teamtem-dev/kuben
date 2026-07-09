//! CRUD on the `App` CRD, rolling restart and recent logs.

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use k8s_openapi::api::core::v1::{PersistentVolumeClaim, Pod};
use kube::{
    Api, ResourceExt,
    api::{DeleteParams, ListParams, LogParams, Patch, PatchParams, PostParams},
};
use kuben_core::perm::Perm;
use kuben_crd::labels;
use kuben_platform::{
    controller::resources::{RESTARTED_AT, RETAIN},
    projection::AppView,
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use utoipa::{IntoParams, ToSchema};

use super::{
    AppDetail, AppDto, PodDto, app_api, app_dto, create_app, record_release,
    spec::{
        CreateApp, UpdateApp, apply_update, ensure_domains_free, from_crd_env, spec_from_create,
        validate_spec,
    },
    spec_json,
};
use crate::{
    authz::Authz,
    error::ApiResult,
    routes::{scope, validate},
    state::ApiState,
};

const MAX_LOG_PODS: usize = 10;

/// List the apps of an environment.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps", operation_id = "listApps",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<AppDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<AppDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _proof = authz.require(&state, Perm::AppRead, &e.chain())?;
    let items = state
        .projections
        .apps()
        .iter()
        .filter(|a| a.namespace == e.view.namespace)
        .map(|a| AppDto::from_view(a, &e.project.view.name, e.short_name()))
        .collect();
    Ok(Json(items))
}

/// Deploy a new app from a container image.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps", operation_id = "createApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    request_body = CreateApp,
    responses(
        (status = 201, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<CreateApp>,
) -> ApiResult<(StatusCode, Json<AppDto>)> {
    let e = scope::environment(&state, &authz, &project, &environment)?;
    let _proof = authz.require(&state, Perm::AppWrite, &e.chain())?;
    validate::dns_label("name", &body.name, 40)?;
    let spec = spec_from_create(&body)?;
    let dto = create_app(&state, &authz, &e, &body.name, spec, "create", None).await?;
    Ok((StatusCode::CREATED, Json(dto)))
}

/// App detail with pods. Plain env values are included only for callers
/// holding `secret-read`.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}", operation_id = "getApp",

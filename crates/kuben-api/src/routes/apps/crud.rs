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
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = AppDetail), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<AppDetail>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let with_values = authz.require(&state, Perm::SecretRead, &a.chain()).is_ok();
    let mut dto = app_dto(&a, &a.view);
    if let Ok(api) = app_api(&state, &a.env)
        && let Ok(live) = api.get(&a.view.name).await
    {
        dto.env = live
            .spec
            .env
            .iter()
            .map(|e| from_crd_env(e, with_values))
            .collect();
    }
    let pods = state
        .projections
        .pods_of_app(&a.view.namespace, &a.view.name)
        .iter()
        .map(|p| PodDto::from(&**p))
        .collect();
    Ok(Json(AppDetail { app: dto, pods }))
}

/// Update an app (image changes require `app-deploy`). Every change is a
/// new release revision.
#[utoipa::path(
    patch,
    path = "/projects/{project}/environments/{environment}/apps/{app}", operation_id = "updateApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = UpdateApp,
    responses(
        (status = 200, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn update(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<UpdateApp>,
) -> ApiResult<Json<AppDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let deploys = body.image.is_some();
    let perm = if deploys { Perm::AppDeploy } else { Perm::AppWrite };
    let _proof = authz.require(&state, perm, &a.chain())?;
    let api = app_api(&state, &a.env)?;
    let mut live = api
        .get(&a.view.name)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    let before = spec_json(&live.spec);
    apply_update(&mut live.spec, body)?;
    validate_spec(&live.spec)?;
    ensure_domains_free(&state, &a.view.namespace, &a.view.name, &live.spec)?;
    // `replace` carries the resourceVersion we read: a concurrent edit is a 409.
    let updated = api
        .replace(&a.view.name, &PostParams::default(), &live)
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    if spec_json(&updated.spec) != before {
        let reason = if deploys { "deploy" } else { "config" };
        record_release(
            &state,
            &authz,
            a.env.project.org,
            &a.view.namespace,
            &a.view.name,
            &updated.spec,
            reason,
            None,
        )
        .await;
    }
    Ok(Json(app_dto(&a, &AppView::from(&updated))))
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct DeleteAppQuery {
    /// Also delete the app's persistent volumes. Irreversible.
    pub delete_volumes: Option<bool>,
}

/// Delete an app and everything it owns. Volumes are kept unless

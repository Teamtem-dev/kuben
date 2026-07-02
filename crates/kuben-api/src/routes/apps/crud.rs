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

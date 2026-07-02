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

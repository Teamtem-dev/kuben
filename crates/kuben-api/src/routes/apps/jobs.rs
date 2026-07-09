//! Scheduled runs: "run now" for a scheduled process (scenario 7).

use std::time::{SystemTime, UNIX_EPOCH};

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use k8s_openapi::api::batch::v1::{CronJob, Job};
use kube::{Api, api::PostParams};
use kuben_core::{Error, perm::Perm};
use kuben_platform::controller::resources;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

#[derive(Debug, Default, Deserialize, ToSchema)]
pub struct RunJob {
    /// Scheduled process to run (default: the first one).
    pub process: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct JobStarted {
    pub job: String,
}


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

/// Name for a manual run, within the 63-character limit of Job names.
#[must_use]
pub fn manual_job_name(cron: &str, unix_secs: u64) -> String {
    let suffix = format!("-run-{unix_secs}");
    let keep = 63_usize.saturating_sub(suffix.len());
    let base: String = cron.chars().take(keep).collect();
    format!("{}{suffix}", base.trim_end_matches('-'))
}

/// Run a scheduled process now (a Job from its CronJob template).
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/run", operation_id = "runApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = RunJob,
    responses(
        (status = 202, body = JobStarted),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn run(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<RunJob>,
) -> ApiResult<(StatusCode, Json<JobStarted>)> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let process = match body.process {
        Some(p) => p,
        None => a
            .view
            .processes
            .iter()
            .find(|p| p.schedule.is_some())
            .map(|p| p.name.clone())
            .ok_or_else(|| Error::Validation("this app has no scheduled process".into()))?,
    };
    let client = scope::cluster(&state)?;
    let cron_name = resources::workload_name(&a.view.name, &process);
    let cron = Api::<CronJob>::namespaced(client.clone(), &a.view.namespace)
        .get(&cron_name)
        .await
        .map_err(|e| scope::kube_error(e, &cron_name))?;
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    let name = manual_job_name(&cron_name, now);
    let job =
        resources::job_from_cron(&cron, &name).ok_or_else(|| Error::internal("cron job has no template"))?;
    Api::<Job>::namespaced(client, &a.view.namespace)
        .create(&PostParams::default(), &job)

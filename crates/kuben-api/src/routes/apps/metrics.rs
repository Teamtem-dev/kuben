//! An app's CPU and memory (M5.5): the last hour from this replica's live
//! window, the last week from hourly rollups. Missing data is reported as
//! unavailable, never as zero.

use axum::{
    Json,
    extract::{Path, Query, State},
};
use kuben_core::{Error, perm::Perm, time::now_ms};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

const HOUR_MS: i64 = 3_600_000;

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct MetricsQuery {
    /// `1h` (live samples) or `7d` (hourly averages and peaks).
    #[serde(default)]
    pub window: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct MetricPoint {
    /// RFC 3339.
    pub at: String,
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    /// The hour's peaks (`7d` only).
    pub cpu_max: Option<u64>,
    pub memory_max: Option<u64>,
    /// Pods sampled (`1h` only).
    pub pods: Option<u32>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct MetricsDto {
    pub window: String,
    /// False when there is nothing to show: never read as zero.
    pub available: bool,
    /// Why it is unavailable.
    pub reason: Option<String>,
    pub points: Vec<MetricPoint>,
}

fn at(ms: i64) -> String {
    crate::routes::request::timestamp(ms)
}

/// An app's CPU and memory usage.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/metrics", operation_id = "getAppMetrics",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        MetricsQuery,
    ),
    responses((status = 200, body = MetricsDto), (status = 422, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Query(q): Query<MetricsQuery>,
) -> ApiResult<Json<MetricsDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let window = q.window.unwrap_or_else(|| "1h".into());
    let now = now_ms();
    let (points, reason) = match window.as_str() {
        "1h" => match &state.usage {
            None => (
                Vec::new(),
                Some("usage is not collected on this server".to_owned()),
            ),
            Some(buffer) => {
                let samples = buffer.window(a.namespace(), a.slug(), now - HOUR_MS);
                let points = samples
                    .iter()
                    .map(|s| MetricPoint {
                        at: at(s.at),
                        cpu_millis: s.cpu_millis,
                        memory_bytes: s.memory_bytes,
                        cpu_max: None,
                        memory_max: None,
                        pods: Some(s.pods),
                    })
                    .collect::<Vec<_>>();
                let reason = points.is_empty().then(|| {
                    buffer.unavailable().unwrap_or_else(|| {
                        "no samples yet: the app runs no pods, or they were not measured".into()
                    })
                });
                (points, reason)
            }
        },
        "7d" => {
            let mut tenant = state.store.tenant(a.env.project.org).await?;
            let hours = tenant.usage_since(a.app.target, now - 7 * 24 * HOUR_MS).await?;
            let points = hours
                .iter()
                .map(|h| MetricPoint {
                    at: at(h.hour),
                    cpu_millis: h.cpu_avg.unsigned_abs(),
                    memory_bytes: h.memory_avg.unsigned_abs(),
                    cpu_max: Some(h.cpu_max.unsigned_abs()),
                    memory_max: Some(h.memory_max.unsigned_abs()),
                    pods: None,
                })
                .collect::<Vec<_>>();
            let reason = points
                .is_empty()
                .then(|| "no hourly usage recorded yet".to_owned());
            (points, reason)
        }
        other => return Err(Error::Validation(format!("window must be 1h or 7d, not `{other}`")).into()),
    };
    Ok(Json(MetricsDto {
        window,
        available: reason.is_none(),
        reason,
        points,
    }))
}

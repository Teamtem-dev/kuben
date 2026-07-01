//! `/livez`, `/readyz`, `/api/v1/healthz/details`.

use axum::{Json, extract::State, http::StatusCode};
use serde::Serialize;
use utoipa::ToSchema;

use crate::{auth::CurrentUser, state::ApiState};

/// Liveness: the runtime is scheduling tasks (watchdog heartbeat < 10s old).
pub async fn livez(State(state): State<ApiState>) -> StatusCode {
    if state.health.is_live(10_000) {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    }
}

/// Readiness: database reachable and initial sync complete.
pub async fn readyz(State(state): State<ApiState>) -> StatusCode {
    if state.health.is_ready() && state.store.ping().await.is_ok() {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct HealthDetails {
    pub ready: bool,
    pub database: &'static str,
    pub cluster: bool,
    pub seq: u64,
    pub pods: usize,
    #[schema(value_type = Object)]
    pub subsystems: serde_json::Value,
}

/// Per-subsystem health (authenticated).
#[utoipa::path(get, path = "/healthz/details", operation_id = "getHealthDetails", tag = "system", responses((status = 200, body = HealthDetails)))]
pub async fn details(State(state): State<ApiState>, _user: CurrentUser) -> Json<HealthDetails> {
    let subsystems = state
        .health
        .details()
        .into_iter()
        .collect::<std::collections::BTreeMap<_, _>>();
    Json(HealthDetails {
        ready: state.health.is_ready(),
        database: state.store.backend(),
        cluster: state.cluster.is_some(),
        seq: state.projections.seq(),
        pods: state.projections.pod_count(),
        subsystems: serde_json::to_value(subsystems).unwrap_or_default(),
    })
}

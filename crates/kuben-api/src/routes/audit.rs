//! Audit log reader (scenario 2): newest first, cursor-paginated, limited to
//! the orgs in which the caller holds `audit-read`.

use std::collections::HashMap;

use axum::{
    Json,
    extract::{Query, State},
};
use kuben_core::{Error, authz::ScopeChain, perm::Perm};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct AuditQuery {
    /// Page size (1–200, default 50).
    pub limit: Option<i64>,
    /// `next_before` of the previous page.
    pub before: Option<i64>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct AuditEventDto {
    pub seq: i64,
    pub id: String,
    /// Unix milliseconds.
    pub at: i64,
    /// `session`, `token`, `user` or `anonymous`.
    pub actor_kind: String,
    /// Email of the actor (or its id if the user no longer exists).
    pub actor: Option<String>,
    /// OpenAPI operation id, e.g. `createApp`.
    pub action: String,
    pub target_kind: Option<String>,
    pub target: Option<String>,
    /// `success`, `denied`, `failure`, `throttled` or `error`.
    pub outcome: String,
    pub status: Option<u16>,
    pub ip: Option<String>,
    pub request_id: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct AuditPage {
    pub events: Vec<AuditEventDto>,
    /// Pass as `before` to fetch the next (older) page.
    pub next_before: Option<i64>,
}

/// Audit log of the caller's organization(s).
#[utoipa::path(
    get,
    path = "/audit", operation_id = "listAudit",
    tag = "audit",
    params(AuditQuery),
    responses((status = 200, body = AuditPage), (status = 403, body = crate::error::Problem))
)]
pub async fn list(

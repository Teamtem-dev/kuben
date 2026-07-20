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
    State(state): State<ApiState>,
    authz: Authz,
    Query(q): Query<AuditQuery>,
) -> ApiResult<Json<AuditPage>> {
    let limit = q.limit.unwrap_or(50).clamp(1, 200);
    let page = usize::try_from(limit).unwrap_or(50);
    let orgs: Vec<_> = authz
        .org_ids()
        .into_iter()
        .filter(|o| {
            authz
                .require(&state, Perm::AuditRead, &ScopeChain::org(*o))
                .is_ok()
        })
        .collect();
    if orgs.is_empty() {
        return Err(Error::Forbidden.into());
    }
    let mut events = Vec::new();
    for org in orgs {
        events.extend(state.store.list_audit(org, q.before, limit).await?);
    }
    events.sort_by_key(|e| std::cmp::Reverse(e.seq));
    events.truncate(page);
    let emails: HashMap<String, String> = state
        .store
        .list_users()
        .await?
        .into_iter()
        .map(|u| (u.id.to_string(), u.email))
        .collect();
    let next_before = if events.len() == page {
        events.last().map(|e| e.seq)
    } else {
        None
    };
    let events = events
        .into_iter()
        .map(|e| AuditEventDto {
            seq: e.seq,
            id: e.id.to_string(),
            at: e.created_at,
            actor_kind: e.actor_kind,
            actor: e
                .actor_id
                .as_ref()
                .map(|id| emails.get(id).cloned().unwrap_or_else(|| id.clone())),
            action: e.action,
            target_kind: e.target_kind,
            target: e.target_ref,
            outcome: e.outcome,
            status: e
                .data
                .as_ref()
                .and_then(|d| d["status"].as_u64())
                .and_then(|s| u16::try_from(s).ok()),
            ip: e.ip,
            request_id: e.request_id,
        })
        .collect();
    Ok(Json(AuditPage { events, next_before }))
}

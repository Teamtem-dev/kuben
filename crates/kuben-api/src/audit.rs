//! Structural audit log (scenario 2).
//!
//! A `route_layer` middleware records every mutating request after it ran.
//! The action is the OpenAPI `operationId` of the matched route, so a new
//! endpoint is audited without a single line in its handler. Request bodies
//! are never recorded; secret values therefore cannot reach the log.

use std::{collections::HashMap, net::SocketAddr, sync::LazyLock};

use axum::{
    extract::{ConnectInfo, MatchedPath, Request, State},
    http::{Method, StatusCode},
    middleware::Next,
    response::Response,
};
use kuben_core::ids::OrgId;
use kuben_store::repo::NewAudit;
use serde_json::json;

use crate::{
    auth::{self, CurrentUser},
    state::ApiState,
};

/// Handlers that write their own, richer record (e.g. the attempted email).
const SELF_AUDITED: &[&str] = &["login"];

/// `(METHOD, "/api/v1/…/{param}")` → `operationId`, built once from the spec.
static OPERATIONS: LazyLock<HashMap<(String, String), String>> =
    LazyLock::new(|| operations(&crate::openapi::spec()));

fn operations(spec: &utoipa::openapi::OpenApi) -> HashMap<(String, String), String> {
    let mut map = HashMap::new();
    for (path, item) in &spec.paths.paths {
        for (method, op) in [
            ("POST", &item.post),
            ("PUT", &item.put),
            ("PATCH", &item.patch),
            ("DELETE", &item.delete),
        ] {
            if let Some(id) = op.as_ref().and_then(|o| o.operation_id.clone()) {
                map.insert((method.to_owned(), path.clone()), id);
            }
        }
    }
    map
}

/// Path parameters, by zipping the route template with the concrete path.
#[must_use]
pub fn path_params(template: &str, path: &str) -> Vec<(String, String)> {
    template
        .split('/')
        .zip(path.split('/'))
        .filter_map(|(t, v)| {
            t.strip_prefix('{')
                .and_then(|t| t.strip_suffix('}'))
                .map(|name| (name.to_owned(), v.to_owned()))
        })
        .collect()
}

#[must_use]
pub fn outcome(status: StatusCode) -> &'static str {
    match status.as_u16() {
        200..=399 => "success",
        401 | 403 => "denied",
        429 => "throttled",
        400..=499 => "failure",
        _ => "error",
    }
}

async fn org_of(
    state: &ApiState,
    params: &[(String, String)],
    current: Option<&CurrentUser>,
) -> Option<OrgId> {
    if let Some((_, project)) = params.iter().find(|(k, _)| k == "project")
        && let Some(org) = state
            .projections
            .project(project)
            .and_then(|p| p.org.as_deref().and_then(|o| o.parse().ok()))
    {
        return Some(org);
    }
    let current = current?;
    if let Some(grant) = &current.token {
        return Some(grant.org);
    }
    state
        .store
        .bindings_for_user(current.user.id)
        .await
        .ok()?
        .first()
        .map(|b| b.org_id)
}

pub async fn record(State(state): State<ApiState>, req: Request, next: Next) -> Response {
    let method = req.method().clone();
    if matches!(method, Method::GET | Method::HEAD | Method::OPTIONS) {

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
        return next.run(req).await;
    }
    let Some(template) = req
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_owned())
    else {
        return next.run(req).await;
    };
    let path = req.uri().path().to_owned();
    let current = req.extensions().get::<CurrentUser>().cloned();
    let peer = req.extensions().get::<ConnectInfo<SocketAddr>>().map(|c| c.0);
    let ip = auth::client_ip(req.headers(), peer, state.cfg.security.trust_forwarded_for);
    let request_id = req
        .headers()
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);

    let response = next.run(req).await;

    let action = OPERATIONS
        .get(&(method.as_str().to_owned(), template.clone()))
        .cloned()
        .unwrap_or_else(|| format!("{method} {template}"));
    if SELF_AUDITED.contains(&action.as_str()) {
        return response;
    }
    let params = path_params(&template, &path);
    let status = response.status();
    let (actor_kind, token) = match &current {
        Some(c) => (c.via, c.token.as_ref().map(|t| t.id.to_string())),
        None => ("anonymous", None),
    };
    let event = NewAudit {
        org_id: org_of(&state, &params, current.as_ref()).await,
        actor_kind: actor_kind.to_owned(),
        actor_id: current.as_ref().map(|c| c.user.id.to_string()),
        action,
        target_kind: params.last().map(|(k, _)| k.clone()),
        target_ref: (!params.is_empty()).then(|| {
            params
                .iter()
                .map(|(_, v)| v.as_str())
                .collect::<Vec<_>>()
                .join("/")
        }),
        outcome: outcome(status).to_owned(),
        ip,
        request_id,
        data: Some(json!({ "status": status.as_u16(), "method": method.as_str(), "token": token })),
    };
    if let Err(e) = state.store.append_audit(event).await {
        tracing::error!(error = %e, "audit write failed");
        metrics::counter!("kuben_audit_write_errors_total").increment(1);
    }
    response
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn params_follow_the_template() {
        assert_eq!(
            path_params(
                "/api/v1/projects/{project}/environments/{environment}/apps/{app}",
                "/api/v1/projects/shop/environments/prod/apps/api"
            ),
            vec![
                ("project".to_owned(), "shop".to_owned()),
                ("environment".to_owned(), "prod".to_owned()),
                ("app".to_owned(), "api".to_owned()),
            ]
        );
        assert!(path_params("/api/v1/projects", "/api/v1/projects").is_empty());
    }

    #[test]
    fn every_mutation_has_an_operation_id() {
        let ops = operations(&crate::openapi::spec());
        assert_eq!(
            ops.get(&(
                "POST".to_owned(),
                "/api/v1/projects/{project}/environments/{environment}/apps".to_owned()
            ))
            .map(String::as_str),
            Some("createApp")
        );
        assert!(ops.len() >= 20, "found {} mutating operations", ops.len());
    }

    #[test]
    fn outcomes() {
        assert_eq!(outcome(StatusCode::CREATED), "success");
        assert_eq!(outcome(StatusCode::FORBIDDEN), "denied");
        assert_eq!(outcome(StatusCode::TOO_MANY_REQUESTS), "throttled");
        assert_eq!(outcome(StatusCode::UNPROCESSABLE_ENTITY), "failure");
        assert_eq!(outcome(StatusCode::SERVICE_UNAVAILABLE), "error");
    }
}

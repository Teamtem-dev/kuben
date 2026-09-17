//! Incidents and webhook endpoints of an organization (M4.10).

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{Error, authz::ScopeChain, ids::OrgId, perm::Perm, time::now_ms};
use kuben_store::repo::{DeliveryRecord, Endpoint, Incident};
use ring::rand::{SecureRandom, SystemRandom};
use serde::{Deserialize, Serialize};
use serde_json::json;
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::request;
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    notify,
    state::ApiState,
};

/// The events an endpoint can subscribe to (`*` for all).
pub const EVENTS: [&str; 11] = [
    "deployment.started",
    "deployment.succeeded",
    "deployment.failed",
    "deployment.cancelled",
    "build.started",
    "build.succeeded",
    "build.failed",
    "build.cancelled",
    "incident.opened",
    "incident.resolved",
    "ping",
];

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct IncidentDto {
    pub id: Uuid,
    pub kind: String,
    /// `critical`, `warning` or `info`.
    pub severity: String,
    pub title: String,
    pub detail: Option<String>,
    pub project: Option<Uuid>,
    pub environment: Option<Uuid>,
    pub app: Option<Uuid>,
    pub opened_at: String,
    pub last_seen_at: String,
    pub occurrences: i64,
    pub acknowledged_at: Option<String>,
    pub acknowledged_by: Option<String>,
    pub resolved_at: Option<String>,
    pub resolved_by: Option<String>,
}

impl From<Incident> for IncidentDto {
    fn from(i: Incident) -> Self {
        Self {
            id: i.id,
            kind: i.kind,
            severity: i.severity,
            title: i.title,
            detail: i.detail,
            project: i.project_id,
            environment: i.environment_id,
            app: i.target_id,
            opened_at: request::timestamp(i.opened_at),
            last_seen_at: request::timestamp(i.last_seen_at),
            occurrences: i.occurrences,
            acknowledged_at: i.acknowledged_at.map(request::timestamp),
            acknowledged_by: i.acknowledged_by,
            resolved_at: i.resolved_at.map(request::timestamp),
            resolved_by: i.resolved_by,
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct IncidentQuery {
    /// Include resolved incidents.
    #[serde(default)]
    pub all: bool,
    /// At most this many (default 100).
    pub limit: Option<i64>,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CreateEndpoint {
    #[schema(example = "ops-pager")]
    pub name: String,
    #[schema(example = "https://hooks.example.com/kuben")]
    pub url: String,
    /// Event types, or `*` for all.
    pub events: Vec<String>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct EndpointDto {
    pub id: Uuid,
    pub name: String,
    pub url: String,
    pub events: Vec<String>,
    pub created_by: String,
    pub created_at: String,
    pub disabled_at: Option<String>,
    /// Deliveries given up in a row; the endpoint is disabled at 20.
    pub failures: i32,
    /// The signing secret: shown once, when the endpoint is made.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub secret: Option<String>,
}

impl From<Endpoint> for EndpointDto {
    fn from(e: Endpoint) -> Self {
        Self {
            id: e.id,
            name: e.name,
            url: e.url,
            events: e.events,
            created_by: e.created_by,
            created_at: request::timestamp(e.created_at),
            disabled_at: e.disabled_at.map(request::timestamp),
            failures: e.failures,
            secret: None,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct DeliveryDto {
    pub id: Uuid,
    pub event: String,
    /// `pending`, `delivered` or `failed`.
    pub status: String,
    pub attempts: i32,
    pub last_status: Option<i32>,
    pub last_error: Option<String>,
    pub created_at: String,
    pub finished_at: Option<String>,
}

impl From<DeliveryRecord> for DeliveryDto {
    fn from(d: DeliveryRecord) -> Self {
        Self {
            id: d.id,
            event: d.event,
            status: d.status,
            attempts: d.attempts,
            last_status: d.last_status,
            last_error: d.last_error,
            created_at: request::timestamp(d.created_at),
            finished_at: d.finished_at.map(request::timestamp),
        }
    }
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

fn check_endpoint(body: &CreateEndpoint) -> Result<Vec<String>, Error> {
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 || name.chars().any(char::is_control) {
        return Err(Error::Validation(
            "name must be 1 to 64 printable characters".into(),
        ));
    }
    if body.url.len() > 2048 {
        return Err(Error::Validation("the URL is too long".into()));
    }
    let mut events: Vec<String> = body.events.iter().map(|e| e.trim().to_owned()).collect();
    events.sort();
    events.dedup();
    if events.is_empty() || events.len() > 32 {
        return Err(Error::Validation("subscribe to 1 to 32 events".into()));
    }
    if let Some(unknown) = events.iter().find(|e| *e != "*" && !EVENTS.contains(&e.as_str())) {
        return Err(Error::Validation(format!("unknown event `{unknown}`")));
    }
    Ok(events)
}

/// The organization's incidents, newest first.
#[utoipa::path(
    get, path = "/incidents", operation_id = "listIncidents", tag = "incidents",
    params(IncidentQuery),
    responses((status = 200, body = Vec<IncidentDto>))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Query(q): Query<IncidentQuery>,
) -> ApiResult<Json<Vec<IncidentDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let found = tenant
        .incidents(q.all, q.limit.unwrap_or(100).clamp(1, 500))
        .await?;
    Ok(Json(found.into_iter().map(IncidentDto::from).collect()))
}

async fn touch(state: &ApiState, authz: &Authz, id: Uuid, resolve: bool) -> ApiResult<StatusCode> {
    let org = org_of(authz)?;
    let _proof = authz.require(state, Perm::AppDeploy, &ScopeChain::org(org))?;
    let (_, actor) = request::actor(authz);
    let mut tenant = state.store.tenant(org).await?;
    let done = if resolve {
        tenant.resolve_incident(id, &actor).await?
    } else {
        tenant.acknowledge_incident(id, &actor).await?
    };
    if !done {
        return Err(Error::Conflict(format!(
            "incident `{id}` is not open{}",
            if resolve { "" } else { " and unacknowledged" }
        ))
        .into());
    }
    let action = if resolve {
        "incident.resolved"
    } else {
        "incident.acknowledged"
    };
    tenant
        .append_audit(request::audit(authz, action, "incident", id.to_string()))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Acknowledge an open incident: someone is on it.
#[utoipa::path(
    post, path = "/incidents/{id}/acknowledge", operation_id = "acknowledgeIncident", tag = "incidents",
    params(("id" = Uuid, Path, description = "Incident id")),
    responses((status = 204, description = "Acknowledged"), (status = 409, body = crate::error::Problem))
)]
pub async fn acknowledge(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    touch(&state, &authz, id, false).await
}

/// Resolve an open incident by hand (a success resolves it on its own).
#[utoipa::path(
    post, path = "/incidents/{id}/resolve", operation_id = "resolveIncident", tag = "incidents",
    params(("id" = Uuid, Path, description = "Incident id")),
    responses((status = 204, description = "Resolved"), (status = 409, body = crate::error::Problem))
)]
pub async fn resolve(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    touch(&state, &authz, id, true).await
}

/// The organization's webhook endpoints.
#[utoipa::path(
    get, path = "/webhooks", operation_id = "listWebhooks", tag = "incidents",
    responses((status = 200, body = Vec<EndpointDto>), (status = 403, body = crate::error::Problem))
)]
pub async fn list_endpoints(
    State(state): State<ApiState>,
    authz: Authz,
) -> ApiResult<Json<Vec<EndpointDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    Ok(Json(
        tenant
            .endpoints()
            .await?
            .into_iter()
            .map(EndpointDto::from)
            .collect(),
    ))
}

/// Add a webhook endpoint; the answer holds its signing secret, once.
#[utoipa::path(
    post, path = "/webhooks", operation_id = "createWebhook", tag = "incidents",
    request_body = CreateEndpoint,
    responses(
        (status = 201, body = EndpointDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "The name is taken"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn create_endpoint(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateEndpoint>,
) -> ApiResult<(StatusCode, Json<EndpointDto>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let events = check_endpoint(&body)?;
    notify::check_target(body.url.trim(), &state.cfg.notify)
        .await
        .map_err(Error::Validation)?;
    let keyring = state
        .keyring
        .as_deref()
        .ok_or_else(|| Error::Unavailable("secret encryption is not configured".into()))?;
    let mut raw = [0u8; 32];
    SystemRandom::new()
        .fill(&mut raw)
        .map_err(|_| Error::Internal("no randomness".into()))?;
    let secret = format!("whsec_{}", URL_SAFE_NO_PAD.encode(raw));
    let id = Uuid::now_v7();
    let sealed = notify::seal_secret(keyring, org, id, secret.as_bytes()).map_err(Error::Internal)?;
    let (_, actor) = request::actor(&authz);
    let name = body.name.trim();
    let mut tenant = state.store.tenant(org).await?;
    tenant
        .create_endpoint(id, name, body.url.trim(), &events, &sealed, &actor)
        .await
        .map_err(|e| request::duplicate(e, &format!("webhook `{name}`")))?;
    tenant
        .append_audit(request::audit(
            &authz,
            "webhook.created",
            "webhook",
            name.to_owned(),
        ))
        .await?;
    let made = tenant
        .endpoints()
        .await?
        .into_iter()
        .find(|e| e.id == id)
        .ok_or_else(|| Error::Internal("the new webhook is missing".into()))?;
    tenant.commit().await?;
    Ok((
        StatusCode::CREATED,
        Json(EndpointDto {
            secret: Some(secret),
            ..EndpointDto::from(made)
        }),
    ))
}

/// Disable a webhook endpoint for good.
#[utoipa::path(
    delete, path = "/webhooks/{id}", operation_id = "disableWebhook", tag = "incidents",
    params(("id" = Uuid, Path, description = "Endpoint id")),
    responses((status = 204, description = "Disabled"), (status = 404, body = crate::error::Problem))
)]
pub async fn disable_endpoint(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    if !tenant.disable_endpoint(id).await? {
        return Err(Error::NotFound(format!("enabled webhook `{id}`")).into());
    }
    tenant
        .append_audit(request::audit(
            &authz,
            "webhook.disabled",
            "webhook",
            id.to_string(),
        ))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Send a `ping` event to an endpoint.
#[utoipa::path(
    post, path = "/webhooks/{id}/ping", operation_id = "pingWebhook", tag = "incidents",
    params(("id" = Uuid, Path, description = "Endpoint id")),
    responses((status = 202, description = "Queued"), (status = 404, body = crate::error::Problem))
)]
pub async fn ping(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let payload = json!({ "type": "ping", "created_at": now_ms() });
    if !tenant.enqueue_to(id, "ping", &payload).await? {
        return Err(Error::NotFound(format!("enabled webhook `{id}`")).into());
    }
    tenant.commit().await?;
    Ok(StatusCode::ACCEPTED)
}

/// The newest deliveries to an endpoint.
#[utoipa::path(
    get, path = "/webhooks/{id}/deliveries", operation_id = "listWebhookDeliveries", tag = "incidents",
    params(("id" = Uuid, Path, description = "Endpoint id")),
    responses((status = 200, body = Vec<DeliveryDto>))
)]
pub async fn deliveries(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<Json<Vec<DeliveryDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let found = tenant.deliveries(id, 100).await?;
    Ok(Json(found.into_iter().map(DeliveryDto::from).collect()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(name: &str, events: &[&str]) -> CreateEndpoint {
        CreateEndpoint {
            name: name.into(),
            url: "https://hooks.example.com/kuben".into(),
            events: events.iter().map(|e| (*e).to_owned()).collect(),
        }
    }

    #[test]
    fn endpoints_subscribe_to_known_events() {
        assert_eq!(
            check_endpoint(&body("pager", &["deployment.failed", "deployment.failed", "*"])).ok(),
            Some(vec!["*".to_owned(), "deployment.failed".to_owned()])
        );
        for bad in [
            body("", &["*"]),
            body("pager", &[]),
            body("pager", &["deploy.everything"]),
        ] {
            assert!(check_endpoint(&bad).is_err(), "{bad:?}");
        }
    }
}

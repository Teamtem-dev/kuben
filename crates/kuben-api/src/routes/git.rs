//! Git providers (M3): the GitHub App's installations and its webhook.
//!
//! An organization admin links an installation once; GitHub cannot say which
//! Kuben organization an installation belongs to. The webhook is served
//! outside the session and CSRF layers (GitHub sends neither) and trusts only
//! the HMAC signature. A push is a hint: it is recorded once per delivery id
//! in the inbox and turned into `source.sync` operations, which read the real
//! branch head from GitHub before anything is built.

use axum::{
    Json,
    body::Bytes,
    extract::State,
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ids::OrgId,
    perm::Perm,
    source::{InstallationAction, PushEvent, WebhookEvent},
};
use kuben_platform::build::ProviderError;
use kuben_store::repo::{GITHUB, NewAudit, Received};
use serde::{Deserialize, Serialize};
use serde_json::json;
use utoipa::ToSchema;

use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    github::{GithubApp, SIGNATURE_HEADER},
    state::ApiState,
};

/// Where GitHub delivers webhooks.
pub const WEBHOOK_PATH: &str = "/api/v1/webhooks/github";
/// Largest delivery accepted; GitHub caps payloads at 25 MiB, pushes are far smaller.
pub const WEBHOOK_BODY_LIMIT: usize = 5 << 20;
const MAX_DELIVERY_ID: usize = 128;

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct LinkInstallation {
    /// The id GitHub shows after the App is installed.
    pub installation_id: u64,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct InstallationDto {
    pub installation_id: u64,
    /// The GitHub user or organization the App is installed on.
    pub account: String,
    pub suspended: bool,
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

pub(crate) fn github(state: &ApiState) -> Result<&GithubApp, ApiError> {
    state.github.as_deref().ok_or_else(|| {
        ApiError(Error::Unavailable(
            "Git sources are not configured on this server (git.github_app_id)".into(),
        ))
    })
}

pub(crate) fn provider_error(e: ProviderError) -> ApiError {
    ApiError(match e {
        ProviderError::NotFound(what) => Error::Validation(format!("GitHub does not know {what}")),
        ProviderError::Refused(why) => Error::Validation(format!("GitHub refused access: {why}")),
        ProviderError::Unavailable(why) => Error::Unavailable(format!("GitHub: {why}")),
    })
}

/// GitHub App installations linked to the caller's organization.
#[utoipa::path(
    get,
    path = "/git/installations", operation_id = "listGitInstallations",
    tag = "git",
    responses((status = 200, body = Vec<InstallationDto>), (status = 403, body = crate::error::Problem))
)]
pub async fn list(State(state): State<ApiState>, authz: Authz) -> ApiResult<Json<Vec<InstallationDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let rows = tenant.installations().await?;
    Ok(Json(
        rows.into_iter()
            .map(|(installation_id, account, suspended)| InstallationDto {
                installation_id,
                account,
                suspended,
            })
            .collect(),
    ))
}

/// Link a GitHub App installation to the caller's organization. GitHub is
/// asked first; an installation stays with the organization that linked it.
#[utoipa::path(
    post,
    path = "/git/installations", operation_id = "linkGitInstallation",
    tag = "git",
    request_body = LinkInstallation,
    responses(
        (status = 201, body = InstallationDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Linked to another organization"),
        (status = 422, body = crate::error::Problem),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn link(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<LinkInstallation>,
) -> ApiResult<(StatusCode, Json<InstallationDto>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let app = github(&state)?;
    let installation = app
        .installation(body.installation_id)
        .await
        .map_err(provider_error)?;
    if installation.suspended {
        return Err(Error::Validation("the installation is suspended on GitHub".into()).into());
    }
    let mut tenant = state.store.tenant(org).await?;
    if !tenant
        .link_installation(body.installation_id, &installation.account)
        .await?
    {
        return Err(Error::Conflict("the installation is linked to another organization".into()).into());
    }
    tenant.commit().await?;
    Ok((
        StatusCode::CREATED,
        Json(InstallationDto {
            installation_id: body.installation_id,
            account: installation.account,
            suspended: false,
        }),
    ))
}

fn header<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers.get(name).and_then(|v| v.to_str().ok())
}

fn answer(status: StatusCode, outcome: &str) -> Response {
    (status, Json(json!({ "outcome": outcome }))).into_response()
}

/// `POST /api/v1/webhooks/github`: a signed delivery from the GitHub App.
pub async fn webhook(State(state): State<ApiState>, headers: HeaderMap, body: Bytes) -> Response {
    let Some(app) = state.github.as_deref() else {
        return answer(StatusCode::NOT_FOUND, "notConfigured");
    };
    let signed = header(&headers, SIGNATURE_HEADER).is_some_and(|s| app.verify(&body, s));
    if !signed {
        tracing::warn!("a GitHub webhook delivery with a missing or wrong signature was refused");
        return answer(StatusCode::UNAUTHORIZED, "badSignature");
    }
    let (Some(event), Some(delivery)) = (
        header(&headers, "x-github-event"),
        header(&headers, "x-github-delivery"),
    ) else {
        return answer(StatusCode::BAD_REQUEST, "missingHeaders");
    };
    if delivery.is_empty() || delivery.len() > MAX_DELIVERY_ID {
        return answer(StatusCode::BAD_REQUEST, "badDeliveryId");
    }
    let parsed = match WebhookEvent::parse_github(event, &body) {
        Ok(parsed) => parsed,
        Err(e) => {
            tracing::warn!(delivery, error = %e, "a malformed GitHub delivery was refused");
            return answer(StatusCode::BAD_REQUEST, "malformed");
        }
    };
    let result = match parsed {
        WebhookEvent::Ping => Ok(answer(StatusCode::OK, "pong")),
        WebhookEvent::Ignored => Ok(answer(StatusCode::ACCEPTED, "ignored")),
        WebhookEvent::Installation {
            action,
            installation_id,
            ..
        } => installation_event(&state, action, installation_id).await,
        WebhookEvent::Push(push) => push_event(&state, delivery, &body, &push).await,
    };
    result.unwrap_or_else(|e| {
        tracing::error!(delivery, error = %e.0, "a GitHub delivery could not be recorded");
        e.into_response()
    })
}

async fn installation_event(
    state: &ApiState,
    action: InstallationAction,
    installation_id: u64,
) -> ApiResult<Response> {
    let suspended = match action {
        // Linking needs an organization admin; GitHub cannot name the org.
        InstallationAction::Created => return Ok(answer(StatusCode::ACCEPTED, "linkInKuben")),
        InstallationAction::Deleted | InstallationAction::Suspended => true,
        InstallationAction::Unsuspended => false,
    };
    let known = state
        .store
        .set_installation_suspended(installation_id, suspended)
        .await?;
    Ok(answer(
        StatusCode::ACCEPTED,
        if known { "recorded" } else { "unknown" },
    ))
}

async fn push_event(state: &ApiState, delivery: &str, body: &[u8], push: &PushEvent) -> ApiResult<Response> {
    let Some((org, suspended)) = state.store.git_installation_org(push.installation_id).await? else {
        return Ok(answer(StatusCode::ACCEPTED, "unknownInstallation"));
    };
    if suspended {
        return Ok(answer(StatusCode::ACCEPTED, "suspended"));
    }
    match state.store.receive(org, GITHUB, delivery, body).await? {
        Received::New(_) => {}
        Received::Duplicate(_) => return Ok(answer(StatusCode::OK, "duplicate")),
        Received::Changed => return Ok(answer(StatusCode::CONFLICT, "deliveryChanged")),
    }
    if push.deleted {
        return Ok(answer(StatusCode::ACCEPTED, "branchDeleted"));
    }
    let mut tenant = state.store.tenant(org).await?;
    let bindings = tenant
        .bindings_for_push(push.installation_id, &push.repository, &push.branch)
        .await?;
    for binding in &bindings {
        let audit = NewAudit {
            actor_kind: "webhook".into(),
            actor_id: Some(format!("github:{}", push.installation_id)),
            action: "syncSource".into(),
            target_kind: Some("app".into()),
            target_ref: Some(binding.target.to_string()),
            outcome: "accepted".into(),
            data: Some(json!({
                "delivery": delivery,
                "repository": push.repository,
                "branch": push.branch,
                "after": push.after,
                "forced": push.forced,
            })),
            ..NewAudit::default()
        };
        tenant
            .request_sync(binding, &format!("github:{delivery}"), audit)
            .await?;
    }
    tenant.commit().await?;
    Ok(answer(
        StatusCode::ACCEPTED,
        if bindings.is_empty() {
            "noBinding"
        } else {
            "syncing"
        },
    ))
}

//! Vulnerability exceptions of an organization (M4.6): one finding may pass
//! the scan gates until the exception expires or is revoked. Granting one
//! weakens every gate it touches, so only people with organization admin
//! rights do it, name an owner and a reason, and never for longer than 90
//! days. Tokens cannot grant or revoke.

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use kuben_core::{Error, authz::ScopeChain, ids::OrgId, perm::Perm, scan::MAX_EXCEPTION_SECS, time::now_ms};
use kuben_store::repo::{NewException, VulnException};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::{request, scope};
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    state::ApiState,
};

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CreateException {
    /// The finding's id, e.g. `CVE-2026-12345` or `GHSA-xxxx-xxxx-xxxx`.
    #[schema(example = "CVE-2026-12345")]
    pub vulnerability: String,
    pub reason: String,
    /// Who answers for it: a person or a team.
    #[schema(example = "platform-team@example.com")]
    pub owner: String,
    /// 1 to 90 days.
    #[schema(example = 30)]
    pub days: u32,
    /// Limit it to one project; every project when omitted.
    pub project: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ExceptionDto {
    pub id: Uuid,
    pub vulnerability: String,
    pub project: Option<Uuid>,
    pub reason: String,
    pub owner: String,
    pub created_by: String,
    pub created_at: String,
    pub expires_at: String,
    pub revoked_at: Option<String>,
    /// In force now.
    pub active: bool,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct ListQuery {
    /// Include expired and revoked exceptions.
    #[serde(default)]
    pub all: bool,
}

impl ExceptionDto {
    fn of(e: VulnException, now: i64) -> Self {
        Self {
            active: e.revoked_at.is_none() && e.expires_at > now,
            id: e.id,
            vulnerability: e.vulnerability,
            project: e.project.map(|p| *p.as_uuid()),
            reason: e.reason,
            owner: e.owner,
            created_by: e.created_by,
            created_at: request::timestamp(e.created_at),
            expires_at: request::timestamp(e.expires_at),
            revoked_at: e.revoked_at.map(request::timestamp),
        }
    }
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

fn check(body: &CreateException) -> Result<(), Error> {
    let id = body.vulnerability.trim();
    let valid_id = !id.is_empty()
        && id.len() <= 64
        && id.as_bytes()[0].is_ascii_alphanumeric()
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b':' | b'-'));
    if !valid_id {
        return Err(Error::Validation(format!("`{id}` is not a vulnerability id")));
    }
    let text = |name: &str, value: &str, max: usize| {
        let value = value.trim();
        if value.is_empty() || value.chars().count() > max || value.chars().any(char::is_control) {
            Err(Error::Validation(format!(
                "{name} must be 1 to {max} printable characters"
            )))
        } else {
            Ok(())
        }
    };
    text("reason", &body.reason, 1024)?;
    text("owner", &body.owner, 256)?;
    let max_days = MAX_EXCEPTION_SECS / 86_400;
    if body.days == 0 || i64::from(body.days) > max_days {
        return Err(Error::Validation(format!(
            "an exception lasts 1 to {max_days} days"
        )));
    }
    Ok(())
}

/// The organization's vulnerability exceptions in force (or all).
#[utoipa::path(
    get,
    path = "/vulnerability-exceptions", operation_id = "listVulnerabilityExceptions",
    tag = "security",
    params(ListQuery),
    responses((status = 200, body = Vec<ExceptionDto>), (status = 403, body = crate::error::Problem))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Query(q): Query<ListQuery>,
) -> ApiResult<Json<Vec<ExceptionDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let now = now_ms();
    let found = tenant.exceptions(q.all).await?;
    Ok(Json(
        found.into_iter().map(|e| ExceptionDto::of(e, now)).collect(),
    ))
}

/// Let one finding pass the scan gates for a while.
#[utoipa::path(
    post,
    path = "/vulnerability-exceptions", operation_id = "createVulnerabilityException",
    tag = "security",
    request_body = CreateException,
    responses(
        (status = 201, body = ExceptionDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateException>,
) -> ApiResult<(StatusCode, Json<ExceptionDto>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    check(&body)?;
    let project = match body.project.as_deref() {
        None => None,
        Some(slug) => Some(scope::project(&state, &authz, slug).await?.id()),
    };
    let (_, actor) = request::actor(&authz);
    let now = now_ms();
    let new = NewException {
        project,
        vulnerability: body.vulnerability.trim().to_owned(),
        reason: body.reason.trim().to_owned(),
        owner: body.owner.trim().to_owned(),
        created_by: actor,
        expires_at: now + i64::from(body.days) * 86_400_000,
    };
    let mut tenant = state.store.tenant(org).await?;
    let id = tenant.create_exception(&new).await?;
    let mut audit = request::audit(
        &authz,
        "vulnerability.exception.created",
        "vulnerability",
        new.vulnerability.clone(),
    );
    audit.data = Some(serde_json::json!({ "id": id, "owner": new.owner, "expires_at": new.expires_at }));
    tenant.append_audit(audit).await?;
    let made = tenant
        .exceptions(true)
        .await?
        .into_iter()
        .find(|e| e.id == id)
        .ok_or_else(|| Error::Internal("the new exception is missing".into()))?;
    tenant.commit().await?;
    Ok((StatusCode::CREATED, Json(ExceptionDto::of(made, now))))
}

/// Revoke an exception for good.
#[utoipa::path(
    delete,
    path = "/vulnerability-exceptions/{id}", operation_id = "revokeVulnerabilityException",
    tag = "security",
    params(("id" = Uuid, Path, description = "Exception id")),
    responses(
        (status = 204, description = "Revoked"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn revoke(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(org).await?;
    if !tenant.revoke_exception(id, &actor).await? {
        return Err(Error::NotFound(format!("exception `{id}`")).into());
    }
    let audit = request::audit(
        &authz,
        "vulnerability.exception.revoked",
        "vulnerability",
        id.to_string(),
    );
    tenant.append_audit(audit).await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(vulnerability: &str, days: u32) -> CreateException {
        CreateException {
            vulnerability: vulnerability.into(),
            reason: "not reachable from the network".into(),
            owner: "platform@example.com".into(),
            days,
            project: None,
        }
    }

    #[test]
    fn exceptions_are_bounded() {
        assert!(check(&body("CVE-2026-12345", 30)).is_ok());
        assert!(check(&body("GHSA-abcd-1234-efgh", 90)).is_ok());
        for bad in [
            body("", 1),
            body("-CVE", 1),
            body("CVE 1", 1),
            body("CVE-1", 0),
            body("CVE-1", 91),
        ] {
            assert!(check(&bad).is_err(), "{bad:?}");
        }
        let ownerless = CreateException {
            owner: "  ".into(),
            ..body("CVE-1", 1)
        };
        assert!(check(&ownerless).is_err());
    }
}

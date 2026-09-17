//! External CI trust (M4.2): trust policies of an organization, and the
//! exchange of a GitHub Actions OIDC token for a short-lived Kuben token.
//!
//! The exchange is served outside the session and CSRF layers (the caller
//! has no session) and trusts only a verified provider token. Every refusal
//! looks the same to the caller; the reason is logged.

use axum::{
    Json,
    extract::{Path, State},
    http::{HeaderMap, StatusCode, header},
    response::{IntoResponse, Response},
};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ci::{DEFAULT_CI_TOKEN_TTL_SECS, DEFAULT_EVENTS, GithubClaims, TrustPolicy},
    ids::{OrgId, TokenId},
    model::TokenScope,
    perm::Perm,
    time::now_ms,
};
use kuben_store::repo::{CiExchange, CiPolicy, Exchanged, NewCiPolicy, NewToken};
use serde::{Deserialize, Serialize};
use serde_json::json;
use utoipa::ToSchema;
use uuid::Uuid;

use super::{request, scope};
use crate::{
    auth::session,
    authz::Authz,
    error::{ApiError, ApiResult},
    state::ApiState,
};

/// Where GitHub Actions exchanges its OIDC token.
pub const EXCHANGE_PATH: &str = "/api/v1/ci/github/token";
/// Largest exchange request body.
pub const EXCHANGE_BODY_LIMIT: usize = 4 << 10;

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CreateCiPolicy {
    /// Unique in the organization, 1–64 characters.
    #[schema(example = "shop-deploy")]
    pub name: String,
    #[schema(example = "shop")]
    pub project: String,
    /// Narrows the tokens to one environment of `project`.
    pub environment: Option<String>,
    /// `owner/name`, for people; the ids decide.
    #[schema(example = "acme/shop")]
    pub repository: String,
    /// GitHub's numeric repository id (`repository_id` claim).
    pub repository_id: u64,
    /// GitHub's numeric owner id (`repository_owner_id` claim).
    pub repository_owner_id: u64,
    /// `refs/heads/main`, `refs/tags/v*`, …
    pub refs: Vec<String>,
    /// GitHub environments allowed; empty for any.
    #[serde(default)]
    pub environments: Vec<String>,
    /// Workflow events allowed; `push`, `workflow_dispatch` and `release`
    /// when empty.
    #[serde(default)]
    pub events: Vec<String>,
    /// `developer` (deploy) or `admin` (deploy and promote).
    pub role: Option<String>,
    /// Life of an exchanged token, 60–3600 seconds (default 900).
    pub token_ttl_secs: Option<u32>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct CiPolicyDto {
    pub id: Uuid,
    pub name: String,
    pub project: Uuid,
    pub environment: Option<Uuid>,
    pub repository: String,
    pub repository_id: u64,
    pub repository_owner_id: u64,
    pub refs: Vec<String>,
    pub environments: Vec<String>,
    pub events: Vec<String>,
    pub role: String,
    pub token_ttl_secs: u32,
    pub created_by: String,
    pub created_at: i64,
    pub revoked_at: Option<i64>,
}

impl From<&CiPolicy> for CiPolicyDto {
    fn from(p: &CiPolicy) -> Self {
        Self {
            id: p.id,
            name: p.name.clone(),
            project: *p.project.as_uuid(),
            environment: p.environment.map(|e| *e.as_uuid()),
            repository: p.repository.clone(),
            repository_id: p.policy.repository_id,
            repository_owner_id: p.policy.repository_owner_id,
            refs: p.policy.refs.clone(),
            environments: p.policy.environments.clone(),
            events: p.policy.events.clone(),
            role: p.policy.role.to_string(),
            token_ttl_secs: p.policy.token_ttl_secs,
            created_by: p.created_by.to_string(),
            created_at: p.created_at,
            revoked_at: p.revoked_at,
        }
    }
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

fn trust_policy(body: &CreateCiPolicy) -> Result<TrustPolicy, Error> {
    let events = if body.events.is_empty() {
        DEFAULT_EVENTS.iter().map(|e| (*e).to_owned()).collect()
    } else {
        body.events.clone()
    };
    let policy = TrustPolicy {
        repository_id: body.repository_id,
        repository_owner_id: body.repository_owner_id,
        refs: body.refs.iter().map(|r| r.trim().to_owned()).collect(),
        environments: body.environments.iter().map(|e| e.trim().to_owned()).collect(),
        events,
        role: body.role.as_deref().unwrap_or("developer").parse()?,
        token_ttl_secs: body.token_ttl_secs.unwrap_or(DEFAULT_CI_TOKEN_TTL_SECS),
    };
    policy.validate().map_err(|e| Error::Validation(e.to_string()))?;
    Ok(policy)
}

/// The organization's CI trust policies.
#[utoipa::path(
    get,
    path = "/ci/trust-policies", operation_id = "listCiTrustPolicies",
    tag = "ci",
    responses((status = 200, body = Vec<CiPolicyDto>), (status = 403, body = crate::error::Problem))
)]
pub async fn list(State(state): State<ApiState>, authz: Authz) -> ApiResult<Json<Vec<CiPolicyDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let policies = state.store.ci_policies(org).await?;
    Ok(Json(policies.iter().map(CiPolicyDto::from).collect()))
}

/// Trust one GitHub repository's workflows to deploy into a project.
/// Exchanged tokens act with the creator's authority, capped at `role`.
#[utoipa::path(
    post,
    path = "/ci/trust-policies", operation_id = "createCiTrustPolicy",
    tag = "ci",
    request_body = CreateCiPolicy,
    responses(
        (status = 201, body = CiPolicyDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "The name is taken"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateCiPolicy>,
) -> ApiResult<(StatusCode, Json<CiPolicyDto>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 || name.chars().any(char::is_control) {
        return Err(Error::Validation("name must be 1–64 printable characters".into()).into());
    }
    let policy = trust_policy(&body)?;
    let own = authz.org_role(org).ok_or(Error::Forbidden)?;
    if policy.role.rank() > own.rank() {
        return Err(Error::Forbidden.into());
    }
    let (project, environment) = match &body.environment {
        None => (scope::project(&state, &authz, &body.project).await?.id(), None),
        Some(env) => {
            let e = scope::environment(&state, &authz, &body.project, env).await?;
            (e.project.id(), Some(e.id()))
        }
    };
    let new = NewCiPolicy {
        org,
        project,
        environment,
        name: name.to_owned(),
        repository: body.repository.trim().to_owned(),
        policy,
        created_by: authz.current.user.id,
    };
    let made = state
        .store
        .create_ci_policy(&new)
        .await
        .map_err(|e| request::duplicate(e, &format!("trust policy `{name}`")))?;
    Ok((StatusCode::CREATED, Json(CiPolicyDto::from(&made))))
}

/// Revoke a trust policy and every token exchanged under it.
#[utoipa::path(
    delete,
    path = "/ci/trust-policies/{policy}", operation_id = "revokeCiTrustPolicy",
    tag = "ci",
    params(("policy" = Uuid, Path, description = "Policy id")),
    responses(
        (status = 204, description = "Revoked"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn revoke(
    State(state): State<ApiState>,
    authz: Authz,
    Path(policy): Path<Uuid>,
) -> ApiResult<StatusCode> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &ScopeChain::org(org))?;
    if state.store.revoke_ci_policy(org, policy).await? {
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(Error::NotFound(format!("an active trust policy `{policy}`")).into())
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct ExchangeRequest {
    policy: Uuid,
}

fn untrusted(reason: &str) -> Response {
    tracing::warn!(reason, "a CI token exchange was refused");
    (
        StatusCode::UNAUTHORIZED,
        Json(json!({ "title": "unauthorized", "detail": "the CI token is not trusted" })),
    )
        .into_response()
}

fn bearer(headers: &HeaderMap) -> Option<&str> {
    headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .map(str::trim)
        .filter(|t| !t.is_empty())
}

/// A token name that says where it came from, at most 64 characters.
fn token_name(policy: &CiPolicy, claims: &GithubClaims) -> String {
    let run = claims.run_id.as_deref().unwrap_or("?");
    format!("ci:{}:{}#{run}", policy.name, claims.repository)
        .chars()
        .take(64)
        .collect()
}

/// `POST /api/v1/ci/github/token` with `Authorization: Bearer <OIDC token>`
/// and `{"policy": "<id>"}`: a Kuben API token for the workflow.
pub async fn exchange(
    State(state): State<ApiState>,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    let Some(oidc) = state.github_oidc.as_deref() else {
        return (
            StatusCode::NOT_FOUND,
            Json(json!({ "detail": "CI trust is not configured" })),
        )
            .into_response();
    };
    let Some(provider_token) = bearer(&headers) else {
        return untrusted("no bearer token");
    };
    let Ok(request) = serde_json::from_slice::<ExchangeRequest>(&body) else {
        return (
            StatusCode::UNPROCESSABLE_ENTITY,
            Json(json!({ "detail": "send {\"policy\": \"<id>\"}" })),
        )
            .into_response();
    };
    let claims = match oidc.verify(provider_token).await {
        Ok(claims) => claims,
        Err(crate::oidc::OidcError::Unavailable(e)) => {
            tracing::error!(error = %e, "the CI issuer's keys are unavailable");
            return StatusCode::SERVICE_UNAVAILABLE.into_response();
        }
        Err(e) => return untrusted(&e.to_string()),
    };
    match issue(&state, oidc.issuer(), &claims, request.policy).await {
        Ok(Ok(response)) => response,
        Ok(Err(reason)) => untrusted(&reason),
        Err(e) => e.into_response(),
    }
}

async fn issue(
    state: &ApiState,
    issuer: &str,
    claims: &GithubClaims,
    policy_id: Uuid,
) -> ApiResult<Result<Response, String>> {
    let Some(policy) = state.store.ci_policy(policy_id).await? else {
        return Ok(Err("no such policy".into()));
    };
    if policy.revoked_at.is_some() {
        return Ok(Err(format!("policy {policy_id} is revoked")));
    }
    if let Err(denied) = policy.policy.evaluate(claims) {
        return Ok(Err(format!("policy {policy_id}: {denied}")));
    }
    let (plaintext, id, secret_hash): (String, TokenId, Vec<u8>) = session::new_api_token();
    let now = now_ms();
    let expires_at = now + i64::from(policy.policy.token_ttl_secs) * 1000;
    let exchange = CiExchange {
        policy: policy.id,
        issuer: issuer.to_owned(),
        jti: claims.jti.clone(),
        provider_expires_at: (claims.exp + kuben_core::ci::CLOCK_LEEWAY_SECS) * 1000,
        token: NewToken {
            id,
            org_id: policy.org,
            owner: policy.created_by,
            name: token_name(&policy, claims),
            prefix: session::token_display_prefix(&plaintext),
            secret_hash,
            scope: TokenScope {
                role: policy.policy.role,
                project: Some(*policy.project.as_uuid()),
                environment: policy.environment.map(|e| *e.as_uuid()),
            },
            expires_at: Some(expires_at),
        },
    };
    match state.store.exchange_ci_token(&exchange).await? {
        Exchanged::Issued(token) => {
            tracing::info!(
                policy = %policy.id,
                token = %token.id,
                repository = %claims.repository,
                git_ref = %claims.git_ref,
                run = claims.run_id.as_deref().unwrap_or_default(),
                "a CI token was issued"
            );
            let body = json!({
                "token": plaintext,
                "expiresAt": expires_at,
                "role": policy.policy.role,
                "project": policy.project,
                "environment": policy.environment,
            });
            Ok(Ok((StatusCode::CREATED, Json(body)).into_response()))
        }
        Exchanged::Replayed => Ok(Err(format!("provider token {} was used before", claims.jti))),
        Exchanged::PolicyRevoked => Ok(Err(format!("policy {policy_id} was revoked"))),
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::perm::Role;

    use super::*;

    fn body() -> CreateCiPolicy {
        serde_json::from_value(json!({
            "name": "shop-deploy", "project": "shop", "repository": "acme/shop",
            "repositoryId": 123_456, "repositoryOwnerId": 42, "refs": ["refs/heads/main"],
        }))
        .expect("body")
    }

    #[test]
    fn bodies_become_deny_by_default_policies() {
        let p = trust_policy(&body()).expect("valid");
        assert_eq!(p.role, Role::Developer);
        assert_eq!(p.events, vec!["push", "workflow_dispatch", "release"]);
        assert_eq!(p.token_ttl_secs, DEFAULT_CI_TOKEN_TTL_SECS);
        let owner = CreateCiPolicy {
            role: Some("owner".into()),
            ..body()
        };
        assert!(trust_policy(&owner).is_err());
        let fork = CreateCiPolicy {
            refs: vec!["refs/pull/*".into()],
            ..body()
        };
        assert!(trust_policy(&fork).is_err());
        assert!(
            serde_json::from_value::<CreateCiPolicy>(json!({ "name": "x", "admin": true })).is_err(),
            "unknown fields are refused"
        );
    }

    #[test]
    fn bearer_tokens_are_read_strictly() {
        let mut headers = HeaderMap::new();
        assert_eq!(bearer(&headers), None);
        headers.insert(header::AUTHORIZATION, "Basic abc".parse().expect("header"));
        assert_eq!(bearer(&headers), None);
        headers.insert(header::AUTHORIZATION, "Bearer  ey.x.y ".parse().expect("header"));
        assert_eq!(bearer(&headers), Some("ey.x.y"));
    }

    #[test]
    fn token_names_say_where_they_came_from() {
        let claims: GithubClaims = serde_json::from_value(json!({
            "iss": "i", "aud": "a", "sub": "s", "jti": "j", "iat": 1, "exp": 2,
            "repository": "acme/shop", "repository_id": "1", "repository_owner": "acme",
            "repository_owner_id": "2", "ref": "refs/heads/main", "event_name": "push", "run_id": "77",
        }))
        .expect("claims");
        let policy = CiPolicy {
            id: Uuid::nil(),
            org: OrgId::new(),
            project: kuben_core::ids::ProjectId::new(),
            environment: None,
            name: "x".repeat(70),
            repository: "acme/shop".into(),
            policy: trust_policy(&body()).expect("policy"),
            created_by: kuben_core::ids::UserId::new(),
            created_at: 0,
            revoked_at: None,
        };
        assert_eq!(token_name(&policy, &claims).chars().count(), 64);
        let short = CiPolicy {
            name: "deploy".into(),
            ..policy
        };
        assert_eq!(token_name(&short, &claims), "ci:deploy:acme/shop#77");
    }
}

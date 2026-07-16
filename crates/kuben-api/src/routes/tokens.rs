//! Personal API tokens (scenario 3). The plaintext token is returned exactly
//! once; the store keeps only `sha256(secret)`. Tokens cannot manage tokens,
//! members or passwords (no privilege persistence through a leaked token).

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{
    Error,
    ids::TokenId,
    model::{ApiToken, TokenScope},
    perm::Role,
    time::now_ms,
};
use kuben_store::repo::NewToken;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::scope;
use crate::{auth::session, authz::Authz, error::ApiResult, state::ApiState};

const DEFAULT_TTL_DAYS: u32 = 90;
const MAX_TTL_DAYS: u32 = 365;
const DAY_MS: i64 = 86_400_000;

fn default_role() -> String {
    "developer".into()
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateToken {
    #[schema(example = "github-actions")]
    pub name: String,
    /// Upper bound on what the token may do: `viewer`, `developer` or `admin`.
    #[serde(default = "default_role")]
    #[schema(example = "developer")]
    pub role: String,
    /// Restrict the token to one project.
    #[schema(example = "shop")]
    pub project: Option<String>,
    /// Restrict the token to one environment of `project`.
    #[schema(example = "staging")]
    pub environment: Option<String>,
    /// Lifetime in days (1–365, default 90).
    pub expires_in_days: Option<u32>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct TokenDto {
    pub id: String,
    pub name: String,
    /// Non-secret prefix to recognise the token, e.g. `kbn_pat_0192f3a1`.
    pub prefix: String,
    pub role: String,
    pub project: Option<String>,
    pub environment: Option<String>,
    pub expires_at: Option<i64>,
    pub last_used_at: Option<i64>,
    pub revoked: bool,
    pub created_at: i64,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct CreatedToken {
    /// The token itself. Shown exactly once; only its hash is stored.
    pub token: String,
    pub info: TokenDto,
}

fn dto(state: &ApiState, t: &ApiToken) -> TokenDto {
    let project = t.scope.project.and_then(|uid| {
        let uid = uid.to_string();
        state
            .projections
            .projects()
            .into_iter()
            .find(|p| p.uid.as_deref() == Some(uid.as_str()))
            .map(|p| p.name.clone())
    });
    let environment = t.scope.environment.and_then(|uid| {
        let uid = uid.to_string();
        state
            .projections
            .environments()
            .into_iter()
            .find(|e| e.uid.as_deref() == Some(uid.as_str()))
            .map(|e| e.name.clone())
    });
    TokenDto {
        id: t.id.to_string(),
        name: t.name.clone(),
        prefix: t.prefix.clone(),
        role: t.scope.role.to_string(),
        project,
        environment,
        expires_at: t.expires_at,
        last_used_at: t.last_used_at,
        revoked: t.revoked_at.is_some(),
        created_at: t.created_at,
    }
}

/// Create a personal API token.
#[utoipa::path(
    post,
    path = "/tokens", operation_id = "createToken",
    tag = "tokens",
    request_body = CreateToken,
    responses(
        (status = 201, body = CreatedToken),
        (status = 403, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateToken>,
) -> ApiResult<(StatusCode, Json<CreatedToken>)> {
    authz.forbid_token()?;
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 || name.chars().any(char::is_control) {
        return Err(Error::Validation("name must be 1–64 printable characters".into()).into());
    }
    let role: Role = body.role.parse()?;
    if role == Role::Owner {
        return Err(Error::Validation("tokens are capped at `admin`".into()).into());
    }
    let org = *authz.org_ids().first().ok_or(Error::Forbidden)?;
    let own = authz.org_role(org).ok_or(Error::Forbidden)?;
    if role.rank() > own.rank() {
        return Err(Error::Validation(format!("a `{own}` cannot create a `{role}` token")).into());
    }
    let (project, environment) = match (&body.project, &body.environment) {
        (None, None) => (None, None),
        (None, Some(_)) => return Err(Error::Validation("environment requires project".into()).into()),
        (Some(p), None) => (Some(scope::project(&state, &authz, p)?.uid), None),
        (Some(p), Some(e)) => {
            let env = scope::environment(&state, &authz, p, e)?;
            let uid = env
                .uid
                .ok_or_else(|| Error::NotFound(format!("environment `{e}`")))?;
            (Some(env.project.uid), Some(uid))
        }
    };
    let days = body.expires_in_days.unwrap_or(DEFAULT_TTL_DAYS);
    if !(1..=MAX_TTL_DAYS).contains(&days) {
        return Err(
            Error::Validation(format!("expires_in_days must be between 1 and {MAX_TTL_DAYS}")).into(),
        );
    }
    let (plaintext, id, secret_hash) = session::new_api_token();
    let token = state
        .store
        .create_token(NewToken {
            id,
            org_id: org,
            owner: authz.current.user.id,
            name: name.to_owned(),
            prefix: session::token_display_prefix(&plaintext),
            secret_hash,
            scope: TokenScope {
                role,
                project,
                environment,
            },
            expires_at: Some(now_ms() + i64::from(days) * DAY_MS),
        })
        .await?;
    Ok((
        StatusCode::CREATED,
        Json(CreatedToken {
            token: plaintext,
            info: dto(&state, &token),
        }),
    ))
}

/// The caller's API tokens.
#[utoipa::path(
    get,
    path = "/tokens", operation_id = "listTokens",

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

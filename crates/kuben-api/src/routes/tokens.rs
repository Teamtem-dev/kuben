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

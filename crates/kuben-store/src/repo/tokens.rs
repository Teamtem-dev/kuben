//! API tokens (scenario 3). Only `sha256(secret)` is stored; the token id is
//! public and used for the lookup, the secret is compared in constant time by
//! the API layer.

use kuben_core::{
    ids::{OrgId, TokenId, UserId},
    model::{ApiToken, TokenScope},
    time::now_ms,
};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for a new token.
#[derive(Debug)]
pub struct NewToken {
    pub id: TokenId,
    pub org_id: OrgId,
    pub owner: UserId,
    pub name: String,
    pub prefix: String,
    pub secret_hash: Vec<u8>,
    pub scope: TokenScope,
    pub expires_at: Option<i64>,
}

#[derive(Debug, sqlx::FromRow)]
struct TokenRow {
    id: String,
    org_id: String,
    owner_user_id: Option<String>,
    name: String,
    prefix: String,
    secret_hash: Vec<u8>,
    scopes: String,
    expires_at: Option<i64>,
    last_used_at: Option<i64>,
    revoked_at: Option<i64>,
    created_at: i64,
}

impl TryFrom<TokenRow> for ApiToken {
    type Error = StoreError;
    fn try_from(r: TokenRow) -> Result<Self, Self::Error> {
        let bad_id = |e: uuid::Error| sqlx::Error::Decode(e.into());
        Ok(Self {
            id: r.id.parse().map_err(bad_id)?,
            org_id: r.org_id.parse().map_err(bad_id)?,
            owner: r.owner_user_id.map(|o| o.parse().map_err(bad_id)).transpose()?,
            name: r.name,
            prefix: r.prefix,
            secret_hash: r.secret_hash,
            scope: serde_json::from_str(&r.scopes).map_err(|e| sqlx::Error::Decode(e.into()))?,
            expires_at: r.expires_at,
            last_used_at: r.last_used_at,
            revoked_at: r.revoked_at,
            created_at: r.created_at,
        })
    }
}

const INSERT_TOKEN: &str = "INSERT INTO api_tokens \
     (id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const SELECT_TOKEN: &str = "SELECT id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, \
     last_used_at, revoked_at, created_at FROM api_tokens WHERE id = $1";
const SELECT_TOKENS_OF_OWNER: &str = "SELECT id, org_id, owner_user_id, name, prefix, secret_hash, scopes, \
     expires_at, last_used_at, revoked_at, created_at FROM api_tokens WHERE owner_user_id = $1 \
     ORDER BY created_at DESC";
const REVOKE_TOKEN: &str =
    "UPDATE api_tokens SET revoked_at = $3 WHERE id = $1 AND owner_user_id = $2 AND revoked_at IS NULL";
const REVOKE_USER_TOKENS: &str =
    "UPDATE api_tokens SET revoked_at = $3 WHERE org_id = $1 AND owner_user_id = $2 AND revoked_at IS NULL";
const TOUCH_TOKEN: &str = "UPDATE api_tokens SET last_used_at = $2 WHERE id = $1";

impl Store {
    pub async fn create_token(&self, t: NewToken) -> Result<ApiToken, StoreError> {
        let created_at = now_ms();

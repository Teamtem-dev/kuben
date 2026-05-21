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

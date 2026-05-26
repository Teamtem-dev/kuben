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
        let scopes = serde_json::to_string(&t.scope).map_err(|e| sqlx::Error::Encode(e.into()))?;
        with_writer!(self, |pool| {
            sqlx::query(INSERT_TOKEN)
                .bind(t.id.to_string())
                .bind(t.org_id.to_string())
                .bind(t.owner.to_string())
                .bind(&t.name)
                .bind(&t.prefix)
                .bind(&t.secret_hash)
                .bind(&scopes)
                .bind(t.expires_at)
                .bind(created_at)
                .execute(pool)
                .await?;
        });
        Ok(ApiToken {
            id: t.id,
            org_id: t.org_id,
            owner: Some(t.owner),
            name: t.name,
            prefix: t.prefix,
            secret_hash: t.secret_hash,
            scope: t.scope,
            expires_at: t.expires_at,
            last_used_at: None,
            revoked_at: None,
            created_at,
        })
    }

    pub async fn find_token(&self, id: TokenId) -> Result<Option<ApiToken>, StoreError> {
        let row: Option<TokenRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_TOKEN)
            .bind(id.to_string())
            .fetch_optional(pool)
            .await?);
        row.map(ApiToken::try_from).transpose()
    }

    pub async fn list_tokens(&self, owner: UserId) -> Result<Vec<ApiToken>, StoreError> {
        let rows: Vec<TokenRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_TOKENS_OF_OWNER)
            .bind(owner.to_string())
            .fetch_all(pool)
            .await?);
        rows.into_iter().map(ApiToken::try_from).collect()
    }

    /// Revoke one of `owner`'s tokens. `false` when there was nothing to revoke.
    pub async fn revoke_token(&self, id: TokenId, owner: UserId) -> Result<bool, StoreError> {
        let n = with_writer!(self, |pool| sqlx::query(REVOKE_TOKEN)
            .bind(id.to_string())
            .bind(owner.to_string())
            .bind(now_ms())
            .execute(pool)
            .await?
            .rows_affected());
        Ok(n > 0)
    }

    /// Revoke every token a user owns in an org (used when a member is removed).
    pub async fn revoke_user_tokens(&self, org: OrgId, user: UserId) -> Result<u64, StoreError> {
        let n = with_writer!(self, |pool| sqlx::query(REVOKE_USER_TOKENS)
            .bind(org.to_string())
            .bind(user.to_string())
            .bind(now_ms())
            .execute(pool)
            .await?
            .rows_affected());
        Ok(n)
    }

    pub async fn touch_token(&self, id: TokenId) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(TOUCH_TOKEN)
                .bind(id.to_string())
                .bind(now_ms())
                .execute(pool)
                .await?;
        });
        Ok(())
    }
}

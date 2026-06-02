use kuben_core::{ids::UserId, model::Session, time::now_ms};

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// Input for creating a session. `id_hash` is `sha256(session_id)`; the raw
/// id only ever lives in the browser cookie (Invariant I-3).
#[derive(Debug)]
pub struct NewSession {
    pub id_hash: Vec<u8>,
    pub user_id: UserId,
    pub expires_at: i64,
    pub ip: Option<String>,
    pub ua_hash: Option<Vec<u8>>,
}

#[derive(Debug, sqlx::FromRow)]
struct SessionRow {
    user_id: String,
    created_at: i64,
    expires_at: i64,
    last_seen_at: Option<i64>,
    revoked_at: Option<i64>,
}

impl TryFrom<SessionRow> for Session {
    type Error = StoreError;
    fn try_from(r: SessionRow) -> Result<Self, Self::Error> {
        Ok(Self {
            user_id: r
                .user_id
                .parse()
                .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
            created_at: r.created_at,
            expires_at: r.expires_at,
            last_seen_at: r.last_seen_at,
            revoked_at: r.revoked_at,
        })
    }
}

const INSERT_SESSION: &str = "INSERT INTO sessions (id_hash, user_id, created_at, expires_at, last_seen_at, ip, ua_hash) \
     VALUES ($1, $2, $3, $4, $5, $6, $7)";
const SELECT_SESSION: &str =
    "SELECT user_id, created_at, expires_at, last_seen_at, revoked_at FROM sessions WHERE id_hash = $1";
const TOUCH_SESSION: &str = "UPDATE sessions SET last_seen_at = $2 WHERE id_hash = $1";
const REVOKE_SESSION: &str = "UPDATE sessions SET revoked_at = $2 WHERE id_hash = $1 AND revoked_at IS NULL";
const REVOKE_ALL_FOR_USER: &str =
    "UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL";
const REVOKE_OTHERS_FOR_USER: &str =
    "UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id_hash <> $2 AND revoked_at IS NULL";
const DELETE_EXPIRED: &str = "DELETE FROM sessions WHERE expires_at < $1";

impl Store {
    pub async fn create_session(&self, s: NewSession) -> Result<(), StoreError> {
        let now = now_ms();
        with_writer!(self, |pool| {
            sqlx::query(INSERT_SESSION)
                .bind(&s.id_hash)
                .bind(s.user_id.to_string())
                .bind(now)
                .bind(s.expires_at)
                .bind(now)
                .bind(&s.ip)
                .bind(&s.ua_hash)
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    pub async fn find_session(&self, id_hash: &[u8]) -> Result<Option<Session>, StoreError> {
        let row: Option<SessionRow> = with_reader!(self, |pool| sqlx::query_as(SELECT_SESSION)
            .bind(id_hash)
            .fetch_optional(pool)
            .await?);
        row.map(Session::try_from).transpose()
    }

    pub async fn touch_session(&self, id_hash: &[u8]) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(TOUCH_SESSION)
                .bind(id_hash)
                .bind(now_ms())
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    pub async fn revoke_session(&self, id_hash: &[u8]) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(REVOKE_SESSION)
                .bind(id_hash)
                .bind(now_ms())
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    pub async fn revoke_all_sessions(&self, user: UserId) -> Result<u64, StoreError> {
        let n = with_writer!(self, |pool| {
            sqlx::query(REVOKE_ALL_FOR_USER)
                .bind(user.to_string())
                .bind(now_ms())
                .execute(pool)
                .await?
                .rows_affected()
        });
        Ok(n)
    }

    /// Revoke every session of `user` except `keep` (after a password change).
    pub async fn revoke_other_sessions(&self, user: UserId, keep: &[u8]) -> Result<u64, StoreError> {
        let n = with_writer!(self, |pool| {
            sqlx::query(REVOKE_OTHERS_FOR_USER)
                .bind(user.to_string())
                .bind(keep)
                .bind(now_ms())
                .execute(pool)
                .await?
                .rows_affected()
        });
        Ok(n)
    }


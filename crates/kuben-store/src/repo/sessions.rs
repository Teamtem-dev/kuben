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


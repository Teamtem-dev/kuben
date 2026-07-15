//! Opaque session ids and API tokens.
//!
//! * Session id: 256 random bits, base64url. The store keeps only
//!   `sha256(id)`; the raw id lives only in the `HttpOnly` cookie.
//! * API token: `kbn_pat_<id>_<secret>`; looked up by its public id, then
//!   `sha256(secret)` is compared in constant time (scenario 3).

use axum_extra::extract::cookie::{Cookie, SameSite};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{config::Config, ids::TokenId, time::now_ms};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

use super::{CurrentUser, TokenGrant};
use crate::state::ApiState;

/// Cookie name when `Secure` is set (`__Host-` prefix forbids Domain/Path tricks).
pub const COOKIE_NAME_SECURE: &str = "__Host-kuben_session";
/// Cookie name for plain-http development.
pub const COOKIE_NAME_DEV: &str = "kuben_session";

#[must_use]
pub fn cookie_name(cfg: &Config) -> &'static str {
    if cfg.security.cookie_secure {
        COOKIE_NAME_SECURE
    } else {
        COOKIE_NAME_DEV
    }
}

#[must_use]
pub fn sha256(bytes: &[u8]) -> Vec<u8> {
    Sha256::digest(bytes).to_vec()
}

/// `(raw_id, sha256(raw_id))`.
#[must_use]
pub fn new_session_id() -> (String, Vec<u8>) {
    let bytes: [u8; 32] = rand::random();
    let raw = URL_SAFE_NO_PAD.encode(bytes);
    let hash = sha256(raw.as_bytes());
    (raw, hash)
}

pub fn build_cookie(cfg: &Config, raw: String) -> Cookie<'static> {
    let mut b = Cookie::build((cookie_name(cfg), raw))
        .path("/")
        .http_only(true)
        .same_site(SameSite::Lax)
        .max_age(time_duration_hours(cfg.security.session_ttl_hours));
    if cfg.security.cookie_secure {
        b = b.secure(true);
    }
    b.build()
}

pub fn removal_cookie(cfg: &Config) -> Cookie<'static> {
    Cookie::build((cookie_name(cfg), "")).path("/").build()
}

fn time_duration_hours(hours: u64) -> time::Duration {
    time::Duration::hours(i64::try_from(hours).unwrap_or(i64::MAX))
}

pub(super) async fn user_from_session(state: &ApiState, raw: &str) -> Option<CurrentUser> {
    let id_hash = sha256(raw.as_bytes());
    let user_id = if let Some(id) = state.session_cache.get(&id_hash).await {
        id
    } else {
        let session = state.store.find_session(&id_hash).await.ok().flatten()?;
        if !session.is_valid_at(now_ms()) {
            return None;
        }
        let _ = state.store.touch_session(&id_hash).await;
        state.session_cache.insert(id_hash, session.user_id).await;
        session.user_id
    };
    let user = state.store.find_user_by_id(user_id).await.ok().flatten()?;
    user.is_active.then_some(CurrentUser {
        user,
        via: "session",
        token: None,
    })
}

pub const TOKEN_PREFIX: &str = "kbn_pat_";
/// `last_used_at` is written at most this often per token.
const TOUCH_EVERY_MS: i64 = 60_000;

/// A new token: `(plaintext, id, sha256(secret))`. Only the hash is stored.
#[must_use]
pub fn new_api_token() -> (String, TokenId, Vec<u8>) {
    let id = TokenId::new();
    let bytes: [u8; 32] = rand::random();
    let secret = URL_SAFE_NO_PAD.encode(bytes);
    let plaintext = format!("{TOKEN_PREFIX}{}_{secret}", id.as_uuid().simple());
    (plaintext, id, sha256(secret.as_bytes()))
}

/// `kbn_pat_<32 hex>_<secret>` → `(id, secret)`. The id is hex, so the first
/// `_` after it is the separator even though the secret may contain `_`.
#[must_use]
pub fn parse_api_token(token: &str) -> Option<(TokenId, &str)> {
    let (id, secret) = token.strip_prefix(TOKEN_PREFIX)?.split_once('_')?;
    if id.len() != 32 || secret.is_empty() {
        return None;
    }
    let uuid = uuid::Uuid::try_parse(id).ok()?;
    Some((TokenId::from_uuid(uuid), secret))
}

/// Non-secret display prefix, e.g. `kbn_pat_0192f3a1`.
#[must_use]
pub fn token_display_prefix(plaintext: &str) -> String {
    plaintext.chars().take(TOKEN_PREFIX.len() + 8).collect()
}

pub(super) async fn user_from_api_token(state: &ApiState, token: &str) -> Option<CurrentUser> {
    let (id, secret) = parse_api_token(token)?;
    let record = state.store.find_token(id).await.ok().flatten()?;
    let presented = sha256(secret.as_bytes());
    if !bool::from(presented.as_slice().ct_eq(record.secret_hash.as_slice())) {
        return None;
    }
    let now = now_ms();
    if !record.is_usable_at(now) {
        return None;
    }
    let user = state.store.find_user_by_id(record.owner?).await.ok().flatten()?;
    if !user.is_active {
        return None;
    }
    if record.last_used_at.is_none_or(|t| now - t > TOUCH_EVERY_MS) {
        let _ = state.store.touch_token(id).await;
    }
    Some(CurrentUser {
        user,
        via: "token",
        token: Some(TokenGrant {
            id,
            org: record.org_id,
            scope: record.scope,

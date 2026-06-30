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

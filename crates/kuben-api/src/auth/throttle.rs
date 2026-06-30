//! Login throttling (scenario 1). Fixed windows over three buckets:
//!
//! * `(email, ip)` — one account guessed from one place (the common attack);
//! * `ip` — one client spraying many accounts;
//! * `email` — a botnet guessing one account from many addresses. Its limit
//!   is high, so an attacker cannot cheaply lock a victim out.
//!
//! Windows live in the database, so every replica counts against the same
//! budget. Bucket keys are SHA-256 hashed before they are stored.

use std::fmt;

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{config::SecurityCfg, time::now_ms};
use kuben_store::Store;

use super::session::sha256;

#[derive(Clone)]
pub struct LoginThrottle {
    store: Store,
    window_ms: i64,
    per_pair: u32,
    per_ip: u32,
    per_account: u32,
}

impl fmt::Debug for LoginThrottle {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("LoginThrottle")
            .field("window_ms", &self.window_ms)
            .field("per_pair", &self.per_pair)
            .field("per_ip", &self.per_ip)
            .field("per_account", &self.per_account)
            .finish_non_exhaustive()
    }
}

/// Stored form of a bucket key: no plaintext email or address at rest.
fn bucket(key: &str) -> String {
    URL_SAFE_NO_PAD.encode(sha256(key.as_bytes()))
}

impl LoginThrottle {
    #[must_use]
    pub fn new(cfg: &SecurityCfg, store: Store) -> Self {
        Self {
            store,
            window_ms: i64::try_from(cfg.login_window_secs.max(1).saturating_mul(1000)).unwrap_or(i64::MAX),
            per_pair: cfg.login_max_failures.max(1),
            per_ip: cfg.login_max_failures_per_ip.max(1),
            per_account: cfg.login_max_failures_per_account.max(1),
        }
    }

    fn buckets(&self, email: &str, ip: Option<&str>) -> [(String, u32); 3] {
        let ip = ip.unwrap_or("unknown");
        [
            (bucket(&format!("pair:{email}|{ip}")), self.per_pair),
            (bucket(&format!("ip:{ip}")), self.per_ip),
            (bucket(&format!("acct:{email}")), self.per_account),
        ]
    }

    /// `Err(retry_after_secs)` when any bucket is exhausted. A database error
    /// lets the attempt through (and is logged): the login itself needs the
    /// same database and fails right after.
    pub async fn check(&self, email: &str, ip: Option<&str>) -> Result<(), u64> {
        let now = now_ms();
        let mut retry_after = 0_u64;
        for (key, limit) in self.buckets(email, ip) {
            let window = self.store.throttle_window(&key).await.unwrap_or_else(|e| {
                tracing::error!(error = %e, "login throttle unavailable");
                None
            });
            if let Some(w) = window {
                let elapsed = now - w.started_at;
                if elapsed < self.window_ms && w.failures >= i64::from(limit) {
                    let remaining = u64::try_from(self.window_ms - elapsed).unwrap_or(0);
                    retry_after = retry_after.max(remaining.div_ceil(1000));
                }
            }
        }
        if retry_after == 0 {
            Ok(())
        } else {
            Err(retry_after)
        }
    }

    pub async fn record_failure(&self, email: &str, ip: Option<&str>) {
        let now = now_ms();
        let window_start = now.saturating_sub(self.window_ms);
        for (key, _) in self.buckets(email, ip) {
            if let Err(e) = self.store.throttle_record_failure(&key, now, window_start).await {
                tracing::error!(error = %e, "failed to record a login failure");

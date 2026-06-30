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
            }
        }
        // Expired windows are dead weight; drop them while we are here.
        if let Err(e) = self.store.throttle_purge(window_start).await {
            tracing::warn!(error = %e, "failed to purge expired login windows");
        }
    }

    /// A correct password clears only the `(email, ip)` bucket: a success
    /// must not reset what an attacker accumulated from other places.
    pub async fn record_success(&self, email: &str, ip: Option<&str>) {
        let [(pair, _), ..] = self.buckets(email, ip);
        if let Err(e) = self.store.throttle_clear(&pair).await {
            tracing::warn!(error = %e, "failed to clear a login window");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg() -> SecurityCfg {
        SecurityCfg {
            login_max_failures: 3,
            login_max_failures_per_ip: 5,
            login_max_failures_per_account: 8,
            login_window_secs: 60,
            ..SecurityCfg::default()
        }
    }

    async fn throttle() -> LoginThrottle {
        LoginThrottle::new(&cfg(), Store::memory().await.expect("store"))
    }

    #[tokio::test]
    async fn pair_bucket_locks_and_success_resets_it() {
        let t = throttle().await;
        for _ in 0..3 {
            assert!(t.check("a@x.io", Some("1.1.1.1")).await.is_ok());
            t.record_failure("a@x.io", Some("1.1.1.1")).await;
        }
        let retry = t.check("a@x.io", Some("1.1.1.1")).await.expect_err("locked");
        assert!((1..=60).contains(&retry), "{retry}");
        assert!(
            t.check("a@x.io", Some("2.2.2.2")).await.is_ok(),
            "other places are unaffected"
        );
        t.record_success("a@x.io", Some("1.1.1.1")).await;
        assert!(t.check("a@x.io", Some("1.1.1.1")).await.is_ok());
    }

    #[tokio::test]
    async fn ip_bucket_catches_password_spraying() {
        let t = throttle().await;
        for i in 0..5 {
            t.record_failure(&format!("user{i}@x.io"), Some("9.9.9.9")).await;
        }
        assert!(t.check("fresh@x.io", Some("9.9.9.9")).await.is_err());
        assert!(t.check("fresh@x.io", Some("8.8.8.8")).await.is_ok());
    }

    #[tokio::test]
    async fn account_bucket_slows_distributed_guessing() {
        let t = throttle().await;
        for i in 0..8 {
            t.record_failure("victim@x.io", Some(&format!("10.0.0.{i}")))
                .await;
        }
        assert!(t.check("victim@x.io", Some("10.9.9.9")).await.is_err());
    }

    #[tokio::test]
    async fn replicas_share_one_budget() {
        let store = Store::memory().await.expect("store");
        let (a, b) = (
            LoginThrottle::new(&cfg(), store.clone()),
            LoginThrottle::new(&cfg(), store),
        );
        a.record_failure("a@x.io", Some("1.1.1.1")).await;
        b.record_failure("a@x.io", Some("1.1.1.1")).await;
        a.record_failure("a@x.io", Some("1.1.1.1")).await;
        assert!(
            b.check("a@x.io", Some("1.1.1.1")).await.is_err(),
            "failures seen by one replica lock the other"
        );
    }

    #[test]
    fn buckets_are_hashed() {
        let key = bucket("pair:a@x.io|1.1.1.1");
        assert!(!key.contains('@') && !key.contains("1.1.1.1"));
        assert_eq!(key, bucket("pair:a@x.io|1.1.1.1"));
    }
}

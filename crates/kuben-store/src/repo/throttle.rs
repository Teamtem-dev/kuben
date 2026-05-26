//! Login-throttle windows (scenario 1), shared by every replica. Buckets are
//! opaque keys chosen by the caller, which hashes them: no email address or
//! client IP is stored.

use crate::{
    Store, StoreError,
    db::{with_reader, with_writer},
};

/// A fixed window: `failures` counted since `started_at` (unix ms).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ThrottleWindow {
    pub failures: i64,
    pub started_at: i64,
}

const SELECT_WINDOW: &str = "SELECT failures, started_at FROM login_throttle WHERE bucket = $1";
/// Count one failure atomically. A window that began at or before `$3`
/// (`now - window`) has expired and restarts at 1.
const RECORD_FAILURE: &str = "INSERT INTO login_throttle (bucket, failures, started_at) VALUES ($1, 1, $2) \
     ON CONFLICT (bucket) DO UPDATE SET \
     failures = CASE WHEN login_throttle.started_at > $3 THEN login_throttle.failures + 1 ELSE 1 END, \
     started_at = CASE WHEN login_throttle.started_at > $3 THEN login_throttle.started_at ELSE $2 END";
const CLEAR: &str = "DELETE FROM login_throttle WHERE bucket = $1";
const PURGE: &str = "DELETE FROM login_throttle WHERE started_at <= $1";

impl Store {
    pub async fn throttle_window(&self, bucket: &str) -> Result<Option<ThrottleWindow>, StoreError> {
        let row: Option<(i64, i64)> = with_reader!(self, |pool| sqlx::query_as(SELECT_WINDOW)
            .bind(bucket)
            .fetch_optional(pool)
            .await?);
        Ok(row.map(|(failures, started_at)| ThrottleWindow { failures, started_at }))
    }

    /// Record a failed attempt at `now`; windows that started at or before
    /// `window_start` are replaced by a new one.
    pub async fn throttle_record_failure(
        &self,
        bucket: &str,
        now: i64,
        window_start: i64,
    ) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(RECORD_FAILURE)
                .bind(bucket)
                .bind(now)
                .bind(window_start)
                .execute(pool)
                .await?;
        });
        Ok(())
    }

    pub async fn throttle_clear(&self, bucket: &str) -> Result<(), StoreError> {
        with_writer!(self, |pool| {
            sqlx::query(CLEAR).bind(bucket).execute(pool).await?;
        });
        Ok(())
    }

    /// Drop windows that started at or before `window_start` (expired).
    pub async fn throttle_purge(&self, window_start: i64) -> Result<u64, StoreError> {
        let done = with_writer!(self, |pool| sqlx::query(PURGE)
            .bind(window_start)
            .execute(pool)
            .await?
            .rows_affected());
        Ok(done)
    }
}

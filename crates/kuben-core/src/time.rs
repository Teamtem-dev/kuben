//! Time helpers. Kuben stores all timestamps as unix milliseconds (`i64`) so
//! the schema is identical on SQLite and Postgres.

/// Current time as unix milliseconds.
#[must_use]
pub fn now_ms() -> i64 {
    jiff::Timestamp::now().as_millisecond()
}

/// Add a number of hours to a unix-millisecond timestamp.
#[must_use]
pub fn plus_hours(ts_ms: i64, hours: u64) -> i64 {
    ts_ms.saturating_add(i64::try_from(hours).unwrap_or(i64::MAX).saturating_mul(3_600_000))
}

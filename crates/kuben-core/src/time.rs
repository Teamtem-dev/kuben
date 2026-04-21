//! Time helpers. Kuben stores all timestamps as unix milliseconds (`i64`) so
//! the schema is identical on SQLite and Postgres.

/// Current time as unix milliseconds.
#[must_use]
pub fn now_ms() -> i64 {
    jiff::Timestamp::now().as_millisecond()

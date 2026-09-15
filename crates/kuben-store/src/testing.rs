//! Test support (ADR-025): tests run against a real PostgreSQL, each in a
//! schema of its own, so they run in parallel without sharing rows.
//!
//! [`pg_store`] reads [`TEST_PG_URL`]; without it, it returns `None` and the
//! caller skips. The Linux PostgreSQL job in CI sets it; the macOS and
//! Windows runners have no database service.

use std::str::FromStr;

use sqlx::{
    AssertSqlSafe,
    postgres::{PgConnectOptions, PgPoolOptions},
};

use crate::Store;

/// Environment variable naming the test server, e.g.
/// `postgres://postgres:kuben@localhost:5432/postgres`.
pub const TEST_PG_URL: &str = "KUBEN_TEST_PG_URL";

/// A migrated [`Store`] on a fresh schema, or `None` when no test server is
/// configured.
///
/// # Panics
/// When the variable is set but the server is unreachable or the schema
/// cannot be migrated: a configured but broken database fails the test
/// instead of skipping it.
pub async fn pg_store() -> Option<Store> {
    let url = std::env::var(TEST_PG_URL).ok().filter(|u| !u.is_empty())?;
    let schema = format!("t_{}", uuid::Uuid::now_v7().simple());
    let admin = PgPoolOptions::new()
        .max_connections(1)
        .connect(&url)
        .await
        .unwrap_or_else(|e| panic!("{TEST_PG_URL} is set but the server is unreachable: {e}"));
    // Safe to format: the name is `t_` followed by hex digits generated above.
    sqlx::query(AssertSqlSafe(format!("CREATE SCHEMA {schema}")))
        .execute(&admin)
        .await
        .expect("create the test schema");
    admin.close().await;
    let options = PgConnectOptions::from_str(&url)
        .expect("parse the test database URL")
        .options([("search_path", schema.as_str())]);
    Some(
        Store::connect_postgres(options, 4)
            .await
            .expect("migrate the test schema"),
    )
}

/// Say why a test did not run.
pub fn skip(test: &str) {
    eprintln!("skipped {test}: set {TEST_PG_URL} to run it against PostgreSQL");
}

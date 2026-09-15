//! Persistence layer on PostgreSQL (ADR-025): product state and durable
//! operations (ADR-032), identity and audit.
//!
//! Every repository test runs against a real PostgreSQL in a schema of its
//! own (`KUBEN_TEST_PG_URL`, see [`testing`] with the `testing` feature).

mod db;
pub mod repo;

pub use db::{Store, StoreError};

#[cfg(any(test, feature = "testing"))]
pub mod testing;

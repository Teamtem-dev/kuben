//! Persistence layer on PostgreSQL (ADR-025). Owns *identity and audit* data
//! only today — desired state lives in Kubernetes CRDs (ADR-015) until the
//! product schema moves here.
//!
//! Every repository test runs against a real PostgreSQL in a schema of its
//! own (`KUBEN_TEST_PG_URL`, see [`testing`] with the `testing` feature).
//! [`legacy`] reads the SQLite database of an older installation, read-only.

mod db;
pub mod legacy;
pub mod repo;

pub use db::{Store, StoreError};

#[cfg(any(test, feature = "testing"))]
pub mod testing;

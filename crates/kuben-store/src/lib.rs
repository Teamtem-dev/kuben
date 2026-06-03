//! Persistence layer. Owns *identity and audit* data only — desired state
//! lives in Kubernetes CRDs (ADR-015).
//!
//! Two backends behind one [`Store`]:
//! * **SQLite (WAL)** for single-replica installs — a single writer pool
//!   (`max_connections = 1`) plus a small read pool, so `SQLITE_BUSY` from
//!   transaction upgrades can never happen.
//! * **Postgres** for HA installs.
//!
//! SQL is written in the portable subset shared by both engines and every
//! repository test runs against both (`KUBEN_TEST_PG_URL`).

mod db;
pub mod repo;

pub use db::{Store, StoreError};

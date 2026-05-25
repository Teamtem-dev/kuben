//! Persistence layer. Owns *identity and audit* data only — desired state
//! lives in Kubernetes CRDs (ADR-015).
//!
//! Two backends behind one [`Store`]:
//! * **SQLite (WAL)** for single-replica installs — a single writer pool

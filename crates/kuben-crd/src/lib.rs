//! Kuben CustomResourceDefinitions (`kuben.dev/v1alpha1`).
//!
//! This crate intentionally has no async runtime dependency so the CLI, the
//! `crdgen` binary and third-party controllers can depend on it cheaply.
//! Kubernetes (etcd) is the source of truth for everything defined here
//! (ADR-001 / ADR-015).

pub mod v1alpha1;

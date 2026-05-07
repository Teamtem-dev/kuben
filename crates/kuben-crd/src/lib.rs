//! Kuben CustomResourceDefinitions (`kuben.dev/v1alpha1`).
//!
//! This crate intentionally has no async runtime dependency so the CLI, the
//! `crdgen` binary and third-party controllers can depend on it cheaply.
//! Kubernetes (etcd) is the source of truth for everything defined here
//! (ADR-001 / ADR-015).

pub mod v1alpha1;

pub use v1alpha1::*;

/// API group of all Kuben CRDs.
pub const GROUP: &str = "kuben.dev";
/// API version of the current CRDs.
pub const VERSION: &str = "v1alpha1";
/// Field manager used for server-side apply.
pub const FIELD_MANAGER: &str = "kuben";

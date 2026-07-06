//! Resolve URL path segments into authorized resources. Objects in orgs the
//! caller does not belong to are reported as `404`, not `403`, so their
//! existence does not leak.

use std::sync::Arc;

use kube::Client;
use kuben_core::{Error, authz::ScopeChain, ids::OrgId};
use kuben_platform::projection::{AppView, EnvironmentView, ProjectView};
use uuid::Uuid;

use crate::{authz::Authz, error::ApiError, state::ApiState};

fn not_found(kind: &str, name: &str) -> ApiError {
    ApiError(Error::NotFound(format!("{kind} `{name}`")))
}

/// The primary cluster, or `503` when none is configured.
pub fn cluster(state: &ApiState) -> Result<Client, ApiError> {
    state
        .cluster
        .as_ref()
        .map(kuben_platform::registry::ClusterRegistry::primary)
        .ok_or_else(|| ApiError(Error::Unavailable("no kubernetes cluster configured".into())))
}

/// Map a Kubernetes API error to a user-facing problem.
pub fn kube_error(err: kube::Error, name: &str) -> ApiError {

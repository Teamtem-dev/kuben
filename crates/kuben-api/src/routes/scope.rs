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
    match &err {
        kube::Error::Api(s) if s.code == 404 => not_found("object", name),
        kube::Error::Api(s) if s.code == 409 => ApiError(Error::Conflict(format!(
            "`{name}` already exists or was changed concurrently; retry"
        ))),
        kube::Error::Api(s) if s.code == 400 || s.code == 422 => {
            ApiError(Error::Validation(s.message.clone()))
        }
        _ => ApiError(Error::internal(err)),
    }
}

#[derive(Debug)]
pub struct ProjectScope {
    pub view: Arc<ProjectView>,
    pub org: OrgId,
    pub uid: Uuid,
}

impl ProjectScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        ScopeChain::project(self.org, self.uid)
    }
}

pub fn project(state: &ApiState, authz: &Authz, name: &str) -> Result<ProjectScope, ApiError> {
    let view = state
        .projections
        .project(name)
        .ok_or_else(|| not_found("project", name))?;
    let org = view
        .org
        .as_deref()
        .and_then(|o| o.parse::<OrgId>().ok())
        .filter(|o| authz.org_ids().contains(o))
        .ok_or_else(|| not_found("project", name))?;
    let uid = view
        .uid
        .as_deref()
        .and_then(|u| u.parse::<Uuid>().ok())
        .ok_or_else(|| not_found("project", name))?;
    Ok(ProjectScope { view, org, uid })
}

/// Kubernetes object name of an environment: `<project>-<env>`.
#[must_use]
pub fn environment_resource_name(project: &str, env: &str) -> String {
    format!("{project}-{env}")
}

/// Short environment name used in URLs and the UI.
#[must_use]
pub fn environment_short_name<'a>(project: &str, resource: &'a str) -> &'a str {
    resource
        .strip_prefix(project)
        .and_then(|r| r.strip_prefix('-'))
        .filter(|s| !s.is_empty())
        .unwrap_or(resource)
}

#[derive(Debug)]
pub struct EnvScope {
    pub project: ProjectScope,
    pub view: Arc<EnvironmentView>,
    pub uid: Option<Uuid>,
}

impl EnvScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        ScopeChain {
            environment: self.uid,
            ..self.project.chain()
        }
    }

    #[must_use]
    pub fn short_name(&self) -> &str {
        environment_short_name(&self.project.view.name, &self.view.name)
    }
}

pub fn environment(state: &ApiState, authz: &Authz, project: &str, env: &str) -> Result<EnvScope, ApiError> {

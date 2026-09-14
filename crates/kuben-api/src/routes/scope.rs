//! Resolve URL path segments into authorized scopes on the SQL model
//! (ADR-032). A project, environment or app is found by slug in each of the
//! caller's organizations in turn. One that does not exist, or exists only in
//! an organization the caller does not belong to, is `404`, not `403`, so its
//! existence does not leak.
//!
//! The scope chain names every node by its SQL id; a row imported from the
//! resource model keeps its Kubernetes UID as an alias, so role bindings and
//! tokens made on that UID keep applying until the importer rewrites them.
//! The resource projections only add live status: a row that was never
//! materialized has none yet.

use std::sync::Arc;

use kube::Client;
use kuben_core::{
    Error,
    authz::{ScopeChain, ScopeRef},
    ids::{ApplicationId, EnvironmentId, OrgId, ProjectId, TargetId},
};
use kuben_platform::{
    controller::resources::namespace_name,
    projection::{AppView, EnvironmentView, ProjectView},
};
use kuben_store::repo::{AppRecord, EnvironmentRecord, Project};

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

/// A live project of one of the caller's organizations.
#[derive(Debug)]
pub struct ProjectScope {
    pub org: OrgId,
    pub project: Project,
    /// Live status of its `Project` resource, once materialized.
    pub view: Option<Arc<ProjectView>>,
}

impl ProjectScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        ScopeChain {
            aliases: self
                .project
                .legacy_uid
                .map(ScopeRef::Project)
                .into_iter()
                .collect(),
            ..ScopeChain::project(self.org, *self.project.id.as_uuid())
        }
    }

    #[must_use]
    pub fn slug(&self) -> &str {
        &self.project.slug
    }

    #[must_use]
    pub const fn id(&self) -> ProjectId {
        self.project.id
    }
}

/// A live environment of a project.
#[derive(Debug)]
pub struct EnvScope {
    pub project: ProjectScope,
    pub env: EnvironmentRecord,
    /// Live status of its `Environment` resource, once materialized.
    pub view: Option<Arc<EnvironmentView>>,
}

impl EnvScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        let mut chain = self.project.chain();
        chain.environment = Some(*self.env.id.as_uuid());
        if let Some(uid) = self.env.legacy_uid {
            chain.aliases.push(ScopeRef::Environment(uid));
        }
        chain
    }

    #[must_use]
    pub fn short_name(&self) -> &str {
        &self.env.slug
    }

    /// Its `Environment` object's name, `<project>-<env>`.
    #[must_use]
    pub fn resource_name(&self) -> String {
        environment_resource_name(self.project.slug(), &self.env.slug)
    }

    /// Its placement's namespace.
    #[must_use]
    pub fn namespace(&self) -> String {
        self.env
            .namespace
            .clone()
            .unwrap_or_else(|| namespace_name(&self.resource_name()))
    }

    /// It, or its project, is being deleted.
    #[must_use]
    pub const fn deleting(&self) -> bool {
        self.env.deleting || self.project.project.deleting
    }

    #[must_use]
    pub const fn id(&self) -> EnvironmentId {
        self.env.id
    }
}

/// A live app: one application on an environment's placement.
#[derive(Debug)]
pub struct AppScope {
    pub env: EnvScope,
    pub app: AppRecord,
    /// Live status of its `App` resource, once materialized.
    pub view: Option<Arc<AppView>>,
}

impl AppScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        let mut chain = self.env.chain();
        chain.app = Some(*self.app.target.as_uuid());
        if let Some(uid) = self.app.legacy_uid {
            chain.aliases.push(ScopeRef::App(uid));
        }
        chain
    }

    #[must_use]
    pub fn slug(&self) -> &str {
        &self.app.slug
    }

    #[must_use]
    pub fn namespace(&self) -> &str {
        &self.app.namespace
    }

    /// It, its environment or its project is being deleted.
    #[must_use]
    pub const fn deleting(&self) -> bool {
        self.app.deleting || self.env.deleting()
    }
}

/// The project `name` in the first of the caller's organizations that has one.
pub async fn project(state: &ApiState, authz: &Authz, name: &str) -> Result<ProjectScope, ApiError> {
    for org in authz.org_ids() {
        let mut tenant = state.store.tenant(org).await?;
        if let Some(project) = tenant.project(name).await? {
            let view = state
                .projections
                .project(&project.slug)
                .filter(|v| v.org.as_deref() == Some(org.to_string().as_str()));
            return Ok(ProjectScope { org, project, view });
        }
    }
    Err(not_found("project", name))
}

pub async fn environment(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
) -> Result<EnvScope, ApiError> {
    let project = self::project(state, authz, project).await?;
    let mut tenant = state.store.tenant(project.org).await?;
    let env = tenant
        .environment(project.id(), env)
        .await?
        .ok_or_else(|| not_found("environment", env))?;
    let view = state
        .projections
        .environment(&environment_resource_name(project.slug(), &env.slug));
    Ok(EnvScope { project, env, view })
}

pub async fn app(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
    app: &str,
) -> Result<AppScope, ApiError> {
    let env = environment(state, authz, project, env).await?;
    let mut tenant = state.store.tenant(env.project.org).await?;
    let record = tenant
        .app(env.id(), app)
        .await?
        .ok_or_else(|| not_found("app", app))?;
    let view = state.projections.app(&record.namespace, &record.slug);
    Ok(AppScope {
        env,
        app: record,
        view,
    })
}

/// The SQL ids of an app's target, for the deployment routes.
#[derive(Debug)]
pub struct TargetScope {
    pub org: OrgId,
    pub project: ProjectId,
    pub environment: EnvironmentId,
    pub application: ApplicationId,
    pub target: TargetId,
    chain: ScopeChain,
}

impl TargetScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        self.chain.clone()
    }
}

/// The target of `app` in environment `env` of `project`.
pub async fn sql_target(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
    app: &str,
) -> Result<TargetScope, ApiError> {
    let a = self::app(state, authz, project, env, app).await?;
    Ok(TargetScope {
        org: a.env.project.org,
        project: a.env.project.id(),
        environment: a.env.id(),
        application: a.app.application,
        target: a.app.target,
        chain: a.chain(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn short_names() {
        assert_eq!(environment_resource_name("shop", "prod"), "shop-prod");
        assert_eq!(environment_short_name("shop", "shop-prod"), "prod");
        assert_eq!(environment_short_name("shop", "legacy"), "legacy");
        assert_eq!(environment_short_name("shop", "shop-"), "shop-");
    }
}

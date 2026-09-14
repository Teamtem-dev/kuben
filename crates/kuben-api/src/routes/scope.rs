//! Resolve URL path segments into authorized resources. Objects in orgs the
//! caller does not belong to are reported as `404`, not `403`, so their
//! existence does not leak.
//!
//! During the handover of ADR-032 a scope is still found in the resource
//! projections, which the routes need to write resources. When SQL has a row
//! behind it, the scope chain names the node by its SQL id and keeps the
//! Kubernetes UID as an alias, so role bindings on either apply.

use std::sync::Arc;

use kube::Client;
use kuben_core::{
    Error,
    authz::{ScopeChain, ScopeRef},
    ids::{ApplicationId, EnvironmentId, OrgId, ProjectId, TargetId},
};
use kuben_platform::projection::{AppView, EnvironmentView, ProjectView};
use kuben_store::repo::{Named, SqlScope};
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

/// The SQL rows behind a scope, looked up in one short read-only transaction
/// of the scope's organization.
async fn sql_scope(
    state: &ApiState,
    org: OrgId,
    project: Named<'_>,
    environment: Option<Named<'_>>,
    app: Option<Named<'_>>,
) -> Result<SqlScope, ApiError> {
    let mut tenant = state.store.tenant(org).await?;
    Ok(tenant.resolve_scope(project, environment, app).await?)
}

#[derive(Debug)]
pub struct ProjectScope {
    pub view: Arc<ProjectView>,
    pub org: OrgId,
    /// The Kubernetes UID of the `Project` resource.
    pub uid: Uuid,
    /// The SQL project behind it, once one exists.
    pub id: Option<ProjectId>,
}

impl ProjectScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        match self.id {
            Some(id) => ScopeChain {
                aliases: vec![ScopeRef::Project(self.uid)],
                ..ScopeChain::project(self.org, *id.as_uuid())
            },
            None => ScopeChain::project(self.org, self.uid),
        }
    }
}

/// The projection of a project the caller may see, with its org and UID.
fn project_view(
    state: &ApiState,
    authz: &Authz,
    name: &str,
) -> Result<(Arc<ProjectView>, OrgId, Uuid), ApiError> {
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
    Ok((view, org, uid))
}

pub async fn project(state: &ApiState, authz: &Authz, name: &str) -> Result<ProjectScope, ApiError> {
    let (view, org, uid) = project_view(state, authz, name)?;
    let sql = sql_scope(
        state,
        org,
        Named {
            slug: &view.name,
            legacy_uid: Some(uid),
        },
        None,
        None,
    )
    .await?;
    Ok(ProjectScope {
        view,
        org,
        uid,
        id: sql.project,
    })
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
    /// The Kubernetes UID of the `Environment` resource.
    pub uid: Option<Uuid>,
    /// The SQL environment behind it, once one exists.
    pub id: Option<EnvironmentId>,
}

impl EnvScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        let mut chain = self.project.chain();
        chain.environment = self.id.map(|id| *id.as_uuid()).or(self.uid);
        if let (Some(_), Some(uid)) = (self.id, self.uid) {
            chain.aliases.push(ScopeRef::Environment(uid));
        }
        chain
    }

    #[must_use]
    pub fn short_name(&self) -> &str {
        environment_short_name(&self.project.view.name, &self.view.name)
    }
}

/// The projections of a project and one of its environments.
struct EnvViews {
    project: Arc<ProjectView>,
    org: OrgId,
    project_uid: Uuid,
    env: Arc<EnvironmentView>,
    env_uid: Option<Uuid>,
}

impl EnvViews {
    fn find(state: &ApiState, authz: &Authz, project: &str, env: &str) -> Result<Self, ApiError> {
        let (project, org, project_uid) = project_view(state, authz, project)?;
        let p = project.name.clone();
        let view = state
            .projections
            .environment(&environment_resource_name(&p, env))
            .or_else(|| state.projections.environment(env))
            .filter(|e| e.project == p)
            .ok_or_else(|| not_found("environment", env))?;
        let env_uid = view.uid.as_deref().and_then(|u| u.parse().ok());
        Ok(Self {
            project,
            org,
            project_uid,
            env: view,
            env_uid,
        })
    }

    /// The environment's short name, its SQL slug.
    fn short_name(&self) -> String {
        environment_short_name(&self.project.name, &self.env.name).to_owned()
    }

    /// The scope, given the SQL rows found behind it.
    fn into_scope(self, sql: SqlScope) -> EnvScope {
        EnvScope {
            project: ProjectScope {
                view: self.project,
                org: self.org,
                uid: self.project_uid,
                id: sql.project,
            },
            view: self.env,
            uid: self.env_uid,
            id: sql.environment,
        }
    }
}

pub async fn environment(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
) -> Result<EnvScope, ApiError> {
    let views = EnvViews::find(state, authz, project, env)?;
    let short = views.short_name();
    let sql = sql_scope(
        state,
        views.org,
        Named {
            slug: &views.project.name,
            legacy_uid: Some(views.project_uid),
        },
        Some(Named {
            slug: &short,
            legacy_uid: views.env_uid,
        }),
        None,
    )
    .await?;
    Ok(views.into_scope(sql))
}

#[derive(Debug)]
pub struct AppScope {
    pub env: EnvScope,
    pub view: Arc<AppView>,
    /// The Kubernetes UID of the `App` resource.
    pub uid: Option<Uuid>,
    /// The SQL application target behind it, once one exists.
    pub target: Option<TargetId>,
}

impl AppScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        let mut chain = self.env.chain();
        chain.app = self.target.map(|id| *id.as_uuid()).or(self.uid);
        if let (Some(_), Some(uid)) = (self.target, self.uid) {
            chain.aliases.push(ScopeRef::App(uid));
        }
        chain
    }
}

pub async fn app(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
    app: &str,
) -> Result<AppScope, ApiError> {
    let views = EnvViews::find(state, authz, project, env)?;
    let view = state
        .projections
        .app(&views.env.namespace, app)
        .ok_or_else(|| not_found("app", app))?;
    let uid = view.uid.as_deref().and_then(|u| u.parse().ok());
    let short = views.short_name();
    let sql = sql_scope(
        state,
        views.org,
        Named {
            slug: &views.project.name,
            legacy_uid: Some(views.project_uid),
        },
        Some(Named {
            slug: &short,
            legacy_uid: views.env_uid,
        }),
        Some(Named {
            slug: &view.name,
            legacy_uid: uid,
        }),
    )
    .await?;
    Ok(AppScope {
        env: views.into_scope(sql),
        view,
        uid,
        target: sql.target,
    })
}

/// A target found through SQL alone, for the routes of the SQL model
/// (ADR-032). Nothing here reads the resource projections.
#[derive(Debug)]
pub struct TargetScope {
    pub org: OrgId,
    pub project: ProjectId,
    pub environment: EnvironmentId,
    pub application: ApplicationId,
    pub target: TargetId,
}

impl TargetScope {
    #[must_use]
    pub fn chain(&self) -> ScopeChain {
        ScopeChain {
            environment: Some(*self.environment.as_uuid()),
            app: Some(*self.target.as_uuid()),
            ..ScopeChain::project(self.org, *self.project.as_uuid())
        }
    }
}

const fn by_slug(slug: &str) -> Named<'_> {
    Named {
        slug,
        legacy_uid: None,
    }
}

/// The target of `app` in environment `env` of `project`, looked up by slug
/// in each of the caller's organizations in turn. Not found anywhere, or in
/// an organization the caller does not belong to, is `404`.
pub async fn sql_target(
    state: &ApiState,
    authz: &Authz,
    project: &str,
    env: &str,
    app: &str,
) -> Result<TargetScope, ApiError> {
    for org in authz.org_ids() {
        let sql = sql_scope(
            state,
            org,
            by_slug(project),
            Some(by_slug(env)),
            Some(by_slug(app)),
        )
        .await?;
        if let (Some(project), Some(environment), Some(application), Some(target)) =
            (sql.project, sql.environment, sql.application, sql.target)
        {
            return Ok(TargetScope {
                org,
                project,
                environment,
                application,
                target,
            });
        }
    }
    Err(not_found("app", app))
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

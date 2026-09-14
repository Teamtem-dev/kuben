//! Name → SQL id resolution for the API's scope chain (ADR-032).
//!
//! While the resource model and SQL coexist, a route still finds its project,
//! environment and app in the resource projections, and asks here for the SQL
//! rows behind them: by the Kubernetes UID the importer recorded
//! (`legacy_uid`), else by slug, always inside the tenant's organization.

use kuben_core::ids::{ApplicationId, EnvironmentId, ProjectId, TargetId};
use uuid::Uuid;

use super::Tenant;
use crate::StoreError;

const FIND_PROJECT: &str = "SELECT id FROM projects \
     WHERE org_id = $1 AND (legacy_uid = $2 OR slug = $3) \
     ORDER BY legacy_uid = $2 DESC NULLS LAST \
     LIMIT 1";
const FIND_ENVIRONMENT: &str = "SELECT id FROM environments \
     WHERE org_id = $1 AND project_id = $2 AND (legacy_uid = $3 OR slug = $4) \
     ORDER BY legacy_uid = $3 DESC NULLS LAST \
     LIMIT 1";
const FIND_TARGET: &str = "SELECT t.id, t.application_id FROM application_targets t \
     JOIN applications a ON a.id = t.application_id \
     JOIN environment_placements p ON p.id = t.placement_id \
     WHERE t.org_id = $1 AND t.project_id = $2 AND p.environment_id = $3 \
       AND (t.legacy_uid = $4 OR a.slug = $5) \
     ORDER BY t.legacy_uid = $4 DESC NULLS LAST, t.created_at \
     LIMIT 1";

/// One level of a scope to resolve: its slug and, when it came from a
/// resource projection, that resource's Kubernetes UID.
#[derive(Clone, Copy, Debug)]
pub struct Named<'a> {
    pub slug: &'a str,
    pub legacy_uid: Option<Uuid>,
}

/// The SQL ids behind a resolved scope; `None` where SQL has no row yet.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct SqlScope {
    pub project: Option<ProjectId>,
    pub environment: Option<EnvironmentId>,
    /// The application target: one application in this environment.
    pub target: Option<TargetId>,
    /// The application of that target.
    pub application: Option<ApplicationId>,
}

impl Tenant {
    /// Resolve a project and, below it, an environment and an application
    /// target. A level is looked up only when the level above was found; a
    /// row matching the Kubernetes UID wins over one matching the slug.
    pub async fn resolve_scope(
        &mut self,
        project: Named<'_>,
        environment: Option<Named<'_>>,
        app: Option<Named<'_>>,
    ) -> Result<SqlScope, StoreError> {
        let org = self.org.to_string();
        let mut scope = SqlScope::default();
        let project_id: Option<Uuid> = sqlx::query_scalar(FIND_PROJECT)
            .bind(&org)
            .bind(project.legacy_uid)
            .bind(project.slug)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(project_id) = project_id else {
            return Ok(scope);
        };
        scope.project = Some(ProjectId::from_uuid(project_id));
        let Some(environment) = environment else {
            return Ok(scope);
        };
        let environment_id: Option<Uuid> = sqlx::query_scalar(FIND_ENVIRONMENT)
            .bind(&org)
            .bind(project_id)
            .bind(environment.legacy_uid)
            .bind(environment.slug)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(environment_id) = environment_id else {
            return Ok(scope);
        };
        scope.environment = Some(EnvironmentId::from_uuid(environment_id));
        let Some(app) = app else {
            return Ok(scope);
        };
        let target: Option<(Uuid, Uuid)> = sqlx::query_as(FIND_TARGET)
            .bind(&org)
            .bind(project_id)
            .bind(environment_id)
            .bind(app.legacy_uid)
            .bind(app.slug)
            .fetch_optional(&mut *self.tx)
            .await?;
        if let Some((target, application)) = target {
            scope.target = Some(TargetId::from_uuid(target));
            scope.application = Some(ApplicationId::from_uuid(application));
        }
        Ok(scope)
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::ids::OrgId;

    use super::*;
    use crate::{
        Store,
        testing::{pg_store, skip},
    };

    /// Project `shop`, environment `production`, application `web` on it.
    async fn shop(store: &Store, org: OrgId) -> SqlScope {
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, &format!("shop-production-{org}"))
            .await
            .expect("placement");
        let app = t.create_application(project, "web", "Web").await.expect("app");
        let target = t.create_target(project, app, placement).await.expect("target");
        t.commit().await.expect("commit");
        SqlScope {
            project: Some(project),
            environment: Some(environment),
            target: Some(target),
            application: Some(app),
        }
    }

    fn named(slug: &str) -> Named<'_> {
        Named {
            slug,
            legacy_uid: None,
        }
    }

    #[tokio::test]
    async fn resolves_by_slug_inside_the_organization() {
        let Some(store) = pg_store().await else {
            skip("scope resolution");
            return;
        };
        let a = store.create_org("a", "A").await.expect("org").id;
        let b = store.create_org("b", "B").await.expect("org").id;
        let expected = shop(&store, a).await;

        let mut t = store.tenant(a).await.expect("tenant");
        assert_eq!(
            t.resolve_scope(named("shop"), Some(named("production")), Some(named("web")))
                .await
                .expect("resolve"),
            expected
        );
        assert_eq!(
            t.resolve_scope(named("shop"), Some(named("staging")), Some(named("web")))
                .await
                .expect("resolve"),
            SqlScope {
                project: expected.project,
                ..SqlScope::default()
            },
            "levels below a missing one are not looked up"
        );
        drop(t);

        let mut t = store.tenant(b).await.expect("tenant");
        assert_eq!(
            t.resolve_scope(named("shop"), None, None).await.expect("resolve"),
            SqlScope::default(),
            "another organization's project is not found"
        );
    }

    #[tokio::test]
    async fn the_kubernetes_uid_wins_over_the_slug() {
        let Some(store) = pg_store().await else {
            skip("legacy scope resolution");
            return;
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let imported = shop(&store, org).await;
        let mut t = store.tenant(org).await.expect("tenant");
        let other = t.create_project("other", "Other").await.expect("project");
        t.commit().await.expect("commit");

        let (project_uid, environment_uid, app_uid) = (Uuid::now_v7(), Uuid::now_v7(), Uuid::now_v7());
        for (table, uid, id) in [
            ("projects", project_uid, imported.project.map(|p| *p.as_uuid())),
            (
                "environments",
                environment_uid,
                imported.environment.map(|e| *e.as_uuid()),
            ),
            (
                "application_targets",
                app_uid,
                imported.target.map(|t| *t.as_uuid()),
            ),
        ] {
            // Safe to format: one of three fixed table names.
            sqlx::query(sqlx::AssertSqlSafe(format!(
                "UPDATE {table} SET legacy_uid = $1 WHERE id = $2"
            )))
            .bind(uid)
            .bind(id)
            .execute(store.pool())
            .await
            .expect("record the Kubernetes UID");
        }

        let mut t = store.tenant(org).await.expect("tenant");
        // The slugs name another project, and an environment and app that do
        // not exist; the UIDs name the imported ones.
        let resolved = t
            .resolve_scope(
                Named {
                    slug: "other",
                    legacy_uid: Some(project_uid),
                },
                Some(Named {
                    slug: "renamed",
                    legacy_uid: Some(environment_uid),
                }),
                Some(Named {
                    slug: "renamed",
                    legacy_uid: Some(app_uid),
                }),
            )
            .await
            .expect("resolve");
        assert_eq!(resolved, imported);
        assert_ne!(resolved.project, Some(other));
    }
}

//! Product schema (ADR-025, ADR-026): projects, environments, clusters,
//! placements, applications and targets.
//!
//! Every statement runs in a [`Tenant`] transaction, which names the
//! organization for row-level security (`kuben.org_id`, I21) and filters by
//! it explicitly as well: authorization never relies on row-level security
//! alone. Tenant-aware foreign keys refuse a row that points into another
//! organization or project, whatever the caller passes.

use std::fmt;

use kuben_core::{
    ids::{ApplicationId, ClusterId, EnvironmentId, OrgId, PlacementId, ProjectId, TargetId},
    ops::{DeployPolicy, Generation, SourceEpoch, TargetState},
    time::now_ms,
};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::{Store, StoreError};

const SET_TENANT: &str = "SELECT set_config('kuben.org_id', $1, true)";
const INSERT_PROJECT: &str =
    "INSERT INTO projects (id, org_id, slug, name, created_at) VALUES ($1, $2, $3, $4, $5)";
const INSERT_PROJECT_DESCRIBED: &str = "INSERT INTO projects (id, org_id, slug, name, description, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6)";
const SELECT_PROJECTS: &str = "SELECT p.id, p.slug, p.name, p.description, p.created_at, p.legacy_uid, p.deleting, \
     (SELECT count(*) FROM environments e WHERE e.project_id = p.id AND e.deleted_at IS NULL) AS environments \
     FROM projects p \
     WHERE p.org_id = $1 AND p.deleted_at IS NULL AND ($2::text IS NULL OR p.slug = $2) \
     ORDER BY p.slug";
const INSERT_ENVIRONMENT: &str = "INSERT INTO environments \
     (id, org_id, project_id, slug, name, protected, env_type, quota, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)";
const INSERT_CLUSTER: &str = "INSERT INTO clusters (id, org_id, name, created_at) VALUES ($1, $2, $3, $4)";
const ENSURE_CLUSTER: &str = "INSERT INTO clusters (id, org_id, name, created_at) VALUES ($1, $2, $3, $4) \
     ON CONFLICT (org_id, name) DO UPDATE SET name = EXCLUDED.name RETURNING id";
const INSERT_PLACEMENT: &str = "INSERT INTO environment_placements \
     (id, org_id, project_id, environment_id, cluster_id, namespace, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)";
const INSERT_APPLICATION: &str = "INSERT INTO applications (id, org_id, project_id, slug, name, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6)";
// A new target is delivered by its cluster's agent when that agent is linked,
// unrevoked and negotiated the runtime feature (migration 0012).
const INSERT_TARGET: &str = "INSERT INTO application_targets \
     (id, org_id, project_id, application_id, placement_id, lifecycle_uid, created_at, delivery) \
     SELECT $1, $2, $3, $4, $5, $6, $7, \
       CASE WHEN EXISTS (SELECT 1 FROM environment_placements p \
                         JOIN cluster_agents a ON a.cluster_id = p.cluster_id AND a.org_id = p.org_id \
                         WHERE p.id = $5 AND a.revoked_at IS NULL \
                           AND a.features @> jsonb_build_array($8::text)) \
            THEN 'agent' ELSE 'controller' END";
const SELECT_TARGET_STATE: &str = "SELECT lifecycle_uid, deleting, desired_generation, source_epoch, \
     build_config_revision, deploy_policy FROM application_targets WHERE id = $1 AND org_id = $2";

/// A live project of the current organization.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Project {
    pub id: ProjectId,
    pub slug: String,
    pub name: String,
    pub description: Option<String>,
    pub created_at: i64,
    pub legacy_uid: Option<Uuid>,
    pub deleting: bool,
    /// Its live environments.
    pub environments: u32,
}

#[derive(sqlx::FromRow)]
struct ProjectRow {
    id: Uuid,
    slug: String,
    name: String,
    description: Option<String>,
    created_at: i64,
    legacy_uid: Option<Uuid>,
    deleting: bool,
    environments: i64,
}

/// What an environment is for; production is protected.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum EnvironmentKind {
    #[default]
    Standard,
    Production,
    Preview,
}

impl EnvironmentKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Standard => "standard",
            Self::Production => "production",
            Self::Preview => "preview",
        }
    }
}

/// A placement id read back from SQL, for callers that only hold the UUID.
#[must_use]
pub const fn placement_id(id: Uuid) -> PlacementId {
    PlacementId::from_uuid(id)
}

#[derive(sqlx::FromRow)]
struct TargetRow {
    lifecycle_uid: Uuid,
    deleting: bool,
    desired_generation: i64,
    source_epoch: i64,
    build_config_revision: i64,
    deploy_policy: String,
}

/// A transaction scoped to one organization. Row-level security shows it only
/// that organization's rows; dropping it without [`Tenant::commit`] rolls back.
pub struct Tenant {
    pub(super) org: OrgId,
    pub(super) tx: Transaction<'static, Postgres>,
}

impl fmt::Debug for Tenant {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Tenant")
            .field("org", &self.org)
            .finish_non_exhaustive()
    }
}

impl Store {
    /// Begin a transaction for `org`. The tenant setting is transaction-local,
    /// so a pooled connection never carries it into the next transaction.
    pub async fn tenant(&self, org: OrgId) -> Result<Tenant, StoreError> {
        let mut tx = self.pool().begin().await?;
        sqlx::query(SET_TENANT)
            .bind(org.to_string())
            .execute(&mut *tx)
            .await?;
        Ok(Tenant { org, tx })
    }
}

impl Tenant {
    #[must_use]
    pub const fn org(&self) -> OrgId {
        self.org
    }

    pub async fn commit(self) -> Result<(), StoreError> {
        self.tx.commit().await?;
        Ok(())
    }

    pub async fn create_project(&mut self, slug: &str, name: &str) -> Result<ProjectId, StoreError> {
        let id = ProjectId::new();
        sqlx::query(INSERT_PROJECT)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(slug)
            .bind(name)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    pub async fn create_project_described(
        &mut self,
        slug: &str,
        name: &str,
        description: Option<&str>,
    ) -> Result<ProjectId, StoreError> {
        let id = ProjectId::new();
        sqlx::query(INSERT_PROJECT_DESCRIBED)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(slug)
            .bind(name)
            .bind(description)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    async fn project_rows(&mut self, slug: Option<&str>) -> Result<Vec<Project>, StoreError> {
        let rows: Vec<ProjectRow> = sqlx::query_as(SELECT_PROJECTS)
            .bind(self.org.to_string())
            .bind(slug)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(|r| Project {
                id: ProjectId::from_uuid(r.id),
                slug: r.slug,
                name: r.name,
                description: r.description,
                created_at: r.created_at,
                legacy_uid: r.legacy_uid,
                deleting: r.deleting,
                environments: u32::try_from(r.environments).unwrap_or(u32::MAX),
            })
            .collect())
    }

    /// The organization's live projects, ordered by slug.
    pub async fn projects(&mut self) -> Result<Vec<Project>, StoreError> {
        self.project_rows(None).await
    }

    /// The organization's live project `slug`.
    pub async fn project(&mut self, slug: &str) -> Result<Option<Project>, StoreError> {
        Ok(self.project_rows(Some(slug)).await?.pop())
    }

    pub async fn create_environment(
        &mut self,
        project: ProjectId,
        slug: &str,
        name: &str,
        protected: bool,
    ) -> Result<EnvironmentId, StoreError> {
        let kind = if protected {
            EnvironmentKind::Production
        } else {
            EnvironmentKind::Standard
        };
        self.create_environment_typed(project, slug, name, kind, None)
            .await
    }

    /// Create an environment of `kind`; production environments are protected.
    pub async fn create_environment_typed(
        &mut self,
        project: ProjectId,
        slug: &str,
        name: &str,
        kind: EnvironmentKind,
        quota: Option<&serde_json::Value>,
    ) -> Result<EnvironmentId, StoreError> {
        let id = EnvironmentId::new();
        sqlx::query(INSERT_ENVIRONMENT)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(slug)
            .bind(name)
            .bind(kind == EnvironmentKind::Production)
            .bind(kind.as_str())
            .bind(quota.map(ToString::to_string))
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// The organization's cluster `name`, created on first use.
    pub async fn ensure_cluster(&mut self, name: &str) -> Result<ClusterId, StoreError> {
        let id: Uuid = sqlx::query_scalar(ENSURE_CLUSTER)
            .bind(*ClusterId::new().as_uuid())
            .bind(self.org.to_string())
            .bind(name)
            .bind(now_ms())
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(ClusterId::from_uuid(id))
    }

    pub async fn create_cluster(&mut self, name: &str) -> Result<ClusterId, StoreError> {
        let id = ClusterId::new();
        sqlx::query(INSERT_CLUSTER)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(name)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// Bind `environment` to `namespace` in `cluster`. The binding is final:
    /// moving is a new placement and a cutover.
    pub async fn create_placement(
        &mut self,
        project: ProjectId,
        environment: EnvironmentId,
        cluster: ClusterId,
        namespace: &str,
    ) -> Result<PlacementId, StoreError> {
        let id = PlacementId::new();
        sqlx::query(INSERT_PLACEMENT)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(*environment.as_uuid())
            .bind(*cluster.as_uuid())
            .bind(namespace)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    pub async fn create_application(
        &mut self,
        project: ProjectId,
        slug: &str,
        name: &str,
    ) -> Result<ApplicationId, StoreError> {
        let id = ApplicationId::new();
        sqlx::query(INSERT_APPLICATION)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(slug)
            .bind(name)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// Put `application` on `placement`, both of `project`, with a fresh
    /// lifecycle UID and generation 0.
    pub async fn create_target(
        &mut self,
        project: ProjectId,
        application: ApplicationId,
        placement: PlacementId,
    ) -> Result<TargetId, StoreError> {
        let id = TargetId::new();
        sqlx::query(INSERT_TARGET)
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(*application.as_uuid())
            .bind(*placement.as_uuid())
            .bind(Uuid::now_v7())
            .bind(now_ms())
            .bind(super::agents::RUNTIME_FEATURE)
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// The control state of `target`, or `None` when the organization has no
    /// such target.
    pub async fn target_state(&mut self, target: TargetId) -> Result<Option<TargetState>, StoreError> {
        let row: Option<TargetRow> = sqlx::query_as(SELECT_TARGET_STATE)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(r) = row else {
            return Ok(None);
        };
        Ok(Some(TargetState {
            lifecycle_uid: r.lifecycle_uid,
            deleting: r.deleting,
            desired_generation: Generation(counter(r.desired_generation)?),
            source_epoch: SourceEpoch(counter(r.source_epoch)?),
            build_config_revision: counter(r.build_config_revision)?,
            policy: deploy_policy(&r.deploy_policy)?,
        }))
    }
}

/// A counter column: `CHECK (>= 0)` in the schema, so a negative value is
/// corruption, not something to clamp.
pub(super) fn counter(value: i64) -> Result<u64, sqlx::Error> {
    u64::try_from(value).map_err(|e| sqlx::Error::Decode(e.into()))
}

pub(super) fn deploy_policy(value: &str) -> Result<DeployPolicy, sqlx::Error> {
    match value {
        "auto" => Ok(DeployPolicy::Auto),
        "manual" => Ok(DeployPolicy::Manual),
        "pinned" => Ok(DeployPolicy::Pinned),
        other => Err(sqlx::Error::Decode(
            format!("unknown deploy policy {other:?}").into(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use sqlx::{AssertSqlSafe, Connection as _};

    use super::*;
    use crate::testing::{pg_store, skip};

    /// The placement and target of a project with one environment, cluster
    /// and application.
    struct Chain {
        placement: PlacementId,
        target: TargetId,
    }

    async fn chain(store: &Store, org: OrgId, prefix: &str) -> Chain {
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t
            .create_project(&format!("{prefix}-shop"), "Shop")
            .await
            .expect("project");
        let environment = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t
            .create_cluster(&format!("{prefix}-eu-1"))
            .await
            .expect("cluster");
        let placement = t
            .create_placement(
                project,
                environment,
                cluster,
                &format!("{prefix}-shop-production"),
            )
            .await
            .expect("placement");
        let application = t
            .create_application(project, "web", "Web")
            .await
            .expect("application");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        t.commit().await.expect("commit");
        Chain { placement, target }
    }

    #[tokio::test]
    async fn targets_bind_only_within_one_organization_and_project() {
        let Some(store) = pg_store().await else {
            skip("tenant-aware keys");
            return;
        };
        let a = store.create_org("a", "A").await.expect("org a").id;
        let b = store.create_org("b", "B").await.expect("org b").id;
        let chain_a = chain(&store, a, "a").await;

        let mut t = store.tenant(a).await.expect("tenant");
        let state = t
            .target_state(chain_a.target)
            .await
            .expect("read")
            .expect("target");
        assert_eq!(state.desired_generation, Generation(0));
        assert_eq!(state.policy, DeployPolicy::Auto);
        let projects = t.projects().await.expect("projects");
        assert_eq!(
            projects.iter().map(|p| p.slug.as_str()).collect::<Vec<_>>(),
            ["a-shop"]
        );
        drop(t);

        // Organization B cannot bind A's placement, even knowing its id.
        let mut t = store.tenant(b).await.expect("tenant");
        let project = t.create_project("b-shop", "Shop").await.expect("project");
        let app = t.create_application(project, "web", "Web").await.expect("app");
        assert!(
            t.create_target(project, app, chain_a.placement).await.is_err(),
            "a placement of another organization"
        );
        drop(t);

        // Nor can another project of the same organization.
        let mut t = store.tenant(a).await.expect("tenant");
        let other = t.create_project("a-other", "Other").await.expect("project");
        let app = t.create_application(other, "api", "API").await.expect("app");
        assert!(
            t.create_target(other, app, chain_a.placement).await.is_err(),
            "a placement of another project"
        );
        drop(t);

        // B's transaction does not see A's target either.
        let mut t = store.tenant(b).await.expect("tenant");
        assert!(t.target_state(chain_a.target).await.expect("read").is_none());
    }

    #[tokio::test]
    async fn row_level_security_shows_one_organization_and_fails_closed() {
        let Some(store) = pg_store().await else {
            skip("row-level security");
            return;
        };
        let a = store.create_org("a", "A").await.expect("org a").id;
        let b = store.create_org("b", "B").await.expect("org b").id;
        chain(&store, a, "a").await;
        chain(&store, b, "b").await;

        // The test server connects as a superuser, which bypasses row-level
        // security; probe as an ordinary role, as the server runs in production.
        let schema: String = sqlx::query_scalar("SELECT current_schema()")
            .fetch_one(store.pool())
            .await
            .expect("schema");
        // Safe to format: the schema is `t_` followed by hex digits.
        sqlx::raw_sql(AssertSqlSafe(format!(
            "DO $$ BEGIN CREATE ROLE kuben_rls_probe NOLOGIN; \
             EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$; \
             GRANT USAGE ON SCHEMA {schema} TO kuben_rls_probe; \
             GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {schema} TO kuben_rls_probe;"
        )))
        .execute(store.pool())
        .await
        .expect("probe role");

        let mut conn = store.pool().acquire().await.expect("connection");

        let mut tx = conn.begin().await.expect("begin");
        sqlx::query("SET LOCAL ROLE kuben_rls_probe")
            .execute(&mut *tx)
            .await
            .expect("role");
        sqlx::query(SET_TENANT)
            .bind(a.to_string())
            .execute(&mut *tx)
            .await
            .expect("tenant");
        let slugs: Vec<String> = sqlx::query_scalar("SELECT slug FROM projects ORDER BY slug")
            .fetch_all(&mut *tx)
            .await
            .expect("projects");
        assert_eq!(slugs, ["a-shop"], "only organization A's rows");
        let targets: i64 = sqlx::query_scalar("SELECT count(*) FROM application_targets")
            .fetch_one(&mut *tx)
            .await
            .expect("targets");
        assert_eq!(targets, 1);
        tx.commit().await.expect("commit");

        // The same connection, next transaction, no tenant: nothing.
        let mut tx = conn.begin().await.expect("begin");
        sqlx::query("SET LOCAL ROLE kuben_rls_probe")
            .execute(&mut *tx)
            .await
            .expect("role");
        let visible: i64 = sqlx::query_scalar("SELECT count(*) FROM projects")
            .fetch_one(&mut *tx)
            .await
            .expect("projects");
        assert_eq!(visible, 0, "the tenant setting outlived its transaction");
        tx.rollback().await.expect("rollback");

        // Writing a row of another organization is refused.
        let mut tx = conn.begin().await.expect("begin");
        sqlx::query("SET LOCAL ROLE kuben_rls_probe")
            .execute(&mut *tx)
            .await
            .expect("role");
        sqlx::query(SET_TENANT)
            .bind(a.to_string())
            .execute(&mut *tx)
            .await
            .expect("tenant");
        let foreign = sqlx::query(INSERT_PROJECT)
            .bind(Uuid::now_v7())
            .bind(b.to_string())
            .bind("smuggled")
            .bind("Smuggled")
            .bind(now_ms())
            .execute(&mut *tx)
            .await;
        assert!(foreign.is_err(), "a row of organization B written as A");
        tx.rollback().await.expect("rollback");
    }

    /// Run one statement on `id` in its own tenant transaction.
    async fn update(store: &Store, org: OrgId, sql: &'static str, id: Uuid) -> Result<u64, sqlx::Error> {
        let mut t = store.tenant(org).await.expect("tenant");
        let rows = sqlx::query(sql)
            .bind(id)
            .execute(&mut *t.tx)
            .await?
            .rows_affected();
        t.tx.commit().await?;
        Ok(rows)
    }

    #[tokio::test]
    async fn placements_and_targets_only_move_forward() {
        let Some(store) = pg_store().await else {
            skip("placement and target guards");
            return;
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let c = chain(&store, org, "a").await;
        let placement = *c.placement.as_uuid();
        let target = *c.target.as_uuid();

        assert!(
            update(
                &store,
                org,
                "UPDATE environment_placements SET namespace = 'elsewhere' WHERE id = $1",
                placement
            )
            .await
            .is_err(),
            "the namespace is part of the binding"
        );
        assert_eq!(
            update(
                &store,
                org,
                "UPDATE environment_placements SET namespace_uid = 'uid-1', state = 'ready' WHERE id = $1",
                placement
            )
            .await
            .expect("record the namespace UID"),
            1
        );
        assert!(
            update(
                &store,
                org,
                "UPDATE environment_placements SET namespace_uid = 'uid-2' WHERE id = $1",
                placement
            )
            .await
            .is_err(),
            "a recorded namespace UID never changes"
        );

        assert_eq!(
            update(
                &store,
                org,
                "UPDATE application_targets SET desired_generation = 3 WHERE id = $1",
                target
            )
            .await
            .expect("raise the generation"),
            1
        );
        assert!(
            update(
                &store,
                org,
                "UPDATE application_targets SET desired_generation = 2 WHERE id = $1",
                target
            )
            .await
            .is_err(),
            "the generation never decreases"
        );
        assert!(
            update(
                &store,
                org,
                "UPDATE application_targets SET lifecycle_uid = gen_random_uuid() WHERE id = $1",
                target
            )
            .await
            .is_err(),
            "the lifecycle UID is identity"
        );
        assert_eq!(
            update(
                &store,
                org,
                "UPDATE application_targets SET deleting = TRUE WHERE id = $1",
                target
            )
            .await
            .expect("begin deletion"),
            1
        );
        assert!(
            update(
                &store,
                org,
                "UPDATE application_targets SET deleting = FALSE WHERE id = $1",
                target
            )
            .await
            .is_err(),
            "a deleting target stays deleting"
        );

        let mut t = store.tenant(org).await.expect("tenant");
        let state = t.target_state(c.target).await.expect("read").expect("target");
        assert_eq!(state.desired_generation, Generation(3));
        assert!(state.deleting);
    }
}

//! Reads and records of the materializer (ADR-032).
//!
//! [`Tenant::materialization`] gathers everything the materializer renders a
//! claimed deployment run from: the run, its target and placement, the
//! project, environment and application names, the release and the
//! configuration revision. [`Store::record_materialization`] records the App
//! object a write produced, conditional on the claim's fence and never over a
//! newer generation. [`Store::record_drift`] records a change someone else
//! made to that object.

use std::collections::BTreeMap;

use kuben_core::{
    artifact::Digest,
    ids::{
        ApplicationId, ConfigRevisionId, DeploymentRunId, EnvironmentId, OperationId, OrgId, ProjectId,
        ReleaseId, RenderPlanId, TargetId,
    },
    ops::{Generation, RunPhase},
};
use serde_json::Value;
use uuid::Uuid;

use super::{Claim, Tenant, product::counter};
use crate::{Store, StoreError};

const MATERIALIZATION: &str = "SELECT r.id AS run_id, r.phase, r.generation, r.lifecycle_uid, r.render_plan_id, \
     pr.id AS project_id, pr.slug AS project_slug, pr.name AS project_name, \
     pr.description AS project_description, \
     e.id AS environment_id, e.slug AS environment_slug, e.name AS environment_name, e.protected, \
     COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) AS env_type, \
     e.quota::text AS quota, \
     p.namespace, p.cluster_id, t.delivery, a.id AS application_id, a.slug AS application_slug, a.name AS application_name, \
     t.id AS target_id, t.desired_generation, (t.deleting OR e.deleting OR pr.deleting) AS deleting, \
     rel.id AS release_id, rel.artifacts::text AS artifacts, rel.source::text AS source, \
     c.id AS config_revision_id, c.revision AS config_revision, c.config::text AS config \
     FROM deployment_runs r \
     JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id \
     JOIN applications a ON a.id = t.application_id AND a.org_id = r.org_id \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = r.org_id \
     JOIN environments e ON e.id = p.environment_id AND e.org_id = r.org_id \
     JOIN projects pr ON pr.id = r.project_id AND pr.org_id = r.org_id \
     JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id \
     JOIN target_config_revisions c ON c.id = r.config_revision_id AND c.org_id = r.org_id \
     WHERE r.operation_id = $1 AND r.org_id = $2";
const RECORD: &str = "INSERT INTO target_materializations \
     (target_id, org_id, project_id, generation, operation_id, resource_uid, resource_generation, written_at) \
     SELECT $1, $2, $3, $4, o.id, $5, $6, kuben_now_ms() FROM operations o \
     WHERE o.id = $7 AND o.fence = $8 AND NOT o.done AND o.org_id = $2 AND o.target_id = $1 \
     ON CONFLICT (target_id) DO UPDATE \
     SET generation = EXCLUDED.generation, operation_id = EXCLUDED.operation_id, \
         resource_uid = EXCLUDED.resource_uid, resource_generation = EXCLUDED.resource_generation, \
         written_at = EXCLUDED.written_at \
     WHERE target_materializations.generation <= EXCLUDED.generation";
const MATERIALIZED_TARGET: &str = "SELECT target_id, org_id, project_id, generation, operation_id, \
     resource_uid, resource_generation, drift_count, drift_detected_at, drift::text AS drift \
     FROM target_materializations WHERE target_id = $1 AND org_id = $2";
const MATERIALIZED_RESOURCE: &str = "SELECT target_id, org_id, project_id, generation, operation_id, \
     resource_uid, resource_generation, drift_count, drift_detected_at, drift::text AS drift \
     FROM target_materializations WHERE resource_uid = $1";
const RECORD_DRIFT: &str = "UPDATE target_materializations \
     SET resource_uid = COALESCE($3, resource_uid), resource_generation = COALESCE($4, resource_generation), \
         drift_count = drift_count + 1, drift_detected_at = kuben_now_ms(), drift = $5::jsonb \
     WHERE target_id = $1 AND generation = $2";
const RUN_PLAN_ID: &str = "SELECT render_plan_id FROM deployment_runs WHERE id = $1 AND org_id = $2";
const SET_RUN_PLAN: &str = "UPDATE deployment_runs SET render_plan_id = $3, updated_at = kuben_now_ms() \
     WHERE id = $1 AND org_id = $2 AND render_plan_id IS NULL AND EXISTS (SELECT 1 FROM operations o \
       WHERE o.id = deployment_runs.operation_id AND o.id = $4 AND o.fence = $5 AND NOT o.done)";
const RUN_PLAN: &str = "SELECT p.id, p.renderer_version, p.capability_snapshot::text AS capability_snapshot, \
     p.resources::text AS resources FROM deployment_runs r \
     JOIN render_plans p ON p.id = r.render_plan_id AND p.org_id = r.org_id \
     WHERE r.id = $1 AND r.org_id = $2";

/// Everything a deployment run is rendered from.
#[derive(Clone, Debug, PartialEq)]
pub struct Materialization {
    pub org: OrgId,
    pub run: DeploymentRunId,
    pub operation: OperationId,
    pub phase: RunPhase,
    /// The target generation the run owns.
    pub generation: Generation,
    pub lifecycle_uid: Uuid,
    pub project: ProjectId,
    pub project_slug: String,
    pub project_name: String,
    pub project_description: Option<String>,
    pub environment: EnvironmentId,
    pub environment_slug: String,
    pub environment_name: String,
    pub protected: bool,
    /// `standard`, `production` or `preview`.
    pub env_type: String,
    pub quota: Option<Value>,
    /// The namespace of the target's placement.
    pub namespace: String,
    /// The cluster of the target's placement.
    pub cluster: kuben_core::ids::ClusterId,
    /// How the target's runs reach the cluster.
    pub delivery: super::agents::Delivery,
    pub application: ApplicationId,
    pub application_slug: String,
    pub application_name: String,
    pub target: TargetId,
    /// The target's generation now. A live object that claims a higher one
    /// was not written by Kuben.
    pub desired_generation: Generation,
    /// The target, its environment or its project is being deleted.
    pub deleting: bool,
    pub release: ReleaseId,
    /// Process name → digest.
    pub artifacts: BTreeMap<String, Digest>,
    /// `releases.source.image_repository`: where the digests live.
    pub image_repository: Option<String>,
    pub config_revision: ConfigRevisionId,
    pub config_revision_number: u64,
    /// The App spec without its image (the config revision contract).
    pub config: Value,
    /// The run's frozen RenderPlan, once the materializer has frozen it.
    pub render_plan: Option<RenderPlanId>,
}

/// A run's frozen RenderPlan (ADR-026).
#[derive(Clone, Debug, PartialEq)]
pub struct RunPlan {
    pub id: RenderPlanId,
    pub renderer_version: String,
    pub capability_snapshot: Value,
    /// The normalized resources, an array of objects.
    pub resources: Value,
}

/// What the materializer last wrote for a target.
#[derive(Clone, Debug, PartialEq)]
pub struct Materialized {
    pub org: OrgId,
    pub project: ProjectId,
    pub target: TargetId,
    pub generation: Generation,
    pub operation: OperationId,
    /// Kubernetes UID of the App object.
    pub resource_uid: String,
    /// The object's `metadata.generation` after the last write by Kuben.
    pub resource_generation: i64,
    pub drift_count: u64,
    pub drift_detected_at: Option<i64>,
    pub drift: Option<Value>,
}

#[derive(sqlx::FromRow)]
struct MaterializationRow {
    run_id: Uuid,
    phase: String,
    generation: i64,
    lifecycle_uid: Uuid,
    render_plan_id: Option<Uuid>,
    project_id: Uuid,
    project_slug: String,
    project_name: String,
    project_description: Option<String>,
    environment_id: Uuid,
    environment_slug: String,
    environment_name: String,
    protected: bool,
    env_type: String,
    quota: Option<String>,
    namespace: String,
    cluster_id: Uuid,
    delivery: String,
    application_id: Uuid,
    application_slug: String,
    application_name: String,
    target_id: Uuid,
    desired_generation: i64,
    deleting: bool,
    release_id: Uuid,
    artifacts: String,
    source: Option<String>,
    config_revision_id: Uuid,
    config_revision: i64,
    config: String,
}

#[derive(sqlx::FromRow)]
struct RunPlanRow {
    id: Uuid,
    renderer_version: String,
    capability_snapshot: String,
    resources: String,
}

#[derive(sqlx::FromRow)]
struct MaterializedRow {
    target_id: Uuid,
    org_id: String,
    project_id: Uuid,
    generation: i64,
    operation_id: Uuid,
    resource_uid: String,
    resource_generation: i64,
    drift_count: i64,
    drift_detected_at: Option<i64>,
    drift: Option<String>,
}

fn decode(e: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> sqlx::Error {
    sqlx::Error::Decode(e.into())
}

fn json(text: &str) -> Result<Value, sqlx::Error> {
    serde_json::from_str(text).map_err(decode)
}

impl MaterializationRow {
    fn into_materialization(
        self,
        org: OrgId,
        operation: OperationId,
    ) -> Result<Materialization, sqlx::Error> {
        let artifacts: BTreeMap<String, String> = serde_json::from_str(&self.artifacts).map_err(decode)?;
        let artifacts = artifacts
            .into_iter()
            .map(|(process, digest)| Ok((process, digest.parse().map_err(decode)?)))
            .collect::<Result<_, sqlx::Error>>()?;
        let image_repository = self
            .source
            .as_deref()
            .map(json)
            .transpose()?
            .and_then(|s| s.get("image_repository")?.as_str().map(str::to_owned));
        Ok(Materialization {
            org,
            run: DeploymentRunId::from_uuid(self.run_id),
            operation,
            phase: RunPhase::parse(&self.phase)
                .ok_or_else(|| decode(format!("unknown run phase {:?}", self.phase)))?,
            generation: Generation(counter(self.generation)?),
            lifecycle_uid: self.lifecycle_uid,
            project: ProjectId::from_uuid(self.project_id),
            project_slug: self.project_slug,
            project_name: self.project_name,
            project_description: self.project_description,
            environment: EnvironmentId::from_uuid(self.environment_id),
            environment_slug: self.environment_slug,
            environment_name: self.environment_name,
            protected: self.protected,
            env_type: self.env_type,
            quota: self.quota.as_deref().map(json).transpose()?,
            namespace: self.namespace,
            cluster: kuben_core::ids::ClusterId::from_uuid(self.cluster_id),
            delivery: super::agents::Delivery::parse(&self.delivery)
                .ok_or_else(|| decode(format!("unknown delivery {:?}", self.delivery)))?,
            application: ApplicationId::from_uuid(self.application_id),
            application_slug: self.application_slug,
            application_name: self.application_name,
            target: TargetId::from_uuid(self.target_id),
            desired_generation: Generation(counter(self.desired_generation)?),
            deleting: self.deleting,
            release: ReleaseId::from_uuid(self.release_id),
            artifacts,
            image_repository,
            config_revision: ConfigRevisionId::from_uuid(self.config_revision_id),
            config_revision_number: counter(self.config_revision)?,
            config: json(&self.config)?,
            render_plan: self.render_plan_id.map(RenderPlanId::from_uuid),
        })
    }
}

impl MaterializedRow {
    fn into_materialized(self) -> Result<Materialized, sqlx::Error> {
        Ok(Materialized {
            org: self.org_id.parse().map_err(|e: uuid::Error| decode(e))?,
            project: ProjectId::from_uuid(self.project_id),
            target: TargetId::from_uuid(self.target_id),
            generation: Generation(counter(self.generation)?),
            operation: OperationId::from_uuid(self.operation_id),
            resource_uid: self.resource_uid,
            resource_generation: self.resource_generation,
            drift_count: counter(self.drift_count)?,
            drift_detected_at: self.drift_detected_at,
            drift: self.drift.as_deref().map(json).transpose()?,
        })
    }
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Tenant {
    /// The deployment run of `operation` with everything it is rendered from,
    /// or `None` when the organization has no such run.
    pub async fn materialization(
        &mut self,
        operation: OperationId,
    ) -> Result<Option<Materialization>, StoreError> {
        let row: Option<MaterializationRow> = sqlx::query_as(MATERIALIZATION)
            .bind(*operation.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row
            .map(|r| r.into_materialization(self.org, operation))
            .transpose()?)
    }

    /// What the materializer last wrote for `target`, if anything.
    pub async fn materialized(&mut self, target: TargetId) -> Result<Option<Materialized>, StoreError> {
        let row: Option<MaterializedRow> = sqlx::query_as(MATERIALIZED_TARGET)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(MaterializedRow::into_materialized).transpose()?)
    }

    /// Freeze `run`'s RenderPlan for the holder of `claim` (ADR-026): the plan
    /// the run has already, or this one, set once and never replaced. `None`
    /// when the claim was fenced off or the run is gone; commit only on
    /// `Some`.
    pub async fn freeze_run_plan(
        &mut self,
        claim: &Claim,
        run: DeploymentRunId,
        renderer_version: &str,
        capability_snapshot: &Value,
        resources: &Value,
    ) -> Result<Option<RenderPlanId>, StoreError> {
        let org = self.org.to_string();
        let existing: Option<Option<Uuid>> = sqlx::query_scalar(RUN_PLAN_ID)
            .bind(*run.as_uuid())
            .bind(&org)
            .fetch_optional(&mut *self.tx)
            .await?;
        match existing {
            None => return Ok(None),
            Some(Some(plan)) => return Ok(Some(RenderPlanId::from_uuid(plan))),
            Some(None) => {}
        }
        let plan = self
            .freeze_render_plan(renderer_version, capability_snapshot, resources)
            .await?;
        let rows = sqlx::query(SET_RUN_PLAN)
            .bind(*run.as_uuid())
            .bind(&org)
            .bind(*plan.as_uuid())
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok((rows == 1).then_some(plan))
    }

    /// The RenderPlan frozen for `run`, if any.
    pub async fn run_render_plan(&mut self, run: DeploymentRunId) -> Result<Option<RunPlan>, StoreError> {
        let row: Option<RunPlanRow> = sqlx::query_as(RUN_PLAN)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        let plan = |r: RunPlanRow| -> Result<RunPlan, sqlx::Error> {
            Ok(RunPlan {
                id: RenderPlanId::from_uuid(r.id),
                renderer_version: r.renderer_version,
                capability_snapshot: json(&r.capability_snapshot)?,
                resources: json(&r.resources)?,
            })
        };
        Ok(row.map(plan).transpose()?)
    }
}

impl Store {
    /// Record that the holder of `claim` wrote `m`'s target as the App object
    /// `resource_uid`, now at `metadata.generation` `resource_generation`.
    /// False when the claim was fenced off or a newer generation is recorded.
    pub async fn record_materialization(
        &self,
        claim: &Claim,
        m: &Materialization,
        resource_uid: &str,
        resource_generation: i64,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(RECORD)
            .bind(*m.target.as_uuid())
            .bind(m.org.to_string())
            .bind(*m.project.as_uuid())
            .bind(signed(m.generation.0)?)
            .bind(resource_uid)
            .bind(resource_generation)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The target whose App object has the Kubernetes UID `resource_uid`.
    pub async fn materialized_resource(
        &self,
        resource_uid: &str,
    ) -> Result<Option<Materialized>, StoreError> {
        let row: Option<MaterializedRow> = sqlx::query_as(MATERIALIZED_RESOURCE)
            .bind(resource_uid)
            .fetch_optional(self.pool())
            .await?;
        Ok(row.map(MaterializedRow::into_materialized).transpose()?)
    }

    /// Record `drift` found on the App object of `m`, and the UID and
    /// `metadata.generation` of the object that replaced it, if one did (a
    /// deleted object comes back with a new UID). False when the target has
    /// moved on to another generation since `m` was read.
    pub async fn record_drift(
        &self,
        m: &Materialized,
        replaced: Option<(&str, i64)>,
        drift: &Value,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(RECORD_DRIFT)
            .bind(*m.target.as_uuid())
            .bind(signed(m.generation.0)?)
            .bind(replaced.map(|(uid, _)| uid))
            .bind(replaced.map(|(_, generation)| generation))
            .bind(drift.to_string())
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use serde_json::json;

    use super::*;
    use crate::{
        repo::{NewAudit, PortableRelease, RUN_KIND, RunReason, StartDeployment, Started},
        testing::{pg_store, skip},
    };

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        target: TargetId,
        lifecycle_uid: Uuid,
        release: ReleaseId,
        revision: ConfigRevisionId,
    }

    async fn fixture(store: &Store, slug: &str) -> Fixture {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, &format!("kb-shop-production-{slug}"))
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: BTreeMap::from([("web".to_owned(), DIGEST.parse().expect("digest"))]),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: Some(json!({ "image_repository": "ghcr.io/acme/web" })),
                    created_by: "user:alice".into(),
                },
            )
            .await
            .expect("release");
        let config = json!({ "runtime": { "processes": { "web": { "port": 8080 } } } });
        let (revision, _) = t
            .create_config_revision(project, target, &config, "user:alice")
            .await
            .expect("revision")
            .expect("target");
        let lifecycle_uid = t
            .target_state(target)
            .await
            .expect("read")
            .expect("target")
            .lifecycle_uid;
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            target,
            lifecycle_uid,
            release,
            revision,
        }
    }

    /// Accept a deployment expecting `expected` and claim its operation.
    async fn deploy(store: &Store, f: &Fixture, expected: u64) -> Claim {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let started = t
            .start_deployment(
                &StartDeployment {
                    project: f.project,
                    target: f.target,
                    release: f.release,
                    config_revision: f.revision,
                    render_plan: None,
                    expected_generation: Generation(expected),
                    lifecycle_uid: f.lifecycle_uid,
                    reason: RunReason::Deploy,
                    requested_by: "user:alice".into(),
                    input_hash: format!("deploy-{expected}").into_bytes(),
                },
                NewAudit {
                    actor_kind: "user".into(),
                    action: "startDeployment".into(),
                    outcome: "accepted".into(),
                    ..NewAudit::default()
                },
                None,
            )
            .await
            .expect("start");
        assert!(matches!(started, Started::Accepted { .. }), "{started:?}");
        t.commit().await.expect("commit");
        store
            .claim_operation("materializer", &[RUN_KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("due")
    }

    async fn read(store: &Store, org: OrgId, operation: OperationId) -> Option<Materialization> {
        let mut t = store.tenant(org).await.expect("tenant");
        t.materialization(operation).await.expect("read")
    }

    #[tokio::test]
    async fn a_run_reads_everything_it_is_rendered_from() {
        let Some(store) = pg_store().await else {
            skip("materialization");
            return;
        };
        let f = fixture(&store, "a").await;
        let other = fixture(&store, "b").await;
        let claim = deploy(&store, &f, 0).await;

        let m = read(&store, f.org, claim.id).await.expect("the run");
        assert_eq!(m.operation, claim.id);
        assert_eq!(
            (m.generation, m.desired_generation),
            (Generation(1), Generation(1))
        );
        assert_eq!(m.phase, RunPhase::Planned);
        assert_eq!(
            (m.project_slug.as_str(), m.environment_slug.as_str()),
            ("shop", "production")
        );
        assert_eq!(
            (m.application_slug.as_str(), m.namespace.as_str()),
            ("web", "kb-shop-production-a")
        );
        assert!(m.protected && !m.deleting);
        assert_eq!(
            (m.target, m.release, m.lifecycle_uid),
            (f.target, f.release, f.lifecycle_uid)
        );
        assert_eq!(m.artifacts["web"].as_str(), DIGEST);
        assert_eq!(m.image_repository.as_deref(), Some("ghcr.io/acme/web"));
        assert_eq!((m.config_revision, m.config_revision_number), (f.revision, 1));
        assert_eq!(m.config["runtime"]["processes"]["web"]["port"], 8080);

        assert_eq!(
            read(&store, other.org, claim.id).await,
            None,
            "another organization's run"
        );
        assert_eq!(read(&store, f.org, OperationId::new()).await, None);
    }

    async fn app_record(store: &Store, org: OrgId, environment: EnvironmentId) -> crate::repo::AppRecord {
        let mut t = store.tenant(org).await.expect("tenant");
        t.app(environment, "web").await.expect("read").expect("app")
    }

    #[tokio::test]
    async fn an_agent_delivered_app_shows_what_its_agent_observed() {
        let Some(store) = pg_store().await else {
            skip("runtime status");
            return;
        };
        let f = fixture(&store, "a").await;
        let claim = deploy(&store, &f, 0).await;
        let m = read(&store, f.org, claim.id).await.expect("run");
        let before = app_record(&store, f.org, m.environment).await;
        assert_eq!((before.delivery.as_str(), before.runtime), ("controller", None));

        let caps = json!({ "sizes": [], "clusterIssuer": "letsencrypt" });
        let resources = json!([
            { "kind": "Deployment", "metadata": { "name": "web-web" } },
            {
                "kind": "HTTPRoute",
                "metadata": { "name": "web" },
                "spec": { "hostnames": ["shop.example.com", "web-production.apps.example.com"] }
            }
        ]);
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.freeze_run_plan(&claim, m.run, "kuben-renderer/1", &caps, &resources)
            .await
            .expect("freeze")
            .expect("frozen");
        // A target moves to its agent, never back (migration 0012).
        sqlx::query("UPDATE application_targets SET delivery = 'agent' WHERE id = $1")
            .bind(*f.target.as_uuid())
            .execute(&mut *t.tx)
            .await
            .expect("to the agent");
        assert!(
            t.record_runtime_observation(m.cluster, f.target, 1, "applying", None, None)
                .await
                .expect("record")
        );
        t.commit().await.expect("commit");

        let applying = app_record(&store, f.org, m.environment).await;
        assert_eq!(applying.delivery.as_str(), "agent");
        let runtime = applying.runtime.expect("observed");
        assert_eq!(
            (runtime.generation, runtime.phase.as_str(), runtime.ready()),
            (Generation(1), "applying", false)
        );
        assert_eq!(
            runtime.url.as_deref(),
            Some("https://shop.example.com"),
            "the first hostname of the observed plan's route"
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        assert!(
            t.record_runtime_observation(
                m.cluster,
                f.target,
                1,
                "failed",
                Some("ProgressDeadlineExceeded"),
                Some("web-web did not roll out")
            )
            .await
            .expect("record")
        );
        t.commit().await.expect("commit");
        let failed = app_record(&store, f.org, m.environment)
            .await
            .runtime
            .expect("observed");
        assert_eq!(
            (failed.phase.as_str(), failed.reason.as_deref()),
            ("failed", Some("ProgressDeadlineExceeded"))
        );
    }

    #[tokio::test]
    async fn a_run_plan_is_frozen_once_under_the_fence() {
        let Some(store) = pg_store().await else {
            skip("run plans");
            return;
        };
        let f = fixture(&store, "a").await;
        let claim = deploy(&store, &f, 0).await;
        let m = read(&store, f.org, claim.id).await.expect("run");
        assert_eq!(m.render_plan, None);
        let caps = json!({ "sizes": [] });
        let first = json!([{ "kind": "Deployment", "metadata": { "name": "web-web" } }]);

        let mut t = store.tenant(f.org).await.expect("tenant");
        let plan = t
            .freeze_run_plan(&claim, m.run, "kuben-renderer/1", &caps, &first)
            .await
            .expect("freeze")
            .expect("frozen");
        t.commit().await.expect("commit");
        assert_eq!(
            read(&store, f.org, claim.id).await.expect("run").render_plan,
            Some(plan)
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        let again = t
            .freeze_run_plan(&claim, m.run, "kuben-renderer/2", &caps, &json!([]))
            .await
            .expect("freeze");
        assert_eq!(again, Some(plan), "a frozen plan is never replaced");
        let frozen = t.run_render_plan(m.run).await.expect("read").expect("plan");
        assert_eq!(
            (frozen.id, frozen.renderer_version.as_str()),
            (plan, "kuben-renderer/1")
        );
        assert_eq!(
            (frozen.resources, frozen.capability_snapshot),
            (first.clone(), caps.clone())
        );
        drop(t);

        let next = deploy(&store, &f, 1).await;
        let m2 = read(&store, f.org, next.id).await.expect("run 2");
        let stale = Claim {
            fence: next.fence - 1,
            ..next.clone()
        };
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.freeze_run_plan(&stale, m2.run, "kuben-renderer/1", &caps, &first)
                .await
                .expect("freeze"),
            None,
            "a fenced-off worker freezes nothing"
        );
        drop(t);
        assert_eq!(
            read(&store, f.org, next.id).await.expect("run 2").render_plan,
            None
        );
    }

    #[tokio::test]
    async fn records_move_forward_under_the_fence() {
        let Some(store) = pg_store().await else {
            skip("materialization records");
            return;
        };
        let f = fixture(&store, "a").await;
        let older = deploy(&store, &f, 0).await;
        let newer = deploy(&store, &f, 1).await;
        let m_older = read(&store, f.org, older.id).await.expect("run 1");
        let m_newer = read(&store, f.org, newer.id).await.expect("run 2");

        assert!(
            store
                .record_materialization(&newer, &m_newer, "uid-1", 4)
                .await
                .expect("record")
        );
        assert!(
            !store
                .record_materialization(&older, &m_older, "uid-1", 5)
                .await
                .expect("record"),
            "a lower generation never replaces a newer record"
        );

        sqlx::query("UPDATE operations SET lease_until = 0 WHERE id = $1")
            .bind(*newer.id.as_uuid())
            .execute(store.pool())
            .await
            .expect("expire the lease");
        let taken = store
            .claim_operation("other", &[RUN_KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("taken over");
        assert_eq!(taken.id, newer.id);
        assert!(
            !store
                .record_materialization(&newer, &m_newer, "uid-1", 6)
                .await
                .expect("record"),
            "a fenced-off worker records nothing"
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        let recorded = t.materialized(f.target).await.expect("read").expect("recorded");
        assert_eq!(
            (
                recorded.generation,
                recorded.operation,
                recorded.resource_generation
            ),
            (Generation(2), newer.id, 4)
        );
        drop(t);
        assert!(
            sqlx::query("UPDATE target_materializations SET generation = 1")
                .execute(store.pool())
                .await
                .is_err(),
            "the recorded generation never decreases"
        );
    }

    #[tokio::test]
    async fn drift_is_recorded_against_the_written_generation() {
        let Some(store) = pg_store().await else {
            skip("drift records");
            return;
        };
        let f = fixture(&store, "a").await;
        let claim = deploy(&store, &f, 0).await;
        let m = read(&store, f.org, claim.id).await.expect("run");
        assert!(
            store
                .record_materialization(&claim, &m, "uid-1", 3)
                .await
                .expect("record")
        );

        let written = store
            .materialized_resource("uid-1")
            .await
            .expect("read")
            .expect("found by the object's UID");
        assert_eq!((written.org, written.target), (f.org, f.target));
        assert_eq!(store.materialized_resource("uid-2").await.expect("read"), None);

        let drift = json!({ "managers": ["kubectl-edit"] });
        assert!(
            store
                .record_drift(&written, Some(("uid-2", 5)), &drift)
                .await
                .expect("drift")
        );
        let stale = Materialized {
            generation: Generation(2),
            ..written.clone()
        };
        assert!(
            !store.record_drift(&stale, None, &drift).await.expect("drift"),
            "drift of another generation"
        );
        assert_eq!(
            store.materialized_resource("uid-1").await.expect("read"),
            None,
            "the object that replaced the drifted one has a new UID"
        );
        let now = store
            .materialized_resource("uid-2")
            .await
            .expect("read")
            .expect("found by the replacement's UID");
        assert_eq!((now.drift_count, now.resource_generation), (1, 5));
        assert_eq!(now.drift, Some(drift));
        assert!(now.drift_detected_at.is_some());
    }
}

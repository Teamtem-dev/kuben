//! Export and detach (M4.11, migration 0029): what an app's export is made
//! of, and the record of apps handed over out of Kuben.

use kuben_core::{
    ids::{DeploymentRunId, EnvironmentId, ProjectId, TargetId},
    time::now_ms,
};
use serde_json::Value;
use uuid::Uuid;

use super::Tenant;
use crate::StoreError;

const EXPORT_RUN: &str = "SELECT r.id, r.generation, r.phase, r.reason, r.release_id, \
     rel.artifacts::text AS artifacts, rel.process_contract::text AS process_contract, \
     rel.portable_config::text AS portable_config, rel.source::text AS source, \
     c.revision AS config_revision, c.config::text AS config, \
     p.renderer_version, p.capability_snapshot::text AS capabilities, p.resources::text AS resources \
     FROM deployment_runs r \
     JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id \
     JOIN target_config_revisions c ON c.id = r.config_revision_id AND c.org_id = r.org_id \
     LEFT JOIN render_plans p ON p.id = r.render_plan_id AND p.org_id = r.org_id \
     WHERE r.target_id = $1 AND r.org_id = $2 AND r.render_plan_id IS NOT NULL \
     ORDER BY (r.phase = 'succeeded') DESC, r.generation DESC LIMIT 1";
const IN_FLIGHT: &str = "SELECT EXISTS (SELECT 1 FROM deployment_runs r \
     JOIN operations o ON o.id = r.operation_id \
     WHERE r.target_id = $1 AND r.org_id = $2 AND NOT o.done)";
const RUN_SECRETS: &str = "SELECT s.name, b.revision, s.kind, s.registry FROM run_secret_bindings b \
     JOIN secrets s ON s.id = b.secret_id AND s.org_id = b.org_id \
     WHERE b.run_id = $1 AND b.org_id = $2 ORDER BY s.name";
const INSERT_DETACHED: &str = "INSERT INTO detached_apps \
     (target_id, org_id, project_id, environment_id, app, namespace, reason, requested_by, requested_at, export) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)";
const COMPLETE_DETACHED: &str = "UPDATE detached_apps SET completed_at = $3 \
     WHERE target_id = $1 AND org_id = $2 AND completed_at IS NULL";
const RELEASE_DETACHED: &str = "UPDATE detached_apps SET released_at = $3, released_by = $4 \
     WHERE target_id = $1 AND org_id = $2 AND released_at IS NULL";
/// The detach records, with a filter.
macro_rules! detached {
    ($tail:literal) => {
        concat!(
            "SELECT target_id, project_id, environment_id, app, namespace, reason, requested_by, \
             requested_at, completed_at, released_at, released_by FROM detached_apps",
            $tail
        )
    };
}
const DETACHED_ONE: &str = detached!(" WHERE target_id = $1 AND org_id = $2");
const DETACHED_IN: &str = detached!(" WHERE environment_id = $1 AND org_id = $2 ORDER BY requested_at DESC");
const HELD: &str = "SELECT count(*) FROM detached_apps \
     WHERE environment_id = $1 AND org_id = $2 AND released_at IS NULL";
const EXPORT_OF: &str = "SELECT export::text FROM detached_apps WHERE target_id = $1 AND org_id = $2";

/// The newest delivered run of an app: what its export is made of.
#[derive(Clone, Debug, PartialEq)]
pub struct ExportMaterial {
    pub run: DeploymentRunId,
    pub generation: i64,
    pub phase: String,
    pub reason: String,
    pub release: Uuid,
    pub artifacts: Value,
    pub process_contract: Value,
    pub portable_config: Value,
    pub source: Option<Value>,
    pub config_revision: i64,
    pub config: Value,
    pub renderer_version: String,
    pub capabilities: Value,
    /// The frozen render plan: Kubernetes objects in apply order.
    pub resources: Value,
    pub secrets: Vec<SecretReference>,
}

/// A secret revision a run uses; its object is `<name>.r<revision>`.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct SecretReference {
    pub name: String,
    pub revision: i64,
    /// `opaque`, or `registry` for a pull secret.
    pub kind: String,
    pub registry: Option<String>,
}

impl SecretReference {
    /// The name of the Kubernetes Secret holding the revision.
    #[must_use]
    pub fn object(&self) -> String {
        format!("{}.r{}", self.name, self.revision)
    }
}

#[derive(sqlx::FromRow)]
struct ExportRow {
    id: Uuid,
    generation: i64,
    phase: String,
    reason: String,
    release_id: Uuid,
    artifacts: String,
    process_contract: String,
    portable_config: String,
    source: Option<String>,
    config_revision: i64,
    config: String,
    renderer_version: Option<String>,
    capabilities: Option<String>,
    resources: Option<String>,
}

/// A detach request.
#[derive(Clone, Debug, PartialEq)]
pub struct NewDetach<'a> {
    pub project: ProjectId,
    pub environment: EnvironmentId,
    pub target: TargetId,
    pub app: &'a str,
    pub namespace: &'a str,
    pub reason: &'a str,
    pub requested_by: &'a str,
    pub export: &'a Value,
}

/// An app detached from Kuben.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct DetachedApp {
    pub target_id: Uuid,
    pub project_id: Uuid,
    pub environment_id: Uuid,
    pub app: String,
    pub namespace: String,
    pub reason: String,
    pub requested_by: String,
    pub requested_at: i64,
    pub completed_at: Option<i64>,
    pub released_at: Option<i64>,
    pub released_by: Option<String>,
}

fn json(text: &str) -> Result<Value, sqlx::Error> {
    serde_json::from_str(text).map_err(|e| sqlx::Error::Decode(e.into()))
}

impl Tenant {
    /// What `target`'s export is made of: its newest succeeded run with a
    /// frozen plan, else its newest run with one; `None` before any.
    pub async fn export_material(&mut self, target: TargetId) -> Result<Option<ExportMaterial>, StoreError> {
        let org = self.org.to_string();
        let row: Option<ExportRow> = sqlx::query_as(EXPORT_RUN)
            .bind(*target.as_uuid())
            .bind(&org)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(r) = row else {
            return Ok(None);
        };
        let secrets = sqlx::query_as(RUN_SECRETS)
            .bind(r.id)
            .bind(&org)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(Some(ExportMaterial {
            run: DeploymentRunId::from_uuid(r.id),
            generation: r.generation,
            phase: r.phase,
            reason: r.reason,
            release: r.release_id,
            artifacts: json(&r.artifacts)?,
            process_contract: json(&r.process_contract)?,
            portable_config: json(&r.portable_config)?,
            source: r.source.as_deref().map(json).transpose()?,
            config_revision: r.config_revision,
            config: json(&r.config)?,
            renderer_version: r.renderer_version.unwrap_or_default(),
            capabilities: r
                .capabilities
                .as_deref()
                .map(json)
                .transpose()?
                .unwrap_or(Value::Null),
            resources: r
                .resources
                .as_deref()
                .map(json)
                .transpose()?
                .unwrap_or(Value::Null),
            secrets,
        }))
    }

    /// Whether a run of `target` has not finished.
    pub async fn run_in_flight(&mut self, target: TargetId) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(IN_FLIGHT)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// Record a detach request (the target is marked deleting separately).
    pub async fn record_detach(&mut self, d: &NewDetach<'_>) -> Result<(), StoreError> {
        sqlx::query(INSERT_DETACHED)
            .bind(*d.target.as_uuid())
            .bind(self.org.to_string())
            .bind(*d.project.as_uuid())
            .bind(*d.environment.as_uuid())
            .bind(d.app)
            .bind(d.namespace)
            .bind(d.reason)
            .bind(d.requested_by)
            .bind(now_ms())
            .bind(d.export.to_string())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The detach record of `target`, if it was detached.
    pub async fn detached_app(&mut self, target: TargetId) -> Result<Option<DetachedApp>, StoreError> {
        Ok(sqlx::query_as(DETACHED_ONE)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// The apps detached from `environment`, newest first.
    pub async fn detached_apps(
        &mut self,
        environment: EnvironmentId,
    ) -> Result<Vec<DetachedApp>, StoreError> {
        Ok(sqlx::query_as(DETACHED_IN)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// The export frozen when `target` was detached.
    pub async fn detached_export(&mut self, target: TargetId) -> Result<Option<Value>, StoreError> {
        let text: Option<String> = sqlx::query_scalar(EXPORT_OF)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(text.as_deref().map(json).transpose()?)
    }

    /// `target`'s objects are orphaned: the detach is complete.
    pub async fn complete_detach(&mut self, target: TargetId) -> Result<bool, StoreError> {
        let rows = sqlx::query(COMPLETE_DETACHED)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Someone took over the detached `target` for good: its environment's
    /// deletion no longer keeps the namespace for it. False when it was
    /// released already or is not detached.
    pub async fn release_detached(&mut self, target: TargetId, by: &str) -> Result<bool, StoreError> {
        let rows = sqlx::query(RELEASE_DETACHED)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Unreleased detached apps in `environment`.
    pub async fn detached_held(&mut self, environment: EnvironmentId) -> Result<u64, StoreError> {
        let n: i64 = sqlx::query_scalar(HELD)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(u64::try_from(n).unwrap_or_default())
    }
}

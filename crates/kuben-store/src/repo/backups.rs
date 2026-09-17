//! Backups, restores and the database's own facts (M4.7, migration 0025).
//!
//! After a restore the installation must not trust what happened after the
//! backup was taken: [`Store::after_restore`] ends every session, revokes
//! every API token, and raises every target's generation far beyond any the
//! clusters may have seen, so envelopes and objects written in the lost
//! interval are outranked instead of mistaken for newer state.

use std::time::Duration;

use kuben_core::{
    ids::{ConfigRevisionId, OrgId, ProjectId, ReleaseId, TargetId},
    ops::Generation,
    time::now_ms,
};
use sqlx::postgres::PgPoolOptions;
use uuid::Uuid;

use super::{NewAudit, RunReason, StartDeployment, Started, product::counter};
use crate::{Store, StoreError};

/// How far a restore raises every generation.
pub const RESTORE_GENERATION_JUMP: i64 = 1 << 20;

const INSERT_BACKUP: &str = "INSERT INTO backup_runs \
     (id, kind, status, location, bytes, sha256, kuben, schema, detail, started_at, finished_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)";
const LAST_GOOD: &str = "SELECT location, bytes, sha256, finished_at FROM backup_runs \
     WHERE status = 'succeeded' ORDER BY finished_at DESC LIMIT 1";
const SCHEMA: &str = "SELECT COALESCE(max(version), 0) FROM _sqlx_migrations WHERE success";
const TABLES: &str = "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()";
const FACTS: &str = "SELECT current_setting('server_version_num')::int, \
     COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false), \
     current_setting('wal_level'), current_setting('archive_mode'), pg_is_in_recovery()";
const END_SESSIONS: &str = "UPDATE sessions SET revoked_at = $1 WHERE revoked_at IS NULL";
const REVOKE_TOKENS: &str = "UPDATE api_tokens SET revoked_at = $1 WHERE revoked_at IS NULL";
const RAISE_GENERATIONS: &str = "UPDATE application_targets \
     SET desired_generation = desired_generation + $2 WHERE org_id = $1";
/// Every live target with the release and configuration of its newest run.
const REDELIVERABLE: &str = "SELECT t.project_id, t.id, r.release_id, r.config_revision_id, \
     t.desired_generation, t.lifecycle_uid FROM application_targets t \
     JOIN LATERAL (SELECT d.release_id, d.config_revision_id FROM deployment_runs d \
       WHERE d.target_id = t.id AND d.org_id = t.org_id ORDER BY d.generation DESC LIMIT 1) r ON TRUE \
     WHERE t.org_id = $1 AND NOT t.deleting";
const INSERT_RESTORE: &str = "INSERT INTO restore_events \
     (id, backup_created_at, backup_kuben, kuben, generation_jump, restored_at) \
     VALUES ($1, $2, $3, $4, $5, $6)";

/// A backup written.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BackupRecord {
    pub scheduled: bool,
    pub succeeded: bool,
    pub location: String,
    pub bytes: Option<u64>,
    pub sha256: Option<String>,
    pub kuben: String,
    pub schema: i64,
    pub detail: Option<String>,
    pub started_at: i64,
    pub finished_at: i64,
}

/// The newest good backup.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LastBackup {
    pub location: String,
    pub bytes: Option<u64>,
    pub sha256: Option<String>,
    pub finished_at: i64,
}

/// What the database server says about itself.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DatabaseFacts {
    /// `server_version_num`, e.g. 170004.
    pub version_num: i32,
    /// This connection is encrypted.
    pub tls: bool,
    pub wal_level: String,
    pub archive_mode: String,
    /// A standby: Kuben cannot write here.
    pub in_recovery: bool,
}

impl DatabaseFacts {
    /// The major version, e.g. 17.
    #[must_use]
    pub const fn major(&self) -> i32 {
        self.version_num / 10_000
    }

    /// WAL is archived, so a point-in-time recovery is possible.
    #[must_use]
    pub fn archives_wal(&self) -> bool {
        matches!(self.archive_mode.as_str(), "on" | "always") && self.wal_level != "minimal"
    }
}

/// What a restore changed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Restored {
    pub sessions_ended: u64,
    pub tokens_revoked: u64,
    pub targets_raised: u64,
}

impl Store {
    /// Whether the database at `url` has no tables yet (never migrated).
    pub async fn database_is_empty(url: &str) -> Result<bool, StoreError> {
        let pool = PgPoolOptions::new()
            .max_connections(1)
            .acquire_timeout(Duration::from_secs(10))
            .connect(url)
            .await?;
        let tables: i64 = sqlx::query_scalar(TABLES).fetch_one(&pool).await?;
        pool.close().await;
        Ok(tables == 0)
    }

    /// The newest migration applied.
    pub async fn schema_version(&self) -> Result<i64, StoreError> {
        Ok(sqlx::query_scalar(SCHEMA).fetch_one(self.pool()).await?)
    }

    /// The database server's version, encryption and WAL archiving.
    pub async fn database_facts(&self) -> Result<DatabaseFacts, StoreError> {
        let (version_num, tls, wal_level, archive_mode, in_recovery): (i32, bool, String, String, bool) =
            sqlx::query_as(FACTS).fetch_one(self.pool()).await?;
        Ok(DatabaseFacts {
            version_num,
            tls,
            wal_level,
            archive_mode,
            in_recovery,
        })
    }

    /// Record a backup attempt.
    pub async fn record_backup(&self, b: &BackupRecord) -> Result<(), StoreError> {
        let bytes = b
            .bytes
            .map(i64::try_from)
            .transpose()
            .map_err(|e| sqlx::Error::Encode(e.into()))?;
        sqlx::query(INSERT_BACKUP)
            .bind(Uuid::now_v7())
            .bind(if b.scheduled { "scheduled" } else { "manual" })
            .bind(if b.succeeded { "succeeded" } else { "failed" })
            .bind(&b.location)
            .bind(bytes)
            .bind(b.sha256.as_deref())
            .bind(&b.kuben)
            .bind(b.schema)
            .bind(b.detail.as_deref())
            .bind(b.started_at)
            .bind(b.finished_at)
            .execute(self.pool())
            .await?;
        Ok(())
    }

    /// The newest good backup, if any.
    pub async fn last_backup(&self) -> Result<Option<LastBackup>, StoreError> {
        let row: Option<(String, Option<i64>, Option<String>, i64)> =
            sqlx::query_as(LAST_GOOD).fetch_optional(self.pool()).await?;
        row.map(|(location, bytes, sha256, finished_at)| {
            Ok(LastBackup {
                location,
                bytes: bytes.map(counter).transpose()?,
                sha256,
                finished_at,
            })
        })
        .transpose()
    }

    /// Fence everything that happened after the restored backup was taken,
    /// in one transaction per step: sessions, API tokens, and generations of
    /// every organization's targets.
    pub async fn after_restore(
        &self,
        backup_created_at: i64,
        backup_kuben: &str,
        kuben: &str,
    ) -> Result<Restored, StoreError> {
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        let sessions_ended = sqlx::query(END_SESSIONS)
            .bind(now)
            .execute(&mut *tx)
            .await?
            .rows_affected();
        let tokens_revoked = sqlx::query(REVOKE_TOKENS)
            .bind(now)
            .execute(&mut *tx)
            .await?
            .rows_affected();
        sqlx::query(INSERT_RESTORE)
            .bind(Uuid::now_v7())
            .bind(backup_created_at)
            .bind(backup_kuben)
            .bind(kuben)
            .bind(RESTORE_GENERATION_JUMP)
            .bind(now)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        let mut targets_raised = 0;
        for org in self.org_ids().await? {
            targets_raised += self.raise_generations(org).await?;
        }
        Ok(Restored {
            sessions_ended,
            tokens_revoked,
            targets_raised,
        })
    }

    /// Start a run of every live target's newest release and configuration,
    /// so the restored state is written again (to a fresh cluster, too).
    /// Runs are restarts: they carry nothing new, so neither approvals nor
    /// the scan gate hold them. The number started.
    pub async fn redeliver_all(&self, requested_by: &str) -> Result<usize, StoreError> {
        let mut started = 0;
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            let rows: Vec<(Uuid, Uuid, Uuid, Uuid, i64, Uuid)> = sqlx::query_as(REDELIVERABLE)
                .bind(org.to_string())
                .fetch_all(&mut *tenant.tx)
                .await?;
            for (project, target, release, config, generation, lifecycle_uid) in rows {
                let req = StartDeployment {
                    project: ProjectId::from_uuid(project),
                    target: TargetId::from_uuid(target),
                    release: ReleaseId::from_uuid(release),
                    config_revision: ConfigRevisionId::from_uuid(config),
                    render_plan: None,
                    expected_generation: Generation(counter(generation)?),
                    lifecycle_uid,
                    reason: RunReason::Restart,
                    requested_by: requested_by.to_owned(),
                    input_hash: format!("restore/{target}/{generation}").into_bytes(),
                };
                let audit = NewAudit {
                    actor_kind: "system".into(),
                    actor_id: Some(requested_by.to_owned()),
                    action: "deployment.redelivered".into(),
                    target_kind: Some("app".into()),
                    target_ref: Some(target.to_string()),
                    outcome: "accepted".into(),
                    ..NewAudit::default()
                };
                if matches!(
                    tenant.start_deployment(&req, audit, None).await?,
                    Started::Accepted { .. }
                ) {
                    started += 1;
                }
            }
            tenant.commit().await?;
        }
        Ok(started)
    }

    async fn raise_generations(&self, org: OrgId) -> Result<u64, StoreError> {
        let mut tenant = self.tenant(org).await?;
        let raised = sqlx::query(RAISE_GENERATIONS)
            .bind(org.to_string())
            .bind(RESTORE_GENERATION_JUMP)
            .execute(&mut *tenant.tx)
            .await?
            .rows_affected();
        tenant.commit().await?;
        Ok(raised)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{pg_store, skip};

    #[test]
    fn wal_archiving_needs_a_mode_and_a_level() {
        let facts = |level: &str, mode: &str| DatabaseFacts {
            version_num: 170_004,
            tls: false,
            wal_level: level.into(),
            archive_mode: mode.into(),
            in_recovery: false,
        };
        assert_eq!(facts("replica", "on").major(), 17);
        assert!(facts("replica", "on").archives_wal());
        assert!(facts("logical", "always").archives_wal());
        assert!(!facts("replica", "off").archives_wal());
        assert!(!facts("minimal", "on").archives_wal());
    }

    #[tokio::test]
    async fn backups_are_recorded_and_restores_fence_the_past() {
        let Some(store) = pg_store().await else {
            return skip("backups_are_recorded_and_restores_fence_the_past");
        };
        assert!(store.schema_version().await.expect("schema") >= 25);
        let facts = store.database_facts().await.expect("facts");
        assert!(facts.major() >= 15 && !facts.in_recovery);
        assert_eq!(store.last_backup().await.expect("read"), None);
        let record = |succeeded: bool, at: i64| BackupRecord {
            scheduled: true,
            succeeded,
            location: format!("/backups/{at}"),
            bytes: succeeded.then_some(1024),
            sha256: succeeded.then(|| "a".repeat(64)),
            kuben: "2.0.0".into(),
            schema: 25,
            detail: (!succeeded).then(|| "pg_dump failed".into()),
            started_at: at - 5,
            finished_at: at,
        };
        store.record_backup(&record(true, 100)).await.expect("backup");
        store.record_backup(&record(false, 200)).await.expect("backup");
        let last = store.last_backup().await.expect("read").expect("backup");
        assert_eq!(
            (last.location.as_str(), last.bytes, last.finished_at),
            ("/backups/100", Some(1024), 100)
        );

        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "prod", "Prod", false)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, "a-shop")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        t.commit().await.expect("commit");

        let restored = store.after_restore(50, "1.9.0", "2.0.0").await.expect("restore");
        assert_eq!(restored.targets_raised, 1);
        let mut t = store.tenant(org).await.expect("tenant");
        let state = t.target_state(target).await.expect("read").expect("target");
        assert_eq!(
            state.desired_generation.0,
            u64::try_from(RESTORE_GENERATION_JUMP).expect("jump")
        );
    }
}

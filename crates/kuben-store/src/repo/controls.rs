//! Owners, freezes, pauses and silences (M4.9, migration 0027).

use kuben_core::{
    ids::{ApplicationId, ConfigRevisionId, EnvironmentId, ProjectId, ReleaseId, TargetId},
    time::now_ms,
};
use uuid::Uuid;

use super::Tenant;
use crate::StoreError;

const OWNER: &str = "SELECT owner, contact, runbook_url, updated_by, updated_at FROM owners \
     WHERE org_id = $1 AND project_id = $2 AND application_id IS NOT DISTINCT FROM $3";
const DELETE_OWNER: &str = "DELETE FROM owners \
     WHERE org_id = $1 AND project_id = $2 AND application_id IS NOT DISTINCT FROM $3";
const INSERT_OWNER: &str = "INSERT INTO owners \
     (org_id, project_id, application_id, owner, contact, runbook_url, updated_by, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8)";
const TARGET_ENVIRONMENT: &str = "SELECT p.environment_id FROM application_targets t \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     WHERE t.id = $1 AND t.org_id = $2";
const ACTIVE_FREEZE: &str = "SELECT reason FROM environment_freezes \
     WHERE environment_id = $1 AND org_id = $2 AND lifted_at IS NULL AND starts_at <= $3 AND ends_at > $3 \
     ORDER BY ends_at DESC LIMIT 1";
const FREEZES: &str = "SELECT id, reason, created_by, created_at, starts_at, ends_at, lifted_at, lifted_by \
     FROM environment_freezes WHERE environment_id = $1 AND org_id = $2 \
       AND ($3 OR (lifted_at IS NULL AND ends_at > $4)) ORDER BY starts_at DESC LIMIT 200";
const INSERT_FREEZE: &str = "INSERT INTO environment_freezes \
     (id, org_id, project_id, environment_id, reason, created_by, created_at, starts_at, ends_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const LIFT_FREEZE: &str = "UPDATE environment_freezes SET lifted_at = $4, lifted_by = $5 \
     WHERE id = $1 AND environment_id = $2 AND org_id = $3 AND lifted_at IS NULL AND ends_at > $4";
const SILENCES: &str = "SELECT id, target_id, reason, created_by, created_at, ends_at, lifted_at, lifted_by \
     FROM silences WHERE environment_id = $1 AND org_id = $2 \
       AND ($3 OR (lifted_at IS NULL AND ends_at > $4)) ORDER BY created_at DESC LIMIT 200";
const INSERT_SILENCE: &str = "INSERT INTO silences \
     (id, org_id, project_id, environment_id, target_id, reason, created_by, created_at, ends_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)";
const LIFT_SILENCE: &str = "UPDATE silences SET lifted_at = $4, lifted_by = $5 \
     WHERE id = $1 AND environment_id = $2 AND org_id = $3 AND lifted_at IS NULL AND ends_at > $4";
const SILENCED: &str = "SELECT EXISTS (SELECT 1 FROM silences \
     WHERE environment_id = $1 AND org_id = $2 AND lifted_at IS NULL AND ends_at > $4 \
       AND (target_id IS NULL OR target_id = $3))";
const PAUSE: &str = "UPDATE application_targets SET paused_at = $3, paused_by = $4, pause_reason = $5 \
     WHERE id = $1 AND org_id = $2 AND paused_at IS NULL AND NOT deleting";
const RESUME: &str = "UPDATE application_targets SET paused_at = NULL, paused_by = NULL, pause_reason = NULL \
     WHERE id = $1 AND org_id = $2 AND paused_at IS NOT NULL";
const WAKE_TARGET: &str = "UPDATE operations SET next_attempt_at = kuben_now_ms() \
     WHERE target_id = $1 AND NOT done AND next_attempt_at > kuben_now_ms()";
/// The newest successful run of `$1` with release `$3`, or, without one, of
/// a release other than the one its newest run carries.
const ROLLBACK_POINT: &str = "SELECT d.release_id, d.config_revision_id FROM deployment_runs d \
     WHERE d.target_id = $1 AND d.org_id = $2 AND d.phase = 'succeeded' \
       AND (($3::uuid IS NULL AND d.release_id IS DISTINCT FROM (SELECT n.release_id FROM deployment_runs n \
             WHERE n.target_id = $1 AND n.org_id = $2 ORDER BY n.generation DESC LIMIT 1)) \
            OR d.release_id = $3::uuid) \
     ORDER BY d.generation DESC LIMIT 1";
const PAUSED: &str = "SELECT paused_at IS NOT NULL FROM application_targets WHERE id = $1 AND org_id = $2";

/// Who answers for a project or an application.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Owner {
    pub owner: String,
    pub contact: Option<String>,
    pub runbook_url: Option<String>,
    pub updated_by: String,
    pub updated_at: i64,
}

/// A change freeze of an environment.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct Freeze {
    pub id: Uuid,
    pub reason: String,
    pub created_by: String,
    pub created_at: i64,
    pub starts_at: i64,
    pub ends_at: i64,
    pub lifted_at: Option<i64>,
    pub lifted_by: Option<String>,
}

/// A silence of an environment's alerts, or one app's.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct Silence {
    pub id: Uuid,
    pub target_id: Option<Uuid>,
    pub reason: String,
    pub created_by: String,
    pub created_at: i64,
    pub ends_at: i64,
    pub lifted_at: Option<i64>,
    pub lifted_by: Option<String>,
}

/// A freeze or silence to make.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewWindow {
    pub project: ProjectId,
    pub environment: EnvironmentId,
    /// A silence of one app only.
    pub target: Option<TargetId>,
    pub reason: String,
    pub created_by: String,
    pub starts_at: i64,
    pub ends_at: i64,
}

impl Tenant {
    /// The owner of `project`, or of its `application`.
    pub async fn owner(
        &mut self,
        project: ProjectId,
        application: Option<ApplicationId>,
    ) -> Result<Option<Owner>, StoreError> {
        let row: Option<(String, Option<String>, Option<String>, String, i64)> = sqlx::query_as(OWNER)
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .bind(application.map(|a| *a.as_uuid()))
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(
            row.map(|(owner, contact, runbook_url, updated_by, updated_at)| Owner {
                owner,
                contact,
                runbook_url,
                updated_by,
                updated_at,
            }),
        )
    }

    /// Set (or, with `None`, clear) the owner of `project` or its
    /// `application`.
    pub async fn set_owner(
        &mut self,
        project: ProjectId,
        application: Option<ApplicationId>,
        owner: Option<(&str, Option<&str>, Option<&str>)>,
        by: &str,
    ) -> Result<(), StoreError> {
        let org = self.org.to_string();
        sqlx::query(DELETE_OWNER)
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(application.map(|a| *a.as_uuid()))
            .execute(&mut *self.tx)
            .await?;
        if let Some((owner, contact, runbook)) = owner {
            sqlx::query(INSERT_OWNER)
                .bind(&org)
                .bind(*project.as_uuid())
                .bind(application.map(|a| *a.as_uuid()))
                .bind(owner)
                .bind(contact)
                .bind(runbook)
                .bind(by)
                .bind(now_ms())
                .execute(&mut *self.tx)
                .await?;
        }
        Ok(())
    }

    async fn environment_of(&mut self, target: TargetId) -> Result<Option<Uuid>, StoreError> {
        Ok(sqlx::query_scalar(TARGET_ENVIRONMENT)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// The reason of the freeze on `target`'s environment at `now`, if any.
    pub async fn active_freeze(&mut self, target: TargetId, now: i64) -> Result<Option<String>, StoreError> {
        let Some(environment) = self.environment_of(target).await? else {
            return Ok(None);
        };
        Ok(sqlx::query_scalar(ACTIVE_FREEZE)
            .bind(environment)
            .bind(self.org.to_string())
            .bind(now)
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// The freezes of `environment` in force or to come, or all of them.
    pub async fn freezes(
        &mut self,
        environment: EnvironmentId,
        all: bool,
    ) -> Result<Vec<Freeze>, StoreError> {
        Ok(sqlx::query_as(FREEZES)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(all)
            .bind(now_ms())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Freeze an environment for the window of `w`.
    pub async fn create_freeze(&mut self, w: &NewWindow) -> Result<Uuid, StoreError> {
        let id = Uuid::now_v7();
        sqlx::query(INSERT_FREEZE)
            .bind(id)
            .bind(self.org.to_string())
            .bind(*w.project.as_uuid())
            .bind(*w.environment.as_uuid())
            .bind(&w.reason)
            .bind(&w.created_by)
            .bind(now_ms())
            .bind(w.starts_at)
            .bind(w.ends_at)
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// Lift freeze `id` of `environment` early. False when it is not in
    /// force or to come.
    pub async fn lift_freeze(
        &mut self,
        environment: EnvironmentId,
        id: Uuid,
        by: &str,
    ) -> Result<bool, StoreError> {
        self.lift(LIFT_FREEZE, environment, id, by).await
    }

    async fn lift(
        &mut self,
        sql: &'static str,
        environment: EnvironmentId,
        id: Uuid,
        by: &str,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(sql)
            .bind(id)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The silences of `environment` in force, or all of them.
    pub async fn silences(
        &mut self,
        environment: EnvironmentId,
        all: bool,
    ) -> Result<Vec<Silence>, StoreError> {
        Ok(sqlx::query_as(SILENCES)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(all)
            .bind(now_ms())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Silence an environment's alerts, or one app's, until `w.ends_at`.
    pub async fn create_silence(&mut self, w: &NewWindow) -> Result<Uuid, StoreError> {
        let id = Uuid::now_v7();
        sqlx::query(INSERT_SILENCE)
            .bind(id)
            .bind(self.org.to_string())
            .bind(*w.project.as_uuid())
            .bind(*w.environment.as_uuid())
            .bind(w.target.map(|t| *t.as_uuid()))
            .bind(&w.reason)
            .bind(&w.created_by)
            .bind(now_ms())
            .bind(w.ends_at)
            .execute(&mut *self.tx)
            .await?;
        Ok(id)
    }

    /// Lift silence `id` of `environment` early.
    pub async fn lift_silence(
        &mut self,
        environment: EnvironmentId,
        id: Uuid,
        by: &str,
    ) -> Result<bool, StoreError> {
        self.lift(LIFT_SILENCE, environment, id, by).await
    }

    /// Whether alerts about `target` (or its whole environment) are silenced
    /// at `now`.
    pub async fn silenced(
        &mut self,
        environment: EnvironmentId,
        target: Option<TargetId>,
        now: i64,
    ) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(SILENCED)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(target.map(|t| *t.as_uuid()))
            .bind(now)
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// Hold the delivery of `target`. False when it is paused already, being
    /// deleted, or not the organization's.
    pub async fn pause_target(
        &mut self,
        target: TargetId,
        by: &str,
        reason: &str,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(PAUSE)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(by)
            .bind(reason)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Resume the delivery of `target`; its waiting runs are woken, and only
    /// the newest of them writes (older ones are superseded). False when it
    /// was not paused.
    pub async fn resume_target(&mut self, target: TargetId) -> Result<bool, StoreError> {
        let rows = sqlx::query(RESUME)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        if rows == 1 {
            sqlx::query(WAKE_TARGET)
                .bind(*target.as_uuid())
                .execute(&mut *self.tx)
                .await?;
        }
        Ok(rows == 1)
    }

    /// The release and configuration an emergency rollback of `target`
    /// returns to: `release` as it last ran successfully, or the newest
    /// successful release other than the current one.
    pub async fn rollback_point(
        &mut self,
        target: TargetId,
        release: Option<ReleaseId>,
    ) -> Result<Option<(ReleaseId, ConfigRevisionId)>, StoreError> {
        let row: Option<(Uuid, Uuid)> = sqlx::query_as(ROLLBACK_POINT)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(release.map(|r| *r.as_uuid()))
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(|(r, c)| (ReleaseId::from_uuid(r), ConfigRevisionId::from_uuid(c))))
    }

    /// Whether the delivery of `target` is held now.
    pub async fn target_paused(&mut self, target: TargetId) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(PAUSED)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?
            .unwrap_or(false))
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{ids::OrgId, ops::RunPhase};
    use serde_json::json;

    use super::*;
    use crate::{
        Store,
        repo::{NewAudit, PortableRelease, RunReason, StartDeployment, Started},
        testing::{pg_store, skip},
    };

    const DIGEST_A: &str = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const DIGEST_B: &str = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
    const BY: &str = "user:a";

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        environment: EnvironmentId,
        application: ApplicationId,
        target: TargetId,
    }

    async fn fixture(store: &Store) -> Fixture {
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "prod", "Prod", false)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, "a-shop")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            environment,
            application,
            target,
        }
    }

    async fn release(t: &mut Tenant, f: &Fixture, digest: &str) -> ReleaseId {
        t.create_release(
            f.project,
            &PortableRelease {
                application: f.application,
                artifacts: [("web".to_owned(), digest.parse().expect("digest"))].into(),
                process_contract: json!({}),
                portable_config: json!({}),
                renderer_schema: 1,
                source: None,
                created_by: BY.into(),
            },
        )
        .await
        .expect("release")
        .0
    }

    fn request(
        f: &Fixture,
        release: ReleaseId,
        config: ConfigRevisionId,
        expected: u64,
        uid: Uuid,
    ) -> StartDeployment {
        StartDeployment {
            project: f.project,
            target: f.target,
            release,
            config_revision: config,
            render_plan: None,
            expected_generation: kuben_core::ops::Generation(expected),
            lifecycle_uid: uid,
            reason: RunReason::Deploy,
            requested_by: BY.into(),
            input_hash: format!("{release}/{expected}").into_bytes(),
        }
    }

    /// Releases `old` (ran successfully) and `new` (the newest run), the
    /// configuration and the lifecycle UID; the target is at generation 2.
    async fn two_releases(store: &Store, f: &Fixture) -> (ReleaseId, ReleaseId, ConfigRevisionId, Uuid) {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let (config, _) = t
            .create_config_revision(f.project, f.target, &json!({}), BY)
            .await
            .expect("revision")
            .expect("target");
        let uid = t
            .target_state(f.target)
            .await
            .expect("read")
            .expect("target")
            .lifecycle_uid;
        let old = release(&mut t, f, DIGEST_A).await;
        let new = release(&mut t, f, DIGEST_B).await;
        let Started::Accepted { run, .. } = t
            .start_deployment(&request(f, old, config, 0, uid), NewAudit::default(), None)
            .await
            .expect("start")
        else {
            panic!("not accepted");
        };
        sqlx::query("UPDATE deployment_runs SET phase = 'succeeded' WHERE id = $1")
            .bind(*run.as_uuid())
            .execute(&mut *t.tx)
            .await
            .expect("succeed");
        t.start_deployment(&request(f, new, config, 1, uid), NewAudit::default(), None)
            .await
            .expect("start");
        assert_eq!(
            t.rollback_point(f.target, None).await.expect("read"),
            Some((old, config))
        );
        assert_eq!(
            t.rollback_point(f.target, Some(new)).await.expect("read"),
            None,
            "never succeeded"
        );
        t.commit().await.expect("commit");
        (old, new, config, uid)
    }

    /// Lift both freezes (each once) and resume the paused target.
    async fn lift_and_resume(tenant: &mut Tenant, fx: &Fixture, freeze: Uuid, later: Uuid) {
        assert!(
            tenant
                .lift_freeze(fx.environment, freeze, BY)
                .await
                .expect("lift")
        );
        assert!(
            !tenant
                .lift_freeze(fx.environment, freeze, BY)
                .await
                .expect("lift"),
            "once"
        );
        assert_eq!(
            tenant.freezes(fx.environment, false).await.expect("read").len(),
            1,
            "the later one"
        );
        assert!(tenant.lift_freeze(fx.environment, later, BY).await.expect("lift"));
        assert!(tenant.resume_target(fx.target).await.expect("resume"));
        assert!(!tenant.resume_target(fx.target).await.expect("resume"));
    }

    #[tokio::test]
    async fn freezes_hold_releases_and_emergencies_pass_everything() {
        let Some(store) = pg_store().await else {
            return skip("freezes_hold_releases_and_emergencies_pass_everything");
        };
        let fx = fixture(&store).await;
        let (old, new, config, uid) = two_releases(&store, &fx).await;
        let mut tenant = store.tenant(fx.org).await.expect("tenant");
        let now = now_ms();
        let window = |starts_at: i64| NewWindow {
            project: fx.project,
            environment: fx.environment,
            target: None,
            reason: "launch week".into(),
            created_by: BY.into(),
            starts_at,
            ends_at: now + 3_600_000,
        };
        let later = tenant
            .create_freeze(&window(now + 1_800_000))
            .await
            .expect("freeze");
        assert_eq!(
            tenant.active_freeze(fx.target, now).await.expect("read"),
            None,
            "not started yet"
        );
        let freeze = tenant.create_freeze(&window(now - 1000)).await.expect("freeze");
        assert_eq!(
            tenant
                .active_freeze(fx.target, now)
                .await
                .expect("read")
                .as_deref(),
            Some("launch week")
        );
        let frozen = tenant
            .start_deployment(&request(&fx, old, config, 2, uid), NewAudit::default(), None)
            .await
            .expect("start");
        assert_eq!(frozen, Started::Frozen);
        let mut restart = request(&fx, new, config, 2, uid);
        restart.reason = RunReason::Restart;
        let restarted = tenant
            .start_deployment(&restart, NewAudit::default(), None)
            .await
            .expect("start");
        assert!(
            matches!(restarted, Started::Accepted { .. }),
            "restarts pass a freeze"
        );
        let mut unexplained = request(&fx, old, config, 3, uid);
        unexplained.reason = RunReason::Emergency;
        assert!(
            tenant
                .start_deployment(&unexplained, NewAudit::default(), None)
                .await
                .is_err()
        );
        assert!(
            tenant
                .pause_target(fx.target, BY, "incident")
                .await
                .expect("pause")
        );
        assert!(!tenant.pause_target(fx.target, BY, "again").await.expect("pause"));
        assert!(tenant.target_paused(fx.target).await.expect("read"));
        let emergency = tenant
            .start_emergency_rollback(
                &request(&fx, old, config, 3, uid),
                "checkout is down",
                NewAudit::default(),
            )
            .await
            .expect("emergency");
        let Started::Accepted { operation, .. } = emergency else {
            panic!("not accepted: {emergency:?}");
        };
        let m = tenant
            .materialization(operation)
            .await
            .expect("read")
            .expect("run");
        assert!(m.emergency && m.paused);
        assert_eq!(m.phase, RunPhase::Planned, "no approvals asked");
        lift_and_resume(&mut tenant, &fx, freeze, later).await;
        tenant.commit().await.expect("commit");
    }

    #[tokio::test]
    async fn owners_and_silences() {
        let Some(store) = pg_store().await else {
            return skip("owners_and_silences");
        };
        let f = fixture(&store).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(t.owner(f.project, None).await.expect("read"), None);
        t.set_owner(f.project, None, Some(("shop-team", Some("#shop"), None)), BY)
            .await
            .expect("owner");
        t.set_owner(
            f.project,
            Some(f.application),
            Some(("web-team", None, Some("https://wiki/web"))),
            BY,
        )
        .await
        .expect("owner");
        t.set_owner(f.project, None, Some(("platform", None, None)), BY)
            .await
            .expect("replace");
        assert_eq!(
            t.owner(f.project, None)
                .await
                .expect("read")
                .map(|o| o.owner)
                .as_deref(),
            Some("platform")
        );
        let app_owner = t
            .owner(f.project, Some(f.application))
            .await
            .expect("read")
            .expect("owner");
        assert_eq!(app_owner.runbook_url.as_deref(), Some("https://wiki/web"));
        t.set_owner(f.project, None, None, BY).await.expect("clear");
        assert_eq!(t.owner(f.project, None).await.expect("read"), None);

        let now = now_ms();
        assert!(
            !t.silenced(f.environment, Some(f.target), now)
                .await
                .expect("read")
        );
        let silence = t
            .create_silence(&NewWindow {
                project: f.project,
                environment: f.environment,
                target: Some(f.target),
                reason: "maintenance".into(),
                created_by: BY.into(),
                starts_at: now,
                ends_at: now + 60_000,
            })
            .await
            .expect("silence");
        assert!(
            t.silenced(f.environment, Some(f.target), now)
                .await
                .expect("read")
        );
        assert!(
            !t.silenced(f.environment, None, now).await.expect("read"),
            "only that app"
        );
        assert!(
            !t.silenced(f.environment, Some(f.target), now + 120_000)
                .await
                .expect("read"),
            "ended"
        );
        assert_eq!(t.silences(f.environment, false).await.expect("read").len(), 1);
        assert!(t.lift_silence(f.environment, silence, BY).await.expect("lift"));
        assert!(
            !t.silenced(f.environment, Some(f.target), now)
                .await
                .expect("read")
        );
        t.commit().await.expect("commit");
    }
}

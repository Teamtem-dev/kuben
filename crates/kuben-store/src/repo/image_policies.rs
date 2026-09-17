//! Image update policies (M5.4, migration 0033) and the deployment a new
//! digest starts.

use std::collections::BTreeMap;

use kuben_core::{
    artifact::Digest,
    ids::{DeploymentRunId, EnvironmentId, OrgId, ProjectId, TargetId},
    time::now_ms,
};
use serde_json::json;
use uuid::Uuid;

use super::{NewAudit, PortableRelease, RunReason, StartDeployment, Started, Tenant};
use crate::{Store, StoreError};

/// The policies, with a filter.
macro_rules! policies {
    ($tail:literal) => {
        concat!(
            "SELECT p.target_id, p.project_id, t.application_id, pl.environment_id, p.repository, p.pattern, \
             p.enabled, p.interval_secs, p.next_check_at, p.failures, p.last_checked_at, p.last_tag, \
             p.last_digest, p.last_error, p.last_run_id, p.updated_by, p.updated_at \
             FROM image_policies p \
             JOIN application_targets t ON t.id = p.target_id AND t.org_id = p.org_id \
             JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id ",
            $tail
        )
    };
}
const POLICY_OF: &str = policies!("WHERE p.target_id = $1 AND p.org_id = $2");
const DUE: &str = policies!(
    "WHERE p.org_id = $1 AND p.enabled AND p.next_check_at <= $2 AND t.deleted_at IS NULL AND NOT t.deleting \
     ORDER BY p.next_check_at LIMIT $3 FOR UPDATE OF p SKIP LOCKED"
);
const UPSERT: &str = "INSERT INTO image_policies \
     (target_id, org_id, project_id, repository, pattern, enabled, interval_secs, next_check_at, updated_by, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $8) \
     ON CONFLICT (target_id) DO UPDATE SET repository = EXCLUDED.repository, pattern = EXCLUDED.pattern, \
       enabled = EXCLUDED.enabled, interval_secs = EXCLUDED.interval_secs, next_check_at = EXCLUDED.next_check_at, \
       failures = 0, last_error = NULL, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at";
const DELETE: &str = "DELETE FROM image_policies WHERE target_id = $1 AND org_id = $2";
const CHECKED: &str = "UPDATE image_policies SET last_checked_at = $3, next_check_at = $4, failures = $5, \
     last_error = $6, last_tag = coalesce($7, last_tag), last_digest = coalesce($8, last_digest), \
     last_run_id = coalesce($9, last_run_id) WHERE target_id = $1 AND org_id = $2";
const CURRENT_DIGEST: &str = "SELECT rel.artifacts ->> 'web' FROM deployment_runs r \
     JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id \
     WHERE r.target_id = $1 AND r.org_id = $2 \
       AND r.phase NOT IN ('failed', 'cancelled', 'superseded', 'recoveryFailed') \
     ORDER BY r.generation DESC LIMIT 1";
const ENABLED: &str = "SELECT count(*) FROM image_policies WHERE org_id = $1 AND enabled";

/// An app's image update policy.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct ImagePolicy {
    pub target_id: Uuid,
    pub project_id: Uuid,
    pub application_id: Uuid,
    pub environment_id: Uuid,
    pub repository: String,
    pub pattern: String,
    pub enabled: bool,
    pub interval_secs: i32,
    pub next_check_at: i64,
    pub failures: i32,
    pub last_checked_at: Option<i64>,
    pub last_tag: Option<String>,
    pub last_digest: Option<String>,
    pub last_error: Option<String>,
    pub last_run_id: Option<Uuid>,
    pub updated_by: String,
    pub updated_at: i64,
}

impl ImagePolicy {
    #[must_use]
    pub const fn target(&self) -> TargetId {
        TargetId::from_uuid(self.target_id)
    }

    #[must_use]
    pub const fn environment(&self) -> EnvironmentId {
        EnvironmentId::from_uuid(self.environment_id)
    }
}

/// A policy to set.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewImagePolicy<'a> {
    pub project: ProjectId,
    pub target: TargetId,
    pub repository: &'a str,
    pub pattern: &'a str,
    pub enabled: bool,
    pub interval_secs: u32,
    pub by: &'a str,
}

/// What one check found.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Checked<'a> {
    pub next_check_at: i64,
    pub failures: u32,
    pub error: Option<&'a str>,
    pub tag: Option<&'a str>,
    pub digest: Option<&'a str>,
    pub run: Option<DeploymentRunId>,
}

fn small(value: u32) -> Result<i32, sqlx::Error> {
    i32::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Tenant {
    /// Set `p` as its app's policy; it is checked at once.
    pub async fn set_image_policy(&mut self, p: &NewImagePolicy<'_>) -> Result<(), StoreError> {
        sqlx::query(UPSERT)
            .bind(*p.target.as_uuid())
            .bind(self.org.to_string())
            .bind(*p.project.as_uuid())
            .bind(p.repository)
            .bind(p.pattern)
            .bind(p.enabled)
            .bind(small(p.interval_secs)?)
            .bind(now_ms())
            .bind(p.by)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// `target`'s policy.
    pub async fn image_policy(&mut self, target: TargetId) -> Result<Option<ImagePolicy>, StoreError> {
        Ok(sqlx::query_as(POLICY_OF)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Remove `target`'s policy. False when it has none.
    pub async fn delete_image_policy(&mut self, target: TargetId) -> Result<bool, StoreError> {
        let rows = sqlx::query(DELETE)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Enabled policies due at `now`, locked for this transaction.
    pub async fn due_image_policies(&mut self, now: i64, limit: i64) -> Result<Vec<ImagePolicy>, StoreError> {
        Ok(sqlx::query_as(DUE)
            .bind(self.org.to_string())
            .bind(now)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Record a check of `target`'s policy.
    pub async fn image_policy_checked(
        &mut self,
        target: TargetId,
        c: &Checked<'_>,
    ) -> Result<(), StoreError> {
        let error = c.error.map(|e| e.chars().take(1024).collect::<String>());
        sqlx::query(CHECKED)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(c.next_check_at)
            .bind(small(c.failures.min(1_000))?)
            .bind(error)
            .bind(c.tag)
            .bind(c.digest)
            .bind(c.run.map(|r| *r.as_uuid()))
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The digest `target` runs or is about to run.
    pub async fn current_digest(&mut self, target: TargetId) -> Result<Option<String>, StoreError> {
        let found: Option<Option<String>> = sqlx::query_scalar(CURRENT_DIGEST)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(found.flatten())
    }

    /// Deploy `digest` of `policy`'s repository to its app with the app's
    /// newest configuration. The environment's policy decides whether the
    /// run waits for approval.
    pub async fn deploy_followed_image(
        &mut self,
        policy: &ImagePolicy,
        digest: &Digest,
        given: &str,
    ) -> Result<Started, StoreError> {
        let target = policy.target();
        let project = ProjectId::from_uuid(policy.project_id);
        let (Some(config), Some(state)) = (
            self.latest_config_revision(target).await?,
            self.target_state(target).await?,
        ) else {
            return Ok(Started::NotFound);
        };
        let by = format!("image-policy:{target}");
        let release = PortableRelease {
            application: kuben_core::ids::ApplicationId::from_uuid(policy.application_id),
            artifacts: BTreeMap::from([("web".to_owned(), digest.clone())]),
            process_contract: json!({}),
            portable_config: json!({}),
            renderer_schema: 1,
            source: Some(
                json!({ "image_repository": policy.repository, "image": given, "policy": policy.pattern }),
            ),
            created_by: by.clone(),
        };
        let (release, _) = self.create_release(project, &release).await?;
        let input = format!("{release}/{config}/{}", state.desired_generation.0);
        let request = StartDeployment {
            project,
            target,
            release,
            config_revision: config,
            render_plan: None,
            expected_generation: state.desired_generation,
            lifecycle_uid: state.lifecycle_uid,
            reason: RunReason::Deploy,
            requested_by: by.clone(),
            input_hash: input.into_bytes(),
        };
        let audit = NewAudit {
            actor_kind: "system".into(),
            actor_id: Some(by),
            action: "deployment.accepted".into(),
            target_kind: Some("app".into()),
            target_ref: Some(target.to_string()),
            outcome: "accepted".into(),
            data: Some(json!({ "image": given, "digest": digest, "pattern": policy.pattern })),
            ..NewAudit::default()
        };
        self.start_deployment(&request, audit, None).await
    }
}

impl Store {
    /// Organizations with an enabled image policy, for the watcher.
    pub async fn image_policy_orgs(&self) -> Result<Vec<OrgId>, StoreError> {
        let mut out = Vec::new();
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            let n: i64 = sqlx::query_scalar(ENABLED)
                .bind(org.to_string())
                .fetch_one(&mut *tenant.tx)
                .await?;
            if n > 0 {
                out.push(org);
            }
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{pg_store, skip};

    #[tokio::test]
    async fn policies_come_due_and_record_checks() {
        let Some(store) = pg_store().await else {
            return skip("policies_come_due_and_record_checks");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "dev", "Dev", false)
            .await
            .expect("env");
        let cluster = t.create_cluster("primary").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, "kb-shop-dev")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let new = NewImagePolicy {
            project,
            target,
            repository: "docker.io/library/nginx",
            pattern: "semver:^1",
            enabled: true,
            interval_secs: 300,
            by: "u",
        };
        t.set_image_policy(&new).await.expect("set");
        t.commit().await.expect("commit");
        assert_eq!(store.image_policy_orgs().await.expect("orgs"), [org]);

        let mut t = store.tenant(org).await.expect("tenant");
        let due = t.due_image_policies(now_ms() + 1, 10).await.expect("due");
        assert_eq!(due.len(), 1);
        assert_eq!(due[0].environment(), env);
        let digest = format!("sha256:{}", "a".repeat(64));
        let checked = Checked {
            next_check_at: now_ms() + 60_000,
            tag: Some("1.2.3"),
            digest: Some(&digest),
            ..Checked::default()
        };
        t.image_policy_checked(target, &checked).await.expect("record");
        assert!(
            t.due_image_policies(now_ms(), 10).await.expect("due").is_empty(),
            "not due again yet"
        );
        let failed = Checked {
            next_check_at: now_ms(),
            failures: 2,
            error: Some("unreachable"),
            ..Checked::default()
        };
        t.image_policy_checked(target, &failed).await.expect("record");
        let policy = t.image_policy(target).await.expect("read").expect("policy");
        assert_eq!(
            (
                policy.last_tag.as_deref(),
                policy.failures,
                policy.last_error.as_deref()
            ),
            (Some("1.2.3"), 2, Some("unreachable")),
            "a failure keeps what was found before"
        );
        assert_eq!(
            t.current_digest(target).await.expect("digest"),
            None,
            "never deployed"
        );
        assert_eq!(
            t.deploy_followed_image(&policy, &digest.parse().expect("digest"), "nginx:1.2.3")
                .await
                .expect("deploy"),
            Started::NotFound,
            "an app without configuration is not deployed"
        );
        assert!(t.delete_image_policy(target).await.expect("delete"));
        assert!(!t.delete_image_policy(target).await.expect("delete"));
    }
}

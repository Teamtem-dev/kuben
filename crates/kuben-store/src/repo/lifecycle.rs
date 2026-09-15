//! Project and environment lifecycles, and deletion (ADR-032, M1.8b step 2).
//!
//! The API writes intent in SQL and asks the materializer, through durable
//! operations, to make the cluster follow: write a project's or an
//! environment's resource right away (an environment's namespace holds
//! secrets before any app is deployed), and remove the resources of what is
//! being deleted. A deletion is soft: the request's transaction marks the row
//! `deleting`, and the row gets `deleted_at` once its resources are gone.

use kuben_core::ids::{EnvironmentId, OperationId, ProjectId, TargetId};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use uuid::Uuid;

use super::{Accepted, Claim, NewAudit, NewOperation, Tenant};
use crate::{Store, StoreError};

/// Write a project's resource.
pub const PROJECT_APPLY: &str = "project.apply";
/// Write an environment's resource (and its project's).
pub const ENVIRONMENT_APPLY: &str = "environment.apply";
/// Remove a project's resource, then mark the project deleted.
pub const PROJECT_DELETE: &str = "project.delete";
/// Remove an environment's resource (its controller removes the namespace),
/// then mark the environment, its placement and its apps deleted.
pub const ENVIRONMENT_DELETE: &str = "environment.delete";
/// Remove an app's resource (and, when asked, its volumes), then mark the
/// target deleted.
pub const TARGET_DELETE: &str = "target.delete";
/// Every lifecycle operation kind the materializer claims.
pub const LIFECYCLE_KINDS: [&str; 5] = [
    PROJECT_APPLY,
    ENVIRONMENT_APPLY,
    PROJECT_DELETE,
    ENVIRONMENT_DELETE,
    TARGET_DELETE,
];
const TOPIC: &str = "lifecycle.requested";

const PAYLOAD: &str = "SELECT payload::text FROM operations WHERE id = $1 AND org_id = $2";
const MARK_PROJECT: &str = "UPDATE projects SET deleting = TRUE \
     WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL";
const MARK_ENVIRONMENT: &str = "UPDATE environments SET deleting = TRUE \
     WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL";
const MARK_TARGET: &str = "UPDATE application_targets SET deleting = TRUE \
     WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL";
const LIVE_ENVIRONMENTS: &str = "SELECT count(*) FROM environments \
     WHERE project_id = $1 AND org_id = $2 AND deleted_at IS NULL";
const FINISH_TARGET: &str = "UPDATE application_targets SET deleted_at = kuben_now_ms() \
     WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL";
/// Applications without a live target are deleted with their last target.
const FINISH_APPLICATIONS: &str = "UPDATE applications a SET deleted_at = kuben_now_ms() \
     WHERE a.org_id = $1 AND a.project_id = $2 AND a.deleted_at IS NULL \
       AND NOT EXISTS (SELECT 1 FROM application_targets t \
                       WHERE t.application_id = a.id AND t.deleted_at IS NULL)";
const FINISH_ENVIRONMENT_TARGETS: &str = "UPDATE application_targets t \
     SET deleting = TRUE, deleted_at = kuben_now_ms() \
     FROM environment_placements p \
     WHERE p.id = t.placement_id AND p.environment_id = $1 AND t.org_id = $2 AND t.deleted_at IS NULL";
const RETIRE_PLACEMENTS: &str = "UPDATE environment_placements SET state = 'retired' \
     WHERE environment_id = $1 AND org_id = $2 AND state <> 'retired'";
const FINISH_ENVIRONMENT: &str = "UPDATE environments SET deleted_at = kuben_now_ms() \
     WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL \
     RETURNING project_id";
const FINISH_PROJECT: &str = "UPDATE projects SET deleted_at = kuben_now_ms() \
     WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL";

/// What a lifecycle operation acts on: its payload.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Subject {
    pub project: ProjectId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub environment: Option<EnvironmentId>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<TargetId>,
    /// `target.delete`: also delete the app's retained volumes.
    #[serde(default)]
    pub delete_volumes: bool,
}

impl Subject {
    #[must_use]
    pub const fn project(project: ProjectId) -> Self {
        Self {
            project,
            environment: None,
            target: None,
            delete_volumes: false,
        }
    }

    #[must_use]
    pub const fn environment(project: ProjectId, environment: EnvironmentId) -> Self {
        Self {
            environment: Some(environment),
            ..Self::project(project)
        }
    }

    /// An app: its target, and the environment whose placement it is on.
    #[must_use]
    pub const fn target(
        project: ProjectId,
        environment: EnvironmentId,
        target: TargetId,
        delete_volumes: bool,
    ) -> Self {
        Self {
            environment: Some(environment),
            target: Some(target),
            delete_volumes,
            ..Self::project(project)
        }
    }
}

impl Tenant {
    /// Ask the materializer to carry out `kind` on `subject`. The operation,
    /// `audit` and the outbox message commit with this transaction.
    pub async fn request(
        &mut self,
        kind: &str,
        subject: Subject,
        requested_by: &str,
        audit: NewAudit,
    ) -> Result<OperationId, StoreError> {
        let payload = serde_json::to_value(subject).map_err(|e| sqlx::Error::Encode(e.into()))?;
        let op = NewOperation {
            kind: kind.to_owned(),
            target: subject.target.map(|t| (subject.project, t)),
            lifecycle_uid: None,
            generation: None,
            input_hash: payload.to_string().into_bytes(),
            payload,
            requested_by: requested_by.to_owned(),
            deadline_at: None,
            topic: TOPIC.to_owned(),
        };
        // Without an idempotency key every request is a new operation.
        match self.accept(&op, audit, None).await? {
            Accepted::New(id) | Accepted::Replayed(id) | Accepted::KeyReused(id) => Ok(id),
        }
    }

    async fn mark(&mut self, sql: &'static str, id: Uuid) -> Result<bool, StoreError> {
        let rows = sqlx::query(sql)
            .bind(id)
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Mark `project` deleting. False when it already is, or is not live.
    pub async fn mark_project_deleting(&mut self, project: ProjectId) -> Result<bool, StoreError> {
        self.mark(MARK_PROJECT, *project.as_uuid()).await
    }

    /// Mark `environment` deleting. False when it already is, or is not live.
    pub async fn mark_environment_deleting(
        &mut self,
        environment: EnvironmentId,
    ) -> Result<bool, StoreError> {
        self.mark(MARK_ENVIRONMENT, *environment.as_uuid()).await
    }

    /// Mark `target` deleting. False when it already is, or is not live.
    pub async fn mark_target_deleting(&mut self, target: TargetId) -> Result<bool, StoreError> {
        self.mark(MARK_TARGET, *target.as_uuid()).await
    }

    /// The live environments of `project`, deleting or not.
    pub async fn live_environments(&mut self, project: ProjectId) -> Result<u64, StoreError> {
        let n: i64 = sqlx::query_scalar(LIVE_ENVIRONMENTS)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(u64::try_from(n).unwrap_or_default())
    }

    /// `target`'s resources are gone: mark it deleted, and its application
    /// too when that was its last target.
    pub async fn finish_target_deletion(
        &mut self,
        project: ProjectId,
        target: TargetId,
    ) -> Result<bool, StoreError> {
        let done = self.mark(FINISH_TARGET, *target.as_uuid()).await?;
        self.finish_applications(project).await?;
        Ok(done)
    }

    /// `environment`'s resource and namespace are gone: mark it, its
    /// placements, its targets and the applications left without one deleted.
    pub async fn finish_environment_deletion(
        &mut self,
        environment: EnvironmentId,
    ) -> Result<bool, StoreError> {
        let org = self.org.to_string();
        for sql in [FINISH_ENVIRONMENT_TARGETS, RETIRE_PLACEMENTS] {
            sqlx::query(sql)
                .bind(*environment.as_uuid())
                .bind(&org)
                .execute(&mut *self.tx)
                .await?;
        }
        let project: Option<Uuid> = sqlx::query_scalar(FINISH_ENVIRONMENT)
            .bind(*environment.as_uuid())
            .bind(&org)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(project) = project else {
            return Ok(false);
        };
        self.finish_applications(ProjectId::from_uuid(project)).await?;
        Ok(true)
    }

    /// `project`'s resource is gone: mark it deleted.
    pub async fn finish_project_deletion(&mut self, project: ProjectId) -> Result<bool, StoreError> {
        self.mark(FINISH_PROJECT, *project.as_uuid()).await
    }

    async fn finish_applications(&mut self, project: ProjectId) -> Result<(), StoreError> {
        sqlx::query(FINISH_APPLICATIONS)
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }
}

impl Store {
    /// The subject of the lifecycle operation `claim` holds, or `None` when
    /// its payload is not one.
    pub async fn lifecycle_subject(&self, claim: &Claim) -> Result<Option<Subject>, StoreError> {
        let payload: Option<String> = sqlx::query_scalar(PAYLOAD)
            .bind(*claim.id.as_uuid())
            .bind(claim.org.to_string())
            .fetch_optional(self.pool())
            .await?;
        Ok(payload
            .and_then(|p| serde_json::from_str::<Value>(&p).ok())
            .and_then(|v| serde_json::from_value(v).ok()))
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;
    use crate::{
        repo::{EnvironmentKind, NewAudit},
        testing::{pg_store, skip},
    };

    fn audit(action: &str) -> NewAudit {
        NewAudit {
            actor_kind: "user".into(),
            actor_id: Some("alice".into()),
            action: action.into(),
            outcome: "accepted".into(),
            ..NewAudit::default()
        }
    }

    #[tokio::test]
    async fn requests_are_operations_the_materializer_claims() {
        let Some(store) = pg_store().await else {
            skip("lifecycle requests");
            return;
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment_typed(project, "dev", "Dev", EnvironmentKind::Standard, None)
            .await
            .expect("environment");
        let requested = t
            .request(
                ENVIRONMENT_APPLY,
                Subject::environment(project, environment),
                "user:alice",
                audit("createEnvironment"),
            )
            .await
            .expect("request");
        t.commit().await.expect("commit");

        let claim = store
            .claim_operation("m", &LIFECYCLE_KINDS, Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("due");
        assert_eq!((claim.id, claim.kind.as_str()), (requested, ENVIRONMENT_APPLY));
        assert_eq!(
            store.lifecycle_subject(&claim).await.expect("read"),
            Some(Subject::environment(project, environment))
        );
    }

    #[tokio::test]
    async fn deletion_is_marked_then_finished() {
        let Some(store) = pg_store().await else {
            skip("lifecycle deletion");
            return;
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment_typed(project, "dev", "Dev", EnvironmentKind::Standard, None)
            .await
            .expect("environment");
        let cluster = t.ensure_cluster("primary").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, "kb-shop-dev")
            .await
            .expect("placement");
        let web = t.create_application(project, "web", "Web").await.expect("app");
        let target = t.create_target(project, web, placement).await.expect("target");
        let api = t.create_application(project, "api", "API").await.expect("app");
        t.create_target(project, api, placement).await.expect("target");

        assert!(t.mark_target_deleting(target).await.expect("mark"));
        assert!(!t.mark_target_deleting(target).await.expect("again"), "once");
        assert!(t.finish_target_deletion(project, target).await.expect("finish"));
        assert!(t.app(environment, "web").await.expect("read").is_none());
        assert!(t.app(environment, "api").await.expect("read").is_some());

        assert!(t.mark_environment_deleting(environment).await.expect("mark"));
        assert_eq!(
            t.live_environments(project).await.expect("count"),
            1,
            "deleting is still live"
        );
        assert!(t.finish_environment_deletion(environment).await.expect("finish"));
        assert_eq!(t.live_environments(project).await.expect("count"), 0);
        assert!(t.environment(project, "dev").await.expect("read").is_none());

        assert!(t.mark_project_deleting(project).await.expect("mark"));
        assert!(t.finish_project_deletion(project).await.expect("finish"));
        assert!(t.project("shop").await.expect("read").is_none());

        // Everything's slug is free again.
        let again = t.create_project("shop", "Shop").await.expect("project again");
        let env = t
            .create_environment_typed(again, "dev", "Dev", EnvironmentKind::Standard, None)
            .await
            .expect("environment again");
        t.create_placement(again, env, cluster, "kb-shop-dev")
            .await
            .expect("the namespace is free again");
        t.commit().await.expect("commit");
    }
}

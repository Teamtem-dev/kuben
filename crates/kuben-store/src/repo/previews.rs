//! Preview environments (M5.1, migration 0030): the per-project policy, the
//! previews themselves and what a preview copies from its source
//! environment.

use kuben_core::{
    ids::{ApplicationId, EnvironmentId, OrgId, ProjectId, TargetId},
    source::{BuildRecipe, CommitSha, RepoName},
    time::now_ms,
};
use serde_json::Value;
use uuid::Uuid;

use super::Tenant;
use crate::{Store, StoreError};

const POLICY: &str = "SELECT project_id, enabled, source_environment_id, ttl_hours, max_active, allow_forks, \
     updated_by, updated_at FROM preview_policies WHERE project_id = $1 AND org_id = $2";
const UPSERT_POLICY: &str = "INSERT INTO preview_policies \
     (project_id, org_id, enabled, source_environment_id, ttl_hours, max_active, allow_forks, updated_by, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) \
     ON CONFLICT (project_id) DO UPDATE SET enabled = EXCLUDED.enabled, \
       source_environment_id = EXCLUDED.source_environment_id, ttl_hours = EXCLUDED.ttl_hours, \
       max_active = EXCLUDED.max_active, allow_forks = EXCLUDED.allow_forks, \
       updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at";
/// The previews, with filters.
macro_rules! previews {
    ($tail:literal) => {
        concat!(
            "SELECT p.environment_id, p.project_id, e.slug AS environment, p.provider, p.installation_id, \
             p.repository, p.pr_number, p.preview_epoch, p.head_repository, p.branch, p.commit_sha, \
             p.trusted, p.state, p.auto_delete, p.expires_at, p.last_event_at, p.last_verified_at, \
             p.created_by, p.created_at, p.updated_at, p.closed_at, p.close_reason \
             FROM previews p JOIN environments e ON e.id = p.environment_id AND e.org_id = p.org_id ",
            $tail
        )
    };
}
const PREVIEWS: &str = previews!(
    "WHERE p.project_id = $1 AND p.org_id = $2 AND ($3 OR p.state = 'active') ORDER BY p.created_at DESC"
);
const PREVIEW_OF: &str = previews!("WHERE p.environment_id = $1 AND p.org_id = $2");
const ACTIVE_FOR: &str = previews!(
    "WHERE p.project_id = $1 AND p.org_id = $2 AND p.provider = $3 AND p.repository = $4 \
     AND p.pr_number = $5 AND p.state = 'active' FOR UPDATE OF p"
);
const DUE: &str = previews!(
    "WHERE p.org_id = $1 AND p.state = 'active' AND p.auto_delete AND p.expires_at <= $2 \
     ORDER BY p.expires_at LIMIT $3 FOR UPDATE OF p SKIP LOCKED"
);
const UNVERIFIED: &str = previews!(
    "WHERE p.org_id = $1 AND p.state = 'active' AND p.last_verified_at < $2 \
     ORDER BY p.last_verified_at LIMIT $3"
);
const NEXT_EPOCH: &str = "SELECT coalesce(max(preview_epoch), 0) + 1 FROM previews \
     WHERE org_id = $1 AND project_id = $2 AND provider = $3 AND repository = $4 AND pr_number = $5";
const LAST_CLOSED: &str = "SELECT max(last_event_at) FROM previews \
     WHERE org_id = $1 AND project_id = $2 AND provider = $3 AND repository = $4 AND pr_number = $5 \
       AND state = 'closed'";
const ACTIVE_COUNT: &str =
    "SELECT count(*) FROM previews WHERE org_id = $1 AND project_id = $2 AND state = 'active'";
const INSERT_PREVIEW: &str = "INSERT INTO previews \
     (environment_id, org_id, project_id, provider, installation_id, repository, pr_number, preview_epoch, \
      head_repository, branch, commit_sha, trusted, expires_at, last_event_at, last_verified_at, \
      created_by, created_at, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $15, $15)";
const TOUCH: &str = "UPDATE previews SET head_repository = $3, branch = $4, commit_sha = $5, \
     last_event_at = $6, expires_at = GREATEST(expires_at, $7), last_verified_at = $8, updated_at = $8 \
     WHERE environment_id = $1 AND org_id = $2 AND state = 'active' AND last_event_at <= $6";
const CLOSE: &str = "UPDATE previews SET state = 'closed', closed_at = $3, close_reason = $4, updated_at = $3, \
     last_event_at = GREATEST(last_event_at, $5) \
     WHERE environment_id = $1 AND org_id = $2 AND state = 'active'";
const EXTEND: &str = "UPDATE previews SET expires_at = $3, auto_delete = $4, updated_at = $5 \
     WHERE environment_id = $1 AND org_id = $2 AND state = 'active'";
const VERIFIED: &str = "UPDATE previews SET last_verified_at = $3 \
     WHERE environment_id = $1 AND org_id = $2 AND state = 'active'";
const UNTRUSTED: &str = "SELECT EXISTS (SELECT 1 FROM application_targets t \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     JOIN previews p ON p.environment_id = pl.environment_id AND p.org_id = t.org_id \
     WHERE t.id = $1 AND t.org_id = $2 AND NOT p.trusted)";
const UNTRUSTED_ENVIRONMENT: &str =
    "SELECT EXISTS (SELECT 1 FROM previews WHERE environment_id = $1 AND org_id = $2 AND NOT trusted)";
const SOURCES: &str = "SELECT t.id AS target_id, a.id AS application_id, a.slug, c.config::text AS config, \
     b.recipe::text AS recipe, b.image_repository \
     FROM source_bindings b \
     JOIN application_targets t ON t.id = b.target_id AND t.org_id = b.org_id \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     JOIN LATERAL (SELECT config FROM target_config_revisions r \
                   WHERE r.target_id = t.id AND r.org_id = t.org_id ORDER BY r.revision DESC LIMIT 1) c ON TRUE \
     WHERE b.org_id = $1 AND pl.environment_id = $2 AND b.provider = 'github' AND b.installation_id = $3 \
       AND b.repository = $4 AND b.pull_request IS NULL AND t.deleted_at IS NULL AND NOT t.deleting \
     ORDER BY a.slug";
const ENABLED_PROJECTS: &str = "SELECT pp.project_id FROM preview_policies pp \
     JOIN projects pr ON pr.id = pp.project_id AND pr.org_id = pp.org_id \
     WHERE pp.org_id = $1 AND pp.enabled AND pr.deleted_at IS NULL AND NOT pr.deleting \
       AND EXISTS (SELECT 1 FROM source_bindings b \
                   JOIN application_targets t ON t.id = b.target_id AND t.org_id = b.org_id \
                   JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
                   WHERE b.org_id = pp.org_id AND pl.environment_id = pp.source_environment_id \
                     AND b.installation_id = $2 AND b.repository = $3 AND b.pull_request IS NULL) \
     ORDER BY pp.project_id";
const PREVIEW_TARGETS: &str = "SELECT t.id FROM application_targets t \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     WHERE pl.environment_id = $1 AND t.org_id = $2 AND t.deleted_at IS NULL AND NOT t.deleting";

/// A project's preview settings.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PreviewPolicy {
    pub project: ProjectId,
    pub enabled: bool,
    pub source_environment: EnvironmentId,
    pub ttl_hours: u32,
    pub max_active: u32,
    pub allow_forks: bool,
    pub updated_by: String,
    pub updated_at: i64,
}

#[derive(sqlx::FromRow)]
struct PolicyRow {
    project_id: Uuid,
    enabled: bool,
    source_environment_id: Uuid,
    ttl_hours: i32,
    max_active: i32,
    allow_forks: bool,
    updated_by: String,
    updated_at: i64,
}

/// A preview environment.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct Preview {
    pub environment_id: Uuid,
    pub project_id: Uuid,
    /// The environment's name (`pr<n>-<epoch>`).
    pub environment: String,
    pub provider: String,
    pub installation_id: i64,
    pub repository: String,
    pub pr_number: i64,
    pub preview_epoch: i64,
    pub head_repository: String,
    pub branch: String,
    pub commit_sha: String,
    pub trusted: bool,
    /// `active` or `closed`.
    pub state: String,
    pub auto_delete: bool,
    pub expires_at: i64,
    pub last_event_at: i64,
    pub last_verified_at: i64,
    pub created_by: String,
    pub created_at: i64,
    pub updated_at: i64,
    pub closed_at: Option<i64>,
    pub close_reason: Option<String>,
}

impl Preview {
    #[must_use]
    pub const fn environment_id(&self) -> EnvironmentId {
        EnvironmentId::from_uuid(self.environment_id)
    }

    #[must_use]
    pub const fn project_id(&self) -> ProjectId {
        ProjectId::from_uuid(self.project_id)
    }

    #[must_use]
    pub fn active(&self) -> bool {
        self.state == "active"
    }
}

/// A new preview.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewPreview<'a> {
    pub environment: EnvironmentId,
    pub project: ProjectId,
    pub installation_id: u64,
    pub repository: &'a RepoName,
    pub number: u64,
    pub epoch: u64,
    pub head_repository: &'a str,
    pub branch: &'a str,
    pub commit: &'a CommitSha,
    pub trusted: bool,
    pub expires_at: i64,
    pub event_at: i64,
    pub created_by: &'a str,
}

/// Why a preview closed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CloseReason {
    /// The pull request was closed or merged.
    Closed,
    /// Its lifetime ran out.
    Expired,
    /// Someone destroyed it.
    Manual,
    /// Its environment was deleted.
    Deleted,
}

impl CloseReason {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Closed => "closed",
            Self::Expired => "expired",
            Self::Manual => "manual",
            Self::Deleted => "deleted",
        }
    }
}

/// An app of a source environment a preview copies.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PreviewSource {
    pub target: TargetId,
    pub application: ApplicationId,
    pub slug: String,
    /// The newest configuration of the source app.
    pub config: Value,
    pub recipe: BuildRecipe,
    pub image_repository: String,
}

#[derive(sqlx::FromRow)]
struct SourceRow {
    target_id: Uuid,
    application_id: Uuid,
    slug: String,
    config: String,
    recipe: String,
    image_repository: String,
}

fn decode(e: impl std::error::Error + Send + Sync + 'static) -> sqlx::Error {
    sqlx::Error::Decode(Box::new(e))
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

fn small(value: u32) -> Result<i32, sqlx::Error> {
    i32::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Tenant {
    /// `project`'s preview settings, if set.
    pub async fn preview_policy(&mut self, project: ProjectId) -> Result<Option<PreviewPolicy>, StoreError> {
        let row: Option<PolicyRow> = sqlx::query_as(POLICY)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|r| {
            Ok(PreviewPolicy {
                project: ProjectId::from_uuid(r.project_id),
                enabled: r.enabled,
                source_environment: EnvironmentId::from_uuid(r.source_environment_id),
                ttl_hours: u32::try_from(r.ttl_hours).map_err(decode)?,
                max_active: u32::try_from(r.max_active).map_err(decode)?,
                allow_forks: r.allow_forks,
                updated_by: r.updated_by,
                updated_at: r.updated_at,
            })
        })
        .transpose()
    }

    /// Set `policy` as its project's preview settings.
    pub async fn set_preview_policy(&mut self, policy: &PreviewPolicy) -> Result<(), StoreError> {
        sqlx::query(UPSERT_POLICY)
            .bind(*policy.project.as_uuid())
            .bind(self.org.to_string())
            .bind(policy.enabled)
            .bind(*policy.source_environment.as_uuid())
            .bind(small(policy.ttl_hours)?)
            .bind(small(policy.max_active)?)
            .bind(policy.allow_forks)
            .bind(&policy.updated_by)
            .bind(policy.updated_at)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// `project`'s previews, newest first; closed ones only with `all`.
    pub async fn previews(&mut self, project: ProjectId, all: bool) -> Result<Vec<Preview>, StoreError> {
        Ok(sqlx::query_as(PREVIEWS)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .bind(all)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// The preview `environment` is, if it is one.
    pub async fn preview_of(&mut self, environment: EnvironmentId) -> Result<Option<Preview>, StoreError> {
        Ok(sqlx::query_as(PREVIEW_OF)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// The active preview of pull request `number`, locked.
    pub async fn active_preview(
        &mut self,
        project: ProjectId,
        repository: &RepoName,
        number: u64,
    ) -> Result<Option<Preview>, StoreError> {
        Ok(sqlx::query_as(ACTIVE_FOR)
            .bind(*project.as_uuid())
            .bind(self.org.to_string())
            .bind("github")
            .bind(repository.as_str())
            .bind(signed(number)?)
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// The epoch a new preview of pull request `number` gets, and the
    /// newest event a closed one of it saw (an event not newer than that
    /// must not bring the preview back).
    pub async fn preview_epoch(
        &mut self,
        project: ProjectId,
        repository: &RepoName,
        number: u64,
    ) -> Result<(u64, Option<i64>), StoreError> {
        let org = self.org.to_string();
        let epoch: i64 = sqlx::query_scalar(NEXT_EPOCH)
            .bind(&org)
            .bind(*project.as_uuid())
            .bind("github")
            .bind(repository.as_str())
            .bind(signed(number)?)
            .fetch_one(&mut *self.tx)
            .await?;
        let closed: Option<i64> = sqlx::query_scalar(LAST_CLOSED)
            .bind(&org)
            .bind(*project.as_uuid())
            .bind("github")
            .bind(repository.as_str())
            .bind(signed(number)?)
            .fetch_one(&mut *self.tx)
            .await?;
        Ok((u64::try_from(epoch).map_err(decode)?, closed))
    }

    /// Active previews of `project`.
    pub async fn active_preview_count(&mut self, project: ProjectId) -> Result<u64, StoreError> {
        let n: i64 = sqlx::query_scalar(ACTIVE_COUNT)
            .bind(self.org.to_string())
            .bind(*project.as_uuid())
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(u64::try_from(n).map_err(decode)?)
    }

    /// Record a new preview (its environment exists in this transaction).
    pub async fn insert_preview(&mut self, p: &NewPreview<'_>) -> Result<(), StoreError> {
        sqlx::query(INSERT_PREVIEW)
            .bind(*p.environment.as_uuid())
            .bind(self.org.to_string())
            .bind(*p.project.as_uuid())
            .bind("github")
            .bind(signed(p.installation_id)?)
            .bind(p.repository.as_str())
            .bind(signed(p.number)?)
            .bind(signed(p.epoch)?)
            .bind(p.head_repository)
            .bind(p.branch)
            .bind(p.commit.as_str())
            .bind(p.trusted)
            .bind(p.expires_at)
            .bind(p.event_at)
            .bind(now_ms())
            .bind(p.created_by)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// Record a newer head of an active preview and keep it alive until at
    /// least `expires_at`. False for an event older than the newest seen.
    pub async fn touch_preview(
        &mut self,
        environment: EnvironmentId,
        head_repository: &str,
        branch: &str,
        commit: &CommitSha,
        event_at: i64,
        expires_at: i64,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(TOUCH)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(head_repository)
            .bind(branch)
            .bind(commit.as_str())
            .bind(event_at)
            .bind(expires_at)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Close an active preview for `reason`. False when it is not active.
    pub async fn close_preview(
        &mut self,
        environment: EnvironmentId,
        reason: CloseReason,
        event_at: i64,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(CLOSE)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .bind(reason.as_str())
            .bind(event_at)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Set an active preview's expiry and whether it is deleted then.
    pub async fn extend_preview(
        &mut self,
        environment: EnvironmentId,
        expires_at: i64,
        auto_delete: bool,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(EXTEND)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(expires_at)
            .bind(auto_delete)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The provider confirmed the preview's pull request is open at `at`.
    pub async fn preview_verified(&mut self, environment: EnvironmentId, at: i64) -> Result<(), StoreError> {
        sqlx::query(VERIFIED)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .bind(at)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// Active previews that expired by `now`, locked for this transaction.
    pub async fn due_previews(&mut self, now: i64, limit: i64) -> Result<Vec<Preview>, StoreError> {
        Ok(sqlx::query_as(DUE)
            .bind(self.org.to_string())
            .bind(now)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Active previews whose pull request was not confirmed open since
    /// `before`.
    pub async fn unverified_previews(&mut self, before: i64, limit: i64) -> Result<Vec<Preview>, StoreError> {
        Ok(sqlx::query_as(UNVERIFIED)
            .bind(self.org.to_string())
            .bind(before)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Whether `target` is an app of an untrusted preview.
    pub async fn untrusted_target(&mut self, target: TargetId) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(UNTRUSTED)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// Whether `environment` is an untrusted preview.
    pub async fn untrusted_environment(&mut self, environment: EnvironmentId) -> Result<bool, StoreError> {
        Ok(sqlx::query_scalar(UNTRUSTED_ENVIRONMENT)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// The apps of `environment` that build from `repository` through
    /// `installation_id`: what a preview of it copies.
    pub async fn preview_sources(
        &mut self,
        environment: EnvironmentId,
        installation_id: u64,
        repository: &RepoName,
    ) -> Result<Vec<PreviewSource>, StoreError> {
        let rows: Vec<SourceRow> = sqlx::query_as(SOURCES)
            .bind(self.org.to_string())
            .bind(*environment.as_uuid())
            .bind(signed(installation_id)?)
            .bind(repository.as_str())
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|r| {
                Ok(PreviewSource {
                    target: TargetId::from_uuid(r.target_id),
                    application: ApplicationId::from_uuid(r.application_id),
                    slug: r.slug,
                    config: serde_json::from_str(&r.config).map_err(decode)?,
                    recipe: serde_json::from_str(&r.recipe).map_err(decode)?,
                    image_repository: r.image_repository,
                })
            })
            .collect()
    }

    /// Projects with previews on whose source environment builds from
    /// `repository` through `installation_id`.
    pub async fn preview_projects(
        &mut self,
        installation_id: u64,
        repository: &RepoName,
    ) -> Result<Vec<ProjectId>, StoreError> {
        let ids: Vec<Uuid> = sqlx::query_scalar(ENABLED_PROJECTS)
            .bind(self.org.to_string())
            .bind(signed(installation_id)?)
            .bind(repository.as_str())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(ids.into_iter().map(ProjectId::from_uuid).collect())
    }

    /// The live apps of a preview environment.
    pub async fn preview_targets(&mut self, environment: EnvironmentId) -> Result<Vec<TargetId>, StoreError> {
        let ids: Vec<Uuid> = sqlx::query_scalar(PREVIEW_TARGETS)
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(ids.into_iter().map(TargetId::from_uuid).collect())
    }
}

impl Store {
    /// Organizations with an active preview, for the janitor.
    pub async fn preview_orgs(&self) -> Result<Vec<OrgId>, StoreError> {
        // `previews` is tenant-scoped: ask every organization.
        let mut out = Vec::new();
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            let n: i64 =
                sqlx::query_scalar("SELECT count(*) FROM previews WHERE org_id = $1 AND state = 'active'")
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
    use kuben_core::source::CommitSha;

    use super::*;
    use crate::{
        repo::EnvironmentKind,
        testing::{pg_store, skip},
    };

    const HEAD: &str = "0123456789abcdef0123456789abcdef01234567";
    const HOUR: i64 = 3_600_000;

    #[tokio::test]
    async fn previews_have_epochs_and_expire() {
        let Some(store) = pg_store().await else {
            return skip("previews_have_epochs_and_expire");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let repo: RepoName = "acme/shop".parse().expect("repo");
        let head: CommitSha = HEAD.parse().expect("sha");
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let source = t
            .create_environment(project, "staging", "Staging", false)
            .await
            .expect("env");
        t.set_preview_policy(&PreviewPolicy {
            project,
            enabled: true,
            source_environment: source,
            ttl_hours: 2,
            max_active: 3,
            allow_forks: false,
            updated_by: "u".into(),
            updated_at: 1,
        })
        .await
        .expect("policy");
        assert_eq!(
            t.preview_policy(project)
                .await
                .expect("read")
                .map(|p| p.ttl_hours),
            Some(2)
        );
        assert_eq!(
            t.preview_epoch(project, &repo, 12).await.expect("epoch"),
            (1, None)
        );
        let env = t
            .create_environment_typed(project, "pr12-1", "PR #12", EnvironmentKind::Preview, None)
            .await
            .expect("env");
        let new = NewPreview {
            environment: env,
            project,
            installation_id: 7,
            repository: &repo,
            number: 12,
            epoch: 1,
            head_repository: "acme/shop",
            branch: "feature",
            commit: &head,
            trusted: false,
            expires_at: 2 * HOUR,
            event_at: 100,
            created_by: "github:7",
        };
        t.insert_preview(&new).await.expect("insert");
        let twice = t
            .insert_preview(&NewPreview {
                epoch: 2,
                ..new.clone()
            })
            .await;
        assert!(
            twice.is_err_and(|e| e.is_unique_violation()),
            "one active preview per pull request"
        );
    }

    #[tokio::test]
    async fn preview_lifecycles_ignore_stale_events() {
        let Some(store) = pg_store().await else {
            return skip("preview_lifecycles_ignore_stale_events");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let repo: RepoName = "acme/shop".parse().expect("repo");
        let head: CommitSha = HEAD.parse().expect("sha");
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment_typed(project, "pr12-1", "PR #12", EnvironmentKind::Preview, None)
            .await
            .expect("env");
        t.insert_preview(&NewPreview {
            environment: env,
            project,
            installation_id: 7,
            repository: &repo,
            number: 12,
            epoch: 1,
            head_repository: "mallory/shop",
            branch: "feature",
            commit: &head,
            trusted: false,
            expires_at: HOUR,
            event_at: 100,
            created_by: "github:7",
        })
        .await
        .expect("insert");
        assert!(t.untrusted_environment(env).await.expect("trust"));
        assert!(
            !t.touch_preview(env, "mallory/shop", "feature", &head, 50, 9 * HOUR)
                .await
                .expect("touch"),
            "older"
        );
        assert!(
            t.touch_preview(env, "mallory/shop", "feature", &head, 200, 9 * HOUR)
                .await
                .expect("touch")
        );
        let active = t
            .active_preview(project, &repo, 12)
            .await
            .expect("read")
            .expect("active");
        assert_eq!((active.expires_at, active.last_event_at), (9 * HOUR, 200));
        assert_eq!(t.due_previews(8 * HOUR, 10).await.expect("due"), []);
        assert_eq!(t.due_previews(10 * HOUR, 10).await.expect("due").len(), 1);
        assert!(t.extend_preview(env, 20 * HOUR, false).await.expect("extend"));
        assert_eq!(t.due_previews(30 * HOUR, 10).await.expect("due"), [], "kept");
        assert!(
            t.close_preview(env, CloseReason::Closed, 300)
                .await
                .expect("close")
        );
        assert!(
            !t.close_preview(env, CloseReason::Closed, 300)
                .await
                .expect("close"),
            "once"
        );
        assert_eq!(
            t.preview_epoch(project, &repo, 12).await.expect("epoch"),
            (2, Some(300))
        );
        assert_eq!(t.active_preview_count(project).await.expect("count"), 0);
        let all = t.previews(project, true).await.expect("list");
        assert_eq!(all[0].close_reason.as_deref(), Some("closed"));
        assert!(t.previews(project, false).await.expect("list").is_empty());
        t.commit().await.expect("commit");
        assert!(store.preview_orgs().await.expect("orgs").is_empty());
    }
}

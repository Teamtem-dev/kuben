//! Git sources and build attempts (M3; ADR-028, migration 0018).
//!
//! The flow, every step a durable operation:
//!
//! ```text
//! webhook / API ──► source.sync ──(provider head read)──► observe_head
//!                                                         ├─ same head: nothing
//!                                                         └─ new head: epoch+1, older builds asked to
//!                                                            stop, build operation + attempt queued
//! build ──► claim_build_slot ──► advance_build (Job phases) ──► complete_build
//!                                                               ├─ release (digest verified)
//!                                                               └─ CAS on epoch/config/policy → run
//! ```
//!
//! Every write a worker makes is conditional on the fence of its claim and
//! runs in a [`Tenant`] transaction, so row-level security applies to the
//! worker too.

use kuben_core::{
    artifact::Digest,
    ids::{
        ApplicationId, BuildAttemptId, DeploymentRunId, OperationId, OrgId, ProjectId, ReleaseId,
        SourceBindingId, TargetId,
    },
    ops::{BuildEvent, BuildPhase, Generation, IllegalTransition, Reject, SourceEpoch},
    source::{BranchName, BuildRecipe, CommitSha, RepoName},
    time::now_ms,
};
use serde_json::{Value, json};
use sqlx::AssertSqlSafe;
use uuid::Uuid;

use super::{
    Accepted, Claim, NewAudit, NewOperation, PortableRelease, RunReason, StartDeployment, Started, Tenant,
    product::counter,
};
use crate::{Store, StoreError};

/// Provider name of GitHub App installations and deliveries.
pub const GITHUB: &str = "github";
/// Operation kind that reads a binding's branch head from the provider.
pub const SOURCE_SYNC_KIND: &str = "source.sync";
/// Operation kind of one build attempt.
pub const BUILD_KIND: &str = "build";
/// The process a build's image becomes in its release.
pub const BUILD_PROCESS: &str = "web";
/// Infrastructure retries of one build (a lost worker), counting the first.
pub const MAX_BUILD_ATTEMPTS: u32 = 3;
const SYNC_TOPIC: &str = "source.sync.requested";
const BUILD_TOPIC: &str = "build.queued";
const SLOT_LOCK: i64 = 0x6b75_6265_6e62_6c64; // "kubenbld"

const TERMINAL: [&str; 3] = ["succeeded", "failed", "cancelled"];

const UPSERT_INSTALLATION: &str = "INSERT INTO git_installations \
     (provider, installation_id, org_id, account, suspended, created_at, updated_at) \
     VALUES ($1, $2, $3, $4, FALSE, $5, $5) \
     ON CONFLICT (provider, installation_id) DO UPDATE \
     SET account = EXCLUDED.account, updated_at = EXCLUDED.updated_at \
     WHERE git_installations.org_id = EXCLUDED.org_id \
     RETURNING org_id";
const INSTALLATIONS: &str = "SELECT installation_id, account, suspended FROM git_installations \
     WHERE provider = $1 AND org_id = $2 ORDER BY installation_id";
const INSTALLATION_ORG: &str =
    "SELECT org_id, suspended FROM git_installations WHERE provider = $1 AND installation_id = $2";
const SET_SUSPENDED: &str = "UPDATE git_installations SET suspended = $3, updated_at = $4 \
     WHERE provider = $1 AND installation_id = $2";
const OWNS_INSTALLATION: &str = "SELECT EXISTS (SELECT 1 FROM git_installations \
     WHERE provider = $1 AND installation_id = $2 AND org_id = $3)";
const LOCK_TARGET: &str = "SELECT application_id, build_config_revision, deleting FROM application_targets \
     WHERE id = $1 AND org_id = $2 AND project_id = $3 FOR UPDATE";
const BINDING_COLUMNS: &str = "b.id, b.org_id, b.project_id, b.application_id, b.target_id, \
     b.installation_id, b.repository, b.repository_id, b.branch, b.recipe::text AS recipe, \
     b.image_repository, b.head_sha, b.head_epoch";
const INSERT_BINDING: &str = "INSERT INTO source_bindings \
     (id, org_id, project_id, application_id, target_id, provider, installation_id, repository, \
      branch, recipe, image_repository, created_at, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $12)";
const UPDATE_BINDING: &str = "UPDATE source_bindings \
     SET installation_id = $2, repository = $3, branch = $4, recipe = $5::jsonb, image_repository = $6, \
         repository_id = CASE WHEN repository = $3 THEN repository_id END, \
         head_sha = NULL, updated_at = $7 \
     WHERE id = $1";
const RAISE_BUILD_CONFIG: &str =
    "UPDATE application_targets SET build_config_revision = build_config_revision + 1 WHERE id = $1";
const PENDING_SYNC: &str = "SELECT id FROM operations \
     WHERE kind = $1 AND org_id = $2 AND target_id = $3 AND NOT done ORDER BY requested_at LIMIT 1";
const LOCK_FENCE: &str = "SELECT payload::text FROM operations \
     WHERE id = $1 AND fence = $2 AND NOT done AND org_id = $3 FOR UPDATE";
const LOCK_BINDING_AND_TARGET: &str = "SELECT b.head_sha, b.head_epoch, b.repository_id, \
     t.source_epoch, t.build_config_revision, t.lifecycle_uid, t.deleting \
     FROM source_bindings b JOIN application_targets t ON t.id = b.target_id \
     WHERE b.id = $1 FOR UPDATE OF b, t";
const RECORD_HEAD: &str = "UPDATE source_bindings \
     SET head_sha = $2, head_epoch = $3, repository_id = COALESCE(repository_id, $4), updated_at = $5 \
     WHERE id = $1";
const RAISE_EPOCH: &str =
    "UPDATE application_targets SET source_epoch = $2 WHERE id = $1 AND source_epoch = $2 - 1";
const ASK_OLDER_TO_STOP: &str = "UPDATE build_attempts \
     SET cancel_requested_at = COALESCE(cancel_requested_at, $2), updated_at = $2 \
     WHERE binding_id = $1 AND phase <> ALL($3) RETURNING operation_id";
const WAKE: &str = "UPDATE operations SET next_attempt_at = kuben_now_ms() WHERE id = ANY($1) AND NOT done";
const INSERT_ATTEMPT: &str = "INSERT INTO build_attempts \
     (id, org_id, project_id, application_id, target_id, binding_id, attempt_no, commit_sha, source_epoch, \
      build_config_revision, lifecycle_uid, repository, recipe, image_repository, operation_id, \
      created_at, updated_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $16)";
const ATTEMPT_COLUMNS: &str = "a.id, a.org_id, a.project_id, a.application_id, a.target_id, a.binding_id, \
     a.attempt_no, a.commit_sha, a.source_epoch, a.build_config_revision, a.lifecycle_uid, a.repository, \
     a.recipe::text AS recipe, a.image_repository, a.phase, a.blocked_reason, a.failure, a.failure_detail, \
     a.reported_digest, a.digest, a.job_name, a.release_id, a.deployment_run_id, a.deploy_decision, \
     a.operation_id, a.cancel_requested_at, a.created_at, a.started_at, a.finished_at, \
     b.installation_id, b.branch";
const LOCK_ATTEMPT_UNDER_FENCE: &str = "SELECT a.phase FROM build_attempts a \
     JOIN operations o ON o.id = a.operation_id \
     WHERE a.id = $1 AND o.id = $2 AND o.fence = $3 AND NOT o.done \
     FOR UPDATE OF a";
const SET_ATTEMPT: &str = "UPDATE build_attempts SET phase = $2, updated_at = $3, \
     job_name = COALESCE(job_name, $4), reported_digest = COALESCE(reported_digest, $5), \
     failure = COALESCE(failure, $6), failure_detail = COALESCE(failure_detail, $7), \
     blocked_reason = $8, digest = COALESCE(digest, $9), \
     started_at = CASE WHEN $2 = 'preparing' THEN COALESCE(started_at, $3) ELSE started_at END, \
     finished_at = CASE WHEN $2 = ANY($10) THEN $3 ELSE finished_at END \
     WHERE id = $1";
const RELEASE_SLOT: &str = "DELETE FROM build_slots WHERE attempt_id = $1";
const HAS_SLOT: &str = "SELECT EXISTS (SELECT 1 FROM build_slots WHERE attempt_id = $1)";
const SLOTS_IN_USE: &str = "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM build_slots";
const INSERT_SLOT: &str = "INSERT INTO build_slots (attempt_id, org_id, claimed_at) VALUES ($1, $2, $3)";
const SET_OUTPUT: &str = "UPDATE build_attempts \
     SET release_id = $2, deployment_run_id = $3, deploy_decision = $4, updated_at = $5 WHERE id = $1";
const LOCK_TARGET_GENERATION: &str =
    "SELECT desired_generation FROM application_targets WHERE id = $1 FOR UPDATE";
const REQUEST_CANCEL: &str = "UPDATE build_attempts \
     SET cancel_requested_at = COALESCE(cancel_requested_at, $3), updated_at = $3 \
     WHERE id = $1 AND target_id = $2 AND phase <> ALL($4) RETURNING operation_id";
const NEXT_ATTEMPT_NO: &str = "SELECT COALESCE(max(attempt_no), 0) + 1 FROM build_attempts \
     WHERE binding_id = $1 AND source_epoch = $2 AND build_config_revision = $3";

/// A new or changed source binding of a target.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewBinding {
    pub installation_id: u64,
    pub repository: RepoName,
    pub branch: BranchName,
    pub recipe: BuildRecipe,
    /// Where builds push, without tag or digest.
    pub image_repository: String,
}

/// A target's Git source.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SourceBinding {
    pub id: SourceBindingId,
    pub org: OrgId,
    pub project: ProjectId,
    pub application: ApplicationId,
    pub target: TargetId,
    pub installation_id: u64,
    pub repository: RepoName,
    /// The provider's id, pinned on the first verified read.
    pub repository_id: Option<u64>,
    pub branch: BranchName,
    pub recipe: BuildRecipe,
    pub image_repository: String,
    /// The last head read from the provider, and the source epoch it got.
    pub head_sha: Option<CommitSha>,
    pub head_epoch: SourceEpoch,
}

/// The outcome of [`Tenant::bind_source`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Bound {
    Created(SourceBindingId),
    /// The binding changed: the build configuration revision was raised, so
    /// builds of the old binding no longer deploy themselves.
    Changed(SourceBindingId),
    Unchanged(SourceBindingId),
    /// The installation is not linked to this organization.
    InstallationMissing,
    /// No such target, or it is being deleted.
    NotFound,
}

impl Bound {
    #[must_use]
    pub const fn binding(self) -> Option<SourceBindingId> {
        match self {
            Self::Created(id) | Self::Changed(id) | Self::Unchanged(id) => Some(id),
            Self::InstallationMissing | Self::NotFound => None,
        }
    }
}

/// One build attempt with its binding's installation and branch.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BuildAttempt {
    pub id: BuildAttemptId,
    pub org: OrgId,
    pub project: ProjectId,
    pub application: ApplicationId,
    pub target: TargetId,
    pub binding: SourceBindingId,
    pub attempt_no: u32,
    pub commit: CommitSha,
    pub source_epoch: SourceEpoch,
    pub build_config_revision: u64,
    pub lifecycle_uid: Uuid,
    pub repository: RepoName,
    pub recipe: BuildRecipe,
    pub image_repository: String,
    pub installation_id: u64,
    pub branch: BranchName,
    pub phase: BuildPhase,
    pub blocked_reason: Option<String>,
    pub failure: Option<String>,
    pub failure_detail: Option<String>,
    pub reported_digest: Option<Digest>,
    pub digest: Option<Digest>,
    pub job_name: Option<String>,
    pub release: Option<ReleaseId>,
    pub run: Option<DeploymentRunId>,
    pub deploy_decision: Option<String>,
    pub operation: OperationId,
    pub cancel_requested: bool,
    pub created_at: i64,
    pub started_at: Option<i64>,
    pub finished_at: Option<i64>,
}

impl BuildAttempt {
    /// The image reference the build pushes: the repository tagged with the
    /// short commit and attempt, so retries never overwrite each other.
    #[must_use]
    pub fn push_reference(&self) -> String {
        format!(
            "{}:{}-{}",
            self.image_repository,
            self.commit.short(),
            self.attempt_no
        )
    }
}

/// The outcome of [`Tenant::observe_head`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum HeadObserved {
    /// The provider's head is the one recorded: nothing to build.
    Unchanged,
    /// A new source epoch and a queued build of it.
    Queued {
        attempt: BuildAttemptId,
        operation: OperationId,
        epoch: SourceEpoch,
    },
    /// The provider's repository id differs from the pinned one.
    RepositoryChanged,
    /// The target is being deleted, or the binding is gone.
    Gone,
    /// The claim's fence moved on: nothing was written.
    Fenced,
}

/// What a worker records with a build event.
#[derive(Clone, Debug, Default)]
pub struct BuildProgress {
    pub job_name: Option<String>,
    pub reported_digest: Option<Digest>,
    /// A [`kuben_core::ops::BuildFailure`] code and a bounded detail.
    pub failure: Option<(String, String)>,
    pub blocked_reason: Option<String>,
}

/// The outcome of [`Tenant::advance_build`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BuildAdvance {
    Moved(BuildPhase),
    Illegal(IllegalTransition),
    Fenced,
}

/// Build admission limits (ADR-028). Zero means no build may start.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SlotLimits {
    pub total: u32,
    pub per_org: u32,
}

/// The outcome of [`Tenant::complete_build`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Completed {
    /// The release was deployed by a new run.
    Deployed {
        release: ReleaseId,
        run: DeploymentRunId,
        generation: Generation,
    },
    /// The release is kept; the target did not take it (the reason code).
    Kept {
        release: ReleaseId,
        decision: String,
    },
    Illegal(IllegalTransition),
    Fenced,
}

#[derive(sqlx::FromRow)]
struct BindingRow {
    id: Uuid,
    org_id: String,
    project_id: Uuid,
    application_id: Uuid,
    target_id: Uuid,
    installation_id: i64,
    repository: String,
    repository_id: Option<i64>,
    branch: String,
    recipe: String,
    image_repository: String,
    head_sha: Option<String>,
    head_epoch: i64,
}

#[derive(sqlx::FromRow)]
struct AttemptRow {
    id: Uuid,
    org_id: String,
    project_id: Uuid,
    application_id: Uuid,
    target_id: Uuid,
    binding_id: Uuid,
    attempt_no: i32,
    commit_sha: String,
    source_epoch: i64,
    build_config_revision: i64,
    lifecycle_uid: Uuid,
    repository: String,
    recipe: String,
    image_repository: String,
    phase: String,
    blocked_reason: Option<String>,
    failure: Option<String>,
    failure_detail: Option<String>,
    reported_digest: Option<String>,
    digest: Option<String>,
    job_name: Option<String>,
    release_id: Option<Uuid>,
    deployment_run_id: Option<Uuid>,
    deploy_decision: Option<String>,
    operation_id: Uuid,
    cancel_requested_at: Option<i64>,
    created_at: i64,
    started_at: Option<i64>,
    finished_at: Option<i64>,
    installation_id: i64,
    branch: String,
}

#[derive(sqlx::FromRow)]
struct HeadRow {
    head_sha: Option<String>,
    head_epoch: i64,
    repository_id: Option<i64>,
    source_epoch: i64,
    build_config_revision: i64,
    lifecycle_uid: Uuid,
    deleting: bool,
}

fn decode<E: std::error::Error + Send + Sync + 'static>(e: E) -> sqlx::Error {
    sqlx::Error::Decode(Box::new(e))
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

fn org_of(value: &str) -> Result<OrgId, sqlx::Error> {
    value.parse().map_err(decode)
}

fn phase_of(value: &str) -> Result<BuildPhase, sqlx::Error> {
    BuildPhase::parse(value)
        .ok_or_else(|| sqlx::Error::Decode(format!("unknown build phase {value:?}").into()))
}

fn terminal() -> Vec<String> {
    TERMINAL.iter().map(|p| (*p).to_owned()).collect()
}

impl TryFrom<BindingRow> for SourceBinding {
    type Error = sqlx::Error;

    fn try_from(r: BindingRow) -> Result<Self, Self::Error> {
        Ok(Self {
            id: SourceBindingId::from_uuid(r.id),
            org: org_of(&r.org_id)?,
            project: ProjectId::from_uuid(r.project_id),
            application: ApplicationId::from_uuid(r.application_id),
            target: TargetId::from_uuid(r.target_id),
            installation_id: counter(r.installation_id)?,
            repository: r.repository.parse().map_err(decode)?,
            repository_id: r.repository_id.map(counter).transpose()?,
            branch: r.branch.parse().map_err(decode)?,
            recipe: serde_json::from_str(&r.recipe).map_err(decode)?,
            image_repository: r.image_repository,
            head_sha: r.head_sha.map(|s| s.parse().map_err(decode)).transpose()?,
            head_epoch: SourceEpoch(counter(r.head_epoch)?),
        })
    }
}

impl TryFrom<AttemptRow> for BuildAttempt {
    type Error = sqlx::Error;

    fn try_from(r: AttemptRow) -> Result<Self, Self::Error> {
        let digest = |d: Option<String>| d.map(|d| d.parse::<Digest>().map_err(decode)).transpose();
        Ok(Self {
            id: BuildAttemptId::from_uuid(r.id),
            org: org_of(&r.org_id)?,
            project: ProjectId::from_uuid(r.project_id),
            application: ApplicationId::from_uuid(r.application_id),
            target: TargetId::from_uuid(r.target_id),
            binding: SourceBindingId::from_uuid(r.binding_id),
            attempt_no: u32::try_from(r.attempt_no).map_err(decode)?,
            commit: r.commit_sha.parse().map_err(decode)?,
            source_epoch: SourceEpoch(counter(r.source_epoch)?),
            build_config_revision: counter(r.build_config_revision)?,
            lifecycle_uid: r.lifecycle_uid,
            repository: r.repository.parse().map_err(decode)?,
            recipe: serde_json::from_str(&r.recipe).map_err(decode)?,
            image_repository: r.image_repository,
            installation_id: counter(r.installation_id)?,
            branch: r.branch.parse().map_err(decode)?,
            phase: phase_of(&r.phase)?,
            blocked_reason: r.blocked_reason,
            failure: r.failure,
            failure_detail: r.failure_detail,
            reported_digest: digest(r.reported_digest)?,
            digest: digest(r.digest)?,
            job_name: r.job_name,
            release: r.release_id.map(ReleaseId::from_uuid),
            run: r.deployment_run_id.map(DeploymentRunId::from_uuid),
            deploy_decision: r.deploy_decision,
            operation: OperationId::from_uuid(r.operation_id),
            cancel_requested: r.cancel_requested_at.is_some(),
            created_at: r.created_at,
            started_at: r.started_at,
            finished_at: r.finished_at,
        })
    }
}

/// The stable code of a refused automatic deploy.
#[must_use]
pub const fn reject_code(reject: Reject) -> &'static str {
    match reject {
        Reject::LifecycleMismatch => "LifecycleMismatch",
        Reject::Deleting => "TargetDeleting",
        Reject::StaleSource { .. } => "StaleSource",
        Reject::BuildConfigChanged => "BuildConfigChanged",
        Reject::GenerationMoved { .. } => "GenerationMoved",
        Reject::NotAutomatic(_) => "NotAutomatic",
        Reject::Exhausted => "GenerationExhausted",
    }
}

impl Store {
    /// The organization an installation is linked to, and whether it is
    /// suspended.
    pub async fn git_installation_org(
        &self,
        installation_id: u64,
    ) -> Result<Option<(OrgId, bool)>, StoreError> {
        let row: Option<(String, bool)> = sqlx::query_as(INSTALLATION_ORG)
            .bind(GITHUB)
            .bind(signed(installation_id)?)
            .fetch_optional(self.pool())
            .await?;
        Ok(row
            .map(|(org, suspended)| Ok::<_, sqlx::Error>((org_of(&org)?, suspended)))
            .transpose()?)
    }

    /// Record a suspension or its end, told by the provider. False when the
    /// installation is not linked.
    pub async fn set_installation_suspended(
        &self,
        installation_id: u64,
        suspended: bool,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(SET_SUSPENDED)
            .bind(GITHUB)
            .bind(signed(installation_id)?)
            .bind(suspended)
            .bind(now_ms())
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }
}

impl Tenant {
    /// Link a GitHub App installation to this organization. False when
    /// another organization holds it; the link never moves silently.
    pub async fn link_installation(
        &mut self,
        installation_id: u64,
        account: &str,
    ) -> Result<bool, StoreError> {
        let org: Option<String> = sqlx::query_scalar(UPSERT_INSTALLATION)
            .bind(GITHUB)
            .bind(signed(installation_id)?)
            .bind(self.org.to_string())
            .bind(account)
            .bind(now_ms())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(org.is_some())
    }

    /// This organization's installations: id, account, suspended.
    pub async fn installations(&mut self) -> Result<Vec<(u64, String, bool)>, StoreError> {
        let rows: Vec<(i64, String, bool)> = sqlx::query_as(INSTALLATIONS)
            .bind(GITHUB)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|(id, account, suspended)| Ok((counter(id)?, account, suspended)))
            .collect()
    }

    /// Bind `target` to a repository and branch, or change its binding. A
    /// change raises the target's build configuration revision and forgets
    /// the recorded head, so the next sync builds again.
    pub async fn bind_source(
        &mut self,
        project: ProjectId,
        target: TargetId,
        new: &NewBinding,
    ) -> Result<Bound, StoreError> {
        let org = self.org.to_string();
        let owns: bool = sqlx::query_scalar(OWNS_INSTALLATION)
            .bind(GITHUB)
            .bind(signed(new.installation_id)?)
            .bind(&org)
            .fetch_one(&mut *self.tx)
            .await?;
        if !owns {
            return Ok(Bound::InstallationMissing);
        }
        let locked: Option<(Uuid, i64, bool)> = sqlx::query_as(LOCK_TARGET)
            .bind(*target.as_uuid())
            .bind(&org)
            .bind(*project.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some((application, _, false)) = locked else {
            return Ok(Bound::NotFound);
        };
        let recipe = serde_json::to_string(&new.recipe).map_err(|e| sqlx::Error::Encode(e.into()))?;
        let now = now_ms();
        let Some(existing) = self.binding_of_target(target).await? else {
            let id = SourceBindingId::new();
            sqlx::query(INSERT_BINDING)
                .bind(*id.as_uuid())
                .bind(&org)
                .bind(*project.as_uuid())
                .bind(application)
                .bind(*target.as_uuid())
                .bind(GITHUB)
                .bind(signed(new.installation_id)?)
                .bind(new.repository.as_str())
                .bind(new.branch.as_str())
                .bind(&recipe)
                .bind(&new.image_repository)
                .bind(now)
                .execute(&mut *self.tx)
                .await?;
            return Ok(Bound::Created(id));
        };
        let same = existing.installation_id == new.installation_id
            && existing.repository == new.repository
            && existing.branch == new.branch
            && existing.recipe == new.recipe
            && existing.image_repository == new.image_repository;
        if same {
            return Ok(Bound::Unchanged(existing.id));
        }
        sqlx::query(UPDATE_BINDING)
            .bind(*existing.id.as_uuid())
            .bind(signed(new.installation_id)?)
            .bind(new.repository.as_str())
            .bind(new.branch.as_str())
            .bind(&recipe)
            .bind(&new.image_repository)
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        sqlx::query(RAISE_BUILD_CONFIG)
            .bind(*target.as_uuid())
            .execute(&mut *self.tx)
            .await?;
        Ok(Bound::Changed(existing.id))
    }

    /// The source binding of `target`.
    pub async fn binding_of_target(&mut self, target: TargetId) -> Result<Option<SourceBinding>, StoreError> {
        let sql = format!(
            "SELECT {BINDING_COLUMNS} FROM source_bindings b WHERE b.target_id = $1 AND b.org_id = $2"
        );
        let row: Option<BindingRow> = sqlx::query_as(AssertSqlSafe(sql))
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(SourceBinding::try_from).transpose()?)
    }

    /// A binding by id.
    pub async fn binding(&mut self, id: SourceBindingId) -> Result<Option<SourceBinding>, StoreError> {
        let sql =
            format!("SELECT {BINDING_COLUMNS} FROM source_bindings b WHERE b.id = $1 AND b.org_id = $2");
        let row: Option<BindingRow> = sqlx::query_as(AssertSqlSafe(sql))
            .bind(*id.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(SourceBinding::try_from).transpose()?)
    }

    /// This organization's bindings a push to `repository` and `branch`
    /// through `installation_id` concerns.
    pub async fn bindings_for_push(
        &mut self,
        installation_id: u64,
        repository: &RepoName,
        branch: &BranchName,
    ) -> Result<Vec<SourceBinding>, StoreError> {
        let sql = format!(
            "SELECT {BINDING_COLUMNS} FROM source_bindings b \
             WHERE b.provider = $1 AND b.installation_id = $2 AND b.repository = $3 AND b.branch = $4 \
               AND b.org_id = $5 ORDER BY b.id"
        );
        let rows: Vec<BindingRow> = sqlx::query_as(AssertSqlSafe(sql))
            .bind(GITHUB)
            .bind(signed(installation_id)?)
            .bind(repository.as_str())
            .bind(branch.as_str())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(SourceBinding::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// Ask for `binding`'s head to be read from the provider. A sync that is
    /// still pending answers instead of a second one.
    pub async fn request_sync(
        &mut self,
        binding: &SourceBinding,
        requested_by: &str,
        audit: NewAudit,
    ) -> Result<OperationId, StoreError> {
        let pending: Option<Uuid> = sqlx::query_scalar(PENDING_SYNC)
            .bind(SOURCE_SYNC_KIND)
            .bind(self.org.to_string())
            .bind(*binding.target.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        if let Some(pending) = pending {
            return Ok(OperationId::from_uuid(pending));
        }
        let mut input_hash = binding.id.as_uuid().as_bytes().to_vec();
        input_hash.extend_from_slice(Uuid::now_v7().as_bytes());
        let op = NewOperation {
            kind: SOURCE_SYNC_KIND.into(),
            target: Some((binding.project, binding.target)),
            lifecycle_uid: None,
            generation: None,
            input_hash,
            payload: json!({ "binding": binding.id }),
            requested_by: requested_by.to_owned(),
            deadline_at: None,
            topic: SYNC_TOPIC.into(),
        };
        match self.accept(&op, audit, None).await? {
            Accepted::New(id) | Accepted::Replayed(id) | Accepted::KeyReused(id) => Ok(id),
        }
    }

    /// The binding a claimed `source.sync` operation reads.
    pub async fn sync_binding(&mut self, claim: &Claim) -> Result<Option<SourceBinding>, StoreError> {
        let Some(payload) = self.fence_payload(claim).await? else {
            return Ok(None);
        };
        let Some(id) = payload
            .get("binding")
            .and_then(Value::as_str)
            .and_then(|s| s.parse::<Uuid>().ok())
        else {
            return Ok(None);
        };
        self.binding(SourceBindingId::from_uuid(id)).await
    }

    /// Lock `claim`'s operation if the claim still holds it; its payload.
    async fn fence_payload(&mut self, claim: &Claim) -> Result<Option<Value>, StoreError> {
        let payload: Option<String> = sqlx::query_scalar(LOCK_FENCE)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        payload
            .map(|p| serde_json::from_str(&p).map_err(|e| decode(e).into()))
            .transpose()
    }

    /// Record `head` as read from the provider for `binding` (plan §14.1).
    /// The same head changes nothing; a new one (a push, a force-push or a
    /// changed binding) raises the target's source epoch, asks the binding's
    /// unfinished builds to stop and queues a build of `head`.
    pub async fn observe_head(
        &mut self,
        claim: &Claim,
        binding: &SourceBinding,
        head: &CommitSha,
        repository_id: u64,
    ) -> Result<HeadObserved, StoreError> {
        if self.fence_payload(claim).await?.is_none() {
            return Ok(HeadObserved::Fenced);
        }
        let row: Option<HeadRow> = sqlx::query_as(LOCK_BINDING_AND_TARGET)
            .bind(*binding.id.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(row) = row else {
            return Ok(HeadObserved::Gone);
        };
        // Read under the lock: a concurrent rebinding cannot slip between.
        let binding = match self.binding(binding.id).await? {
            Some(b) if !row.deleting => b,
            _ => return Ok(HeadObserved::Gone),
        };
        let pinned = row.repository_id.map(counter).transpose()?;
        if pinned.is_some_and(|id| id != repository_id) {
            return Ok(HeadObserved::RepositoryChanged);
        }
        if row.head_sha.as_deref() == Some(head.as_str()) && row.head_epoch == row.source_epoch {
            return Ok(HeadObserved::Unchanged);
        }
        let epoch = SourceEpoch(counter(row.source_epoch)?.saturating_add(1));
        let now = now_ms();
        let raised = sqlx::query(RAISE_EPOCH)
            .bind(*binding.target.as_uuid())
            .bind(signed(epoch.0)?)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        if raised != 1 {
            return Err(sqlx::Error::RowNotFound.into());
        }
        sqlx::query(RECORD_HEAD)
            .bind(*binding.id.as_uuid())
            .bind(head.as_str())
            .bind(signed(epoch.0)?)
            .bind(signed(repository_id)?)
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        self.stop_older_builds(binding.id, now).await?;
        let config = counter(row.build_config_revision)?;
        let (attempt, operation) = self
            .queue_attempt(&binding, head, epoch, config, row.lifecycle_uid, 1)
            .await?;
        Ok(HeadObserved::Queued {
            attempt,
            operation,
            epoch,
        })
    }

    /// A newer head supersedes every unfinished build of the binding.
    async fn stop_older_builds(&mut self, binding: SourceBindingId, now: i64) -> Result<(), StoreError> {
        let operations: Vec<Uuid> = sqlx::query_scalar(ASK_OLDER_TO_STOP)
            .bind(*binding.as_uuid())
            .bind(now)
            .bind(terminal())
            .fetch_all(&mut *self.tx)
            .await?;
        if !operations.is_empty() {
            sqlx::query(WAKE).bind(operations).execute(&mut *self.tx).await?;
        }
        Ok(())
    }

    async fn queue_attempt(
        &mut self,
        binding: &SourceBinding,
        commit: &CommitSha,
        epoch: SourceEpoch,
        build_config_revision: u64,
        lifecycle_uid: Uuid,
        attempt_no: u32,
    ) -> Result<(BuildAttemptId, OperationId), StoreError> {
        let attempt = BuildAttemptId::new();
        let op = NewOperation {
            kind: BUILD_KIND.into(),
            target: Some((binding.project, binding.target)),
            lifecycle_uid: Some(lifecycle_uid),
            generation: None,
            input_hash: attempt.as_uuid().as_bytes().to_vec(),
            payload: json!({ "attempt": attempt, "commit": commit, "source_epoch": epoch }),
            requested_by: format!("source:{}", binding.id),
            deadline_at: None,
            topic: BUILD_TOPIC.into(),
        };
        let audit = NewAudit {
            actor_kind: "system".into(),
            actor_id: Some(format!("source:{}", binding.id)),
            action: "queueBuild".into(),
            target_kind: Some("build".into()),
            target_ref: Some(attempt.to_string()),
            outcome: "accepted".into(),
            data: Some(json!({ "commit": commit, "repository": binding.repository, "attempt": attempt_no })),
            ..NewAudit::default()
        };
        let Accepted::New(operation) = self.accept(&op, audit, None).await? else {
            return Err(sqlx::Error::Protocol("a build operation without a key was replayed".into()).into());
        };
        let recipe = serde_json::to_string(&binding.recipe).map_err(|e| sqlx::Error::Encode(e.into()))?;
        sqlx::query(INSERT_ATTEMPT)
            .bind(*attempt.as_uuid())
            .bind(self.org.to_string())
            .bind(*binding.project.as_uuid())
            .bind(*binding.application.as_uuid())
            .bind(*binding.target.as_uuid())
            .bind(*binding.id.as_uuid())
            .bind(i32::try_from(attempt_no).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(commit.as_str())
            .bind(signed(epoch.0)?)
            .bind(signed(build_config_revision)?)
            .bind(lifecycle_uid)
            .bind(binding.repository.as_str())
            .bind(&recipe)
            .bind(&binding.image_repository)
            .bind(*operation.as_uuid())
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok((attempt, operation))
    }

    /// Queue a new attempt with the inputs of `failed` after an
    /// infrastructure failure (a lost worker), unless the source moved on or
    /// [`MAX_BUILD_ATTEMPTS`] were made. `None` when no retry was queued.
    pub async fn retry_build(&mut self, failed: &BuildAttempt) -> Result<Option<BuildAttemptId>, StoreError> {
        let Some(binding) = self.binding(failed.binding).await? else {
            return Ok(None);
        };
        let row: Option<HeadRow> = sqlx::query_as(LOCK_BINDING_AND_TARGET)
            .bind(*binding.id.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        let current = row.is_some_and(|r| {
            !r.deleting
                && r.source_epoch == signed(failed.source_epoch.0).unwrap_or(-1)
                && r.build_config_revision == signed(failed.build_config_revision).unwrap_or(-1)
        });
        if !current {
            return Ok(None);
        }
        let next: i32 = sqlx::query_scalar(NEXT_ATTEMPT_NO)
            .bind(*binding.id.as_uuid())
            .bind(signed(failed.source_epoch.0)?)
            .bind(signed(failed.build_config_revision)?)
            .fetch_one(&mut *self.tx)
            .await?;
        let next = u32::try_from(next).map_err(decode)?;
        if next > MAX_BUILD_ATTEMPTS {
            return Ok(None);
        }
        // The binding's recipe may have changed only with a new revision,
        // which `current` rules out; the attempt keeps the recorded inputs.
        let binding = SourceBinding {
            recipe: failed.recipe.clone(),
            image_repository: failed.image_repository.clone(),
            repository: failed.repository.clone(),
            ..binding
        };
        let (attempt, _) = self
            .queue_attempt(
                &binding,
                &failed.commit,
                failed.source_epoch,
                failed.build_config_revision,
                failed.lifecycle_uid,
                next,
            )
            .await?;
        Ok(Some(attempt))
    }

    /// The build attempt of a claimed `build` operation.
    pub async fn build_of_operation(
        &mut self,
        operation: OperationId,
    ) -> Result<Option<BuildAttempt>, StoreError> {
        self.attempt_where("a.operation_id = $1", *operation.as_uuid())
            .await
    }

    /// A build attempt of `target`.
    pub async fn build_of_target(
        &mut self,
        target: TargetId,
        build: BuildAttemptId,
    ) -> Result<Option<BuildAttempt>, StoreError> {
        Ok(self
            .attempt_where("a.id = $1", *build.as_uuid())
            .await?
            .filter(|a| a.target == target))
    }

    async fn attempt_where(&mut self, filter: &str, id: Uuid) -> Result<Option<BuildAttempt>, StoreError> {
        let sql = format!(
            "SELECT {ATTEMPT_COLUMNS} FROM build_attempts a JOIN source_bindings b ON b.id = a.binding_id \
             WHERE {filter} AND a.org_id = $2"
        );
        let row: Option<AttemptRow> = sqlx::query_as(AssertSqlSafe(sql))
            .bind(id)
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(BuildAttempt::try_from).transpose()?)
    }

    /// The newest `limit` build attempts of `target`.
    pub async fn builds_of_target(
        &mut self,
        target: TargetId,
        limit: i64,
    ) -> Result<Vec<BuildAttempt>, StoreError> {
        let sql = format!(
            "SELECT {ATTEMPT_COLUMNS} FROM build_attempts a JOIN source_bindings b ON b.id = a.binding_id \
             WHERE a.target_id = $1 AND a.org_id = $2 ORDER BY a.created_at DESC, a.id DESC LIMIT $3"
        );
        let rows: Vec<AttemptRow> = sqlx::query_as(AssertSqlSafe(sql))
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(BuildAttempt::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// Ask `build` to stop. The worker applies the request on its next
    /// claim, which this wakes. `None` when the build is unknown or finished.
    pub async fn request_build_cancel(
        &mut self,
        target: TargetId,
        build: BuildAttemptId,
    ) -> Result<Option<OperationId>, StoreError> {
        let operation: Option<Uuid> = sqlx::query_scalar(REQUEST_CANCEL)
            .bind(*build.as_uuid())
            .bind(*target.as_uuid())
            .bind(now_ms())
            .bind(terminal())
            .fetch_optional(&mut *self.tx)
            .await?;
        if let Some(op) = operation {
            sqlx::query(WAKE).bind(vec![op]).execute(&mut *self.tx).await?;
        }
        Ok(operation.map(OperationId::from_uuid))
    }

    /// Apply `event` to `attempt` for the worker holding `claim`, by the rules
    /// of [`BuildPhase::apply`]. A terminal phase releases the build slot in
    /// the same transaction. Reaching `succeeded` needs
    /// [`Tenant::complete_build`].
    pub async fn advance_build(
        &mut self,
        claim: &Claim,
        attempt: BuildAttemptId,
        event: BuildEvent,
        progress: &BuildProgress,
    ) -> Result<BuildAdvance, StoreError> {
        self.move_attempt(claim, attempt, event, progress, None).await
    }

    async fn move_attempt(
        &mut self,
        claim: &Claim,
        attempt: BuildAttemptId,
        event: BuildEvent,
        progress: &BuildProgress,
        verified: Option<&Digest>,
    ) -> Result<BuildAdvance, StoreError> {
        let phase: Option<String> = sqlx::query_scalar(LOCK_ATTEMPT_UNDER_FENCE)
            .bind(*attempt.as_uuid())
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(phase) = phase else {
            return Ok(BuildAdvance::Fenced);
        };
        let next = match phase_of(&phase)?.apply(event) {
            Ok(next) => next,
            Err(illegal) => return Ok(BuildAdvance::Illegal(illegal)),
        };
        if (next == BuildPhase::Succeeded) != verified.is_some() {
            return Err(
                sqlx::Error::Protocol("a build succeeds exactly with a verified digest".into()).into(),
            );
        }
        let (failure, detail) = progress.failure.clone().map_or((None, None), |(code, detail)| {
            (Some(code), Some(bounded(&detail)))
        });
        sqlx::query(SET_ATTEMPT)
            .bind(*attempt.as_uuid())
            .bind(next.as_str())
            .bind(now_ms())
            .bind(progress.job_name.as_deref())
            .bind(progress.reported_digest.as_ref().map(Digest::as_str))
            .bind(failure)
            .bind(detail)
            .bind(progress.blocked_reason.as_deref())
            .bind(verified.map(Digest::as_str))
            .bind(terminal())
            .execute(&mut *self.tx)
            .await?;
        if next.is_terminal() {
            sqlx::query(RELEASE_SLOT)
                .bind(*attempt.as_uuid())
                .execute(&mut *self.tx)
                .await?;
        }
        Ok(BuildAdvance::Moved(next))
    }

    /// Take a build slot for `attempt` (ADR-028): atomic across workers, and
    /// idempotent for an attempt that holds one. False when the limits are
    /// reached, or zero.
    pub async fn claim_build_slot(
        &mut self,
        claim: &Claim,
        attempt: BuildAttemptId,
        limits: SlotLimits,
    ) -> Result<Option<bool>, StoreError> {
        if self.fence_payload(claim).await?.is_none() {
            return Ok(None);
        }
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(SLOT_LOCK)
            .execute(&mut *self.tx)
            .await?;
        let held: bool = sqlx::query_scalar(HAS_SLOT)
            .bind(*attempt.as_uuid())
            .fetch_one(&mut *self.tx)
            .await?;
        if held {
            return Ok(Some(true));
        }
        let (total, org): (i64, i64) = sqlx::query_as(SLOTS_IN_USE)
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?;
        if total >= i64::from(limits.total) || org >= i64::from(limits.per_org) {
            return Ok(Some(false));
        }
        sqlx::query(INSERT_SLOT)
            .bind(*attempt.as_uuid())
            .bind(self.org.to_string())
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(Some(true))
    }

    /// Settle `attempt` as `succeeded` with its verified `digest`, record its
    /// release and, if the target still follows this source epoch and build
    /// configuration automatically, accept a deployment run of it; all in
    /// this transaction (plan §8.2, R04).
    pub async fn complete_build(
        &mut self,
        claim: &Claim,
        attempt: &BuildAttempt,
        digest: &Digest,
    ) -> Result<Completed, StoreError> {
        match self
            .move_attempt(
                claim,
                attempt.id,
                BuildEvent::Verified,
                &BuildProgress::default(),
                Some(digest),
            )
            .await?
        {
            BuildAdvance::Moved(_) => {}
            BuildAdvance::Illegal(illegal) => return Ok(Completed::Illegal(illegal)),
            BuildAdvance::Fenced => return Ok(Completed::Fenced),
        }
        let created_by = format!("build:{}", attempt.id);
        let release = PortableRelease {
            application: attempt.application,
            artifacts: std::iter::once((BUILD_PROCESS.to_owned(), digest.clone())).collect(),
            process_contract: json!({}),
            portable_config: json!({}),
            renderer_schema: 1,
            source: Some(json!({
                "image_repository": attempt.image_repository,
                "repository": attempt.repository,
                "branch": attempt.branch,
                "commit": attempt.commit,
                "build": attempt.id,
            })),
            created_by: created_by.clone(),
        };
        let (release, _) = self.create_release(attempt.project, &release).await?;
        let (run, decision, generation) = self.autodeploy(attempt, release, &created_by).await?;
        sqlx::query(SET_OUTPUT)
            .bind(*attempt.id.as_uuid())
            .bind(*release.as_uuid())
            .bind(run.map(|r| *r.as_uuid()))
            .bind(&decision)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(match (run, generation) {
            (Some(run), Some(generation)) => Completed::Deployed {
                release,
                run,
                generation,
            },
            _ => Completed::Kept { release, decision },
        })
    }

    async fn autodeploy(
        &mut self,
        attempt: &BuildAttempt,
        release: ReleaseId,
        created_by: &str,
    ) -> Result<(Option<DeploymentRunId>, String, Option<Generation>), StoreError> {
        let generation: i64 = sqlx::query_scalar(LOCK_TARGET_GENERATION)
            .bind(*attempt.target.as_uuid())
            .fetch_one(&mut *self.tx)
            .await?;
        let Some(config_revision) = self.latest_config_revision(attempt.target).await? else {
            return Ok((None, "NoConfiguration".into(), None));
        };
        let req = StartDeployment {
            project: attempt.project,
            target: attempt.target,
            release,
            config_revision,
            render_plan: None,
            expected_generation: Generation(counter(generation)?),
            lifecycle_uid: attempt.lifecycle_uid,
            reason: RunReason::Build,
            requested_by: created_by.to_owned(),
            input_hash: attempt.id.as_uuid().as_bytes().to_vec(),
        };
        let audit = NewAudit {
            actor_kind: "system".into(),
            actor_id: Some(created_by.to_owned()),
            action: "startDeployment".into(),
            target_kind: Some("app".into()),
            target_ref: Some(attempt.target.to_string()),
            outcome: "accepted".into(),
            data: Some(json!({ "build": attempt.id, "commit": attempt.commit })),
            ..NewAudit::default()
        };
        let started = self
            .start_build_deployment(&req, attempt.source_epoch, attempt.build_config_revision, audit)
            .await?;
        Ok(match started {
            Started::Accepted { run, generation, .. } => (Some(run), "deployed".into(), Some(generation)),
            Started::Rejected(reject) => (None, reject_code(reject).into(), None),
            Started::NotFound => (None, "TargetMissing".into(), None),
            Started::SecretRevoked => (None, "SecretRevoked".into(), None),
            Started::Replayed(_) | Started::KeyReused(_) => (None, "Replayed".into(), None),
        })
    }
}

/// At most 2048 bytes, cut on a character boundary.
fn bounded(detail: &str) -> String {
    const MAX: usize = 2048;
    if detail.len() <= MAX {
        return detail.to_owned();
    }
    let mut end = MAX;
    while !detail.is_char_boundary(end) {
        end -= 1;
    }
    detail[..end].to_owned()
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use kuben_core::{ops::DeployPolicy, source::BuildStrategy};

    use super::*;
    use crate::testing::{pg_store, skip};

    const HEAD_A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const HEAD_B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    const LIMITS: SlotLimits = SlotLimits { total: 4, per_org: 2 };
    const LEASE: Duration = Duration::from_secs(30);

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        target: TargetId,
        binding: SourceBinding,
    }

    fn binding_input() -> NewBinding {
        NewBinding {
            installation_id: 7,
            repository: "acme/shop".parse().expect("repo"),
            branch: "main".parse().expect("branch"),
            recipe: BuildRecipe::default(),
            image_repository: "registry.local/acme/shop".into(),
        }
    }

    async fn fixture(store: &Store, slug: &str, installation: u64) -> Fixture {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, &format!("{slug}-shop"))
            .await
            .expect("placement");
        let app = t.create_application(project, "web", "Web").await.expect("app");
        let target = t.create_target(project, app, placement).await.expect("target");
        t.create_config_revision(project, target, &json!({ "replicas": 1 }), "test")
            .await
            .expect("config");
        assert!(t.link_installation(installation, "acme").await.expect("link"));
        let input = NewBinding {
            installation_id: installation,
            ..binding_input()
        };
        let bound = t.bind_source(project, target, &input).await.expect("bind");
        assert!(matches!(bound, Bound::Created(_)));
        let binding = t.binding_of_target(target).await.expect("read").expect("binding");
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            target,
            binding,
        }
    }

    async fn sync(store: &Store, f: &Fixture, head: &str) -> HeadObserved {
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.request_sync(&f.binding, "test", NewAudit::default())
            .await
            .expect("sync");
        t.commit().await.expect("commit");
        let claim = store
            .claim_operation("w", &[SOURCE_SYNC_KIND], LEASE)
            .await
            .expect("claim")
            .expect("due");
        let mut t = store.tenant(claim.org).await.expect("tenant");
        let binding = t.sync_binding(&claim).await.expect("read").expect("binding");
        let observed = t
            .observe_head(&claim, &binding, &head.parse().expect("sha"), 42)
            .await
            .expect("observe");
        t.commit().await.expect("commit");
        store
            .finish_operation(&claim, "succeeded", None)
            .await
            .expect("finish");
        observed
    }

    async fn claim_build(store: &Store) -> (Claim, BuildAttempt) {
        let claim = store
            .claim_operation("w", &[BUILD_KIND], LEASE)
            .await
            .expect("claim")
            .expect("due");
        let mut t = store.tenant(claim.org).await.expect("tenant");
        let attempt = t
            .build_of_operation(claim.id)
            .await
            .expect("read")
            .expect("attempt");
        t.commit().await.expect("commit");
        (claim, attempt)
    }

    async fn step(store: &Store, claim: &Claim, attempt: &BuildAttempt, event: BuildEvent) -> BuildAdvance {
        let mut t = store.tenant(claim.org).await.expect("tenant");
        let moved = t
            .advance_build(claim, attempt.id, event, &BuildProgress::default())
            .await
            .expect("advance");
        t.commit().await.expect("commit");
        moved
    }

    async fn run_to_verifying(store: &Store, claim: &Claim, attempt: &BuildAttempt) {
        for event in [
            BuildEvent::Started,
            BuildEvent::Building,
            BuildEvent::Publishing,
            BuildEvent::Published,
        ] {
            assert!(matches!(
                step(store, claim, attempt, event).await,
                BuildAdvance::Moved(_)
            ));
        }
    }

    #[tokio::test]
    async fn installations_never_move_between_organizations() {
        let Some(store) = pg_store().await else {
            return skip("installations_never_move_between_organizations");
        };
        let f = fixture(&store, "a", 7).await;
        let other = store.create_org("b", "B").await.expect("org").id;
        let mut t = store.tenant(other).await.expect("tenant");
        assert!(
            !t.link_installation(7, "evil").await.expect("link"),
            "held by another org"
        );
        let refused = t
            .bind_source(f.project, f.target, &binding_input())
            .await
            .expect("bind");
        assert_eq!(refused, Bound::InstallationMissing);
        drop(t);
        assert_eq!(
            store.git_installation_org(7).await.expect("read"),
            Some((f.org, false))
        );
        assert!(store.set_installation_suspended(7, true).await.expect("suspend"));
        assert_eq!(
            store.git_installation_org(7).await.expect("read"),
            Some((f.org, true))
        );
    }

    #[tokio::test]
    async fn the_same_head_twice_builds_once() {
        let Some(store) = pg_store().await else {
            return skip("the_same_head_twice_builds_once");
        };
        let f = fixture(&store, "a", 7).await;
        let first = sync(&store, &f, HEAD_A).await;
        assert!(
            matches!(
                first,
                HeadObserved::Queued {
                    epoch: SourceEpoch(1),
                    ..
                }
            ),
            "{first:?}"
        );
        assert_eq!(
            sync(&store, &f, HEAD_A).await,
            HeadObserved::Unchanged,
            "a redelivery"
        );
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(t.builds_of_target(f.target, 10).await.expect("list").len(), 1);
        let pushes = t
            .bindings_for_push(
                7,
                &"Acme/Shop".parse().expect("repo"),
                &"main".parse().expect("branch"),
            )
            .await
            .expect("push");
        assert_eq!(pushes.len(), 1);
        assert_eq!(pushes[0].head_sha.as_ref().map(CommitSha::as_str), Some(HEAD_A));
        assert_eq!(pushes[0].repository_id, Some(42));
    }

    #[tokio::test]
    async fn a_new_head_asks_older_builds_to_stop() {
        let Some(store) = pg_store().await else {
            return skip("a_new_head_asks_older_builds_to_stop");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let second = sync(&store, &f, HEAD_B).await;
        assert!(
            matches!(
                second,
                HeadObserved::Queued {
                    epoch: SourceEpoch(2),
                    ..
                }
            ),
            "{second:?}"
        );
        let mut t = store.tenant(f.org).await.expect("tenant");
        let builds = t.builds_of_target(f.target, 10).await.expect("list");
        assert_eq!(builds.len(), 2);
        let (newer, older) = (&builds[0], &builds[1]);
        assert_eq!(newer.commit.as_str(), HEAD_B);
        assert!(!newer.cancel_requested);
        assert!(older.cancel_requested, "the older build is asked to stop");
        // A force-push back to A is a new head too.
        drop(t);
        let back = sync(&store, &f, HEAD_A).await;
        assert!(
            matches!(
                back,
                HeadObserved::Queued {
                    epoch: SourceEpoch(3),
                    ..
                }
            ),
            "{back:?}"
        );
    }

    #[tokio::test]
    async fn a_verified_build_of_the_current_head_deploys() {
        let Some(store) = pg_store().await else {
            return skip("a_verified_build_of_the_current_head_deploys");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.claim_build_slot(&claim, attempt.id, LIMITS)
                .await
                .expect("slot"),
            Some(true)
        );
        t.commit().await.expect("commit");
        run_to_verifying(&store, &claim, &attempt).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        let done = t
            .complete_build(&claim, &attempt, &DIGEST.parse().expect("digest"))
            .await
            .expect("complete");
        let Completed::Deployed { run, generation, .. } = done else {
            panic!("not deployed: {done:?}");
        };
        assert_eq!(generation, Generation(1));
        let stored = t
            .build_of_target(f.target, attempt.id)
            .await
            .expect("read")
            .expect("attempt");
        assert_eq!(stored.phase, BuildPhase::Succeeded);
        assert_eq!(stored.run, Some(run));
        assert_eq!(stored.deploy_decision.as_deref(), Some("deployed"));
        assert!(stored.finished_at.is_some());
        let reason: String = sqlx::query_scalar("SELECT reason FROM deployment_runs WHERE id = $1")
            .bind(*run.as_uuid())
            .fetch_one(&mut *t.tx)
            .await
            .expect("run");
        assert_eq!(reason, "build");
        let has: bool = sqlx::query_scalar(HAS_SLOT)
            .bind(*attempt.id.as_uuid())
            .fetch_one(&mut *t.tx)
            .await
            .expect("slot");
        assert!(!has, "the slot went with the terminal phase");
    }

    #[tokio::test]
    async fn a_late_build_of_an_old_head_is_kept_but_not_deployed() {
        let Some(store) = pg_store().await else {
            return skip("a_late_build_of_an_old_head_is_kept_but_not_deployed");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        run_to_verifying(&store, &claim, &attempt).await;
        // B is pushed while A publishes; A's output is verified first anyway.
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.request_sync(&f.binding, "test", NewAudit::default())
            .await
            .expect("sync");
        t.commit().await.expect("commit");
        let sync_claim = store
            .claim_operation("w2", &[SOURCE_SYNC_KIND], LEASE)
            .await
            .expect("claim")
            .expect("due");
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.observe_head(&sync_claim, &f.binding, &HEAD_B.parse().expect("sha"), 42)
            .await
            .expect("observe");
        t.commit().await.expect("commit");

        let mut t = store.tenant(f.org).await.expect("tenant");
        let done = t
            .complete_build(&claim, &attempt, &DIGEST.parse().expect("digest"))
            .await
            .expect("complete");
        assert!(
            matches!(&done, Completed::Kept { decision, .. } if decision == "StaleSource"),
            "{done:?}"
        );
        let state = t.target_state(f.target).await.expect("read").expect("target");
        assert_eq!(
            state.desired_generation,
            Generation(0),
            "production did not move back"
        );
    }

    #[tokio::test]
    async fn a_pinned_or_reconfigured_target_keeps_the_release() {
        let Some(store) = pg_store().await else {
            return skip("a_pinned_or_reconfigured_target_keeps_the_release");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        run_to_verifying(&store, &claim, &attempt).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        let changed = NewBinding {
            recipe: BuildRecipe {
                strategy: BuildStrategy::Railpack,
                ..BuildRecipe::default()
            },
            ..binding_input()
        };
        assert!(matches!(
            t.bind_source(f.project, f.target, &changed).await.expect("bind"),
            Bound::Changed(_)
        ));
        let done = t
            .complete_build(&claim, &attempt, &DIGEST.parse().expect("digest"))
            .await
            .expect("complete");
        assert!(
            matches!(&done, Completed::Kept { decision, .. } if decision == "BuildConfigChanged"),
            "{done:?}"
        );
        let state = t.target_state(f.target).await.expect("read").expect("target");
        assert_eq!(state.policy, DeployPolicy::Auto);
    }

    #[tokio::test]
    async fn slots_are_limited_per_org_and_released_when_final() {
        let Some(store) = pg_store().await else {
            return skip("slots_are_limited_per_org_and_released_when_final");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        let one = SlotLimits { total: 1, per_org: 1 };
        let zero = SlotLimits { total: 0, per_org: 0 };
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.claim_build_slot(&claim, attempt.id, zero).await.expect("slot"),
            Some(false)
        );
        assert_eq!(
            t.claim_build_slot(&claim, attempt.id, one).await.expect("slot"),
            Some(true)
        );
        assert_eq!(
            t.claim_build_slot(&claim, attempt.id, one).await.expect("slot"),
            Some(true),
            "idempotent for its holder"
        );
        t.commit().await.expect("commit");

        let g = fixture(&store, "b", 8).await;
        sync(&store, &g, HEAD_A).await;
        let (other_claim, other) = claim_build(&store).await;
        let mut t = store.tenant(g.org).await.expect("tenant");
        assert_eq!(
            t.claim_build_slot(&other_claim, other.id, one)
                .await
                .expect("slot"),
            Some(false),
            "the only slot is taken"
        );
        t.commit().await.expect("commit");

        let moved = step(&store, &claim, &attempt, BuildEvent::CancelRequested).await;
        assert_eq!(moved, BuildAdvance::Moved(BuildPhase::Cancelled));
        let mut t = store.tenant(g.org).await.expect("tenant");
        assert_eq!(
            t.claim_build_slot(&other_claim, other.id, one)
                .await
                .expect("slot"),
            Some(true),
            "released by the terminal phase"
        );
    }

    #[tokio::test]
    async fn fenced_workers_and_final_attempts_change_nothing() {
        let Some(store) = pg_store().await else {
            return skip("fenced_workers_and_final_attempts_change_nothing");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        let stale = Claim {
            fence: claim.fence - 1,
            ..claim.clone()
        };
        assert_eq!(
            step(&store, &stale, &attempt, BuildEvent::Started).await,
            BuildAdvance::Fenced
        );
        let failed = BuildProgress {
            failure: Some(("OutOfMemory".into(), "x".repeat(5000))),
            ..BuildProgress::default()
        };
        let mut t = store.tenant(f.org).await.expect("tenant");
        let moved = t
            .advance_build(&claim, attempt.id, BuildEvent::Failed, &failed)
            .await
            .expect("fail");
        assert_eq!(moved, BuildAdvance::Moved(BuildPhase::Failed));
        let stored = t
            .build_of_target(f.target, attempt.id)
            .await
            .expect("read")
            .expect("attempt");
        assert_eq!(stored.failure.as_deref(), Some("OutOfMemory"));
        assert_eq!(stored.failure_detail.map(|d| d.len()), Some(2048));
        let again = t
            .advance_build(&claim, attempt.id, BuildEvent::Started, &BuildProgress::default())
            .await
            .expect("illegal");
        assert!(matches!(again, BuildAdvance::Illegal(i) if i.terminal));
        let forced = sqlx::query("UPDATE build_attempts SET phase = 'running' WHERE id = $1")
            .bind(*attempt.id.as_uuid())
            .execute(&mut *t.tx)
            .await;
        assert!(forced.is_err(), "the database refuses to reopen a final attempt");
    }

    #[tokio::test]
    async fn a_lost_worker_is_retried_as_a_new_attempt() {
        let Some(store) = pg_store().await else {
            return skip("a_lost_worker_is_retried_as_a_new_attempt");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        for round in 1..=MAX_BUILD_ATTEMPTS {
            let (claim, attempt) = claim_build(&store).await;
            assert_eq!(attempt.attempt_no, round);
            step(&store, &claim, &attempt, BuildEvent::Failed).await;
            store
                .finish_operation(&claim, "failed", Some("LostWorker"))
                .await
                .expect("finish");
            let mut t = store.tenant(f.org).await.expect("tenant");
            let retried = t.retry_build(&attempt).await.expect("retry");
            assert_eq!(retried.is_some(), round < MAX_BUILD_ATTEMPTS, "round {round}");
            t.commit().await.expect("commit");
        }
    }

    #[tokio::test]
    async fn a_running_build_is_cancelled_only_after_its_job_is_gone() {
        let Some(store) = pg_store().await else {
            return skip("a_running_build_is_cancelled_only_after_its_job_is_gone");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.claim_build_slot(&claim, attempt.id, LIMITS)
                .await
                .expect("slot"),
            Some(true)
        );
        t.commit().await.expect("commit");
        step(&store, &claim, &attempt, BuildEvent::Started).await;
        step(&store, &claim, &attempt, BuildEvent::Building).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert!(
            t.request_build_cancel(f.target, attempt.id)
                .await
                .expect("cancel")
                .is_some()
        );
        t.commit().await.expect("commit");
        for (event, phase) in [
            (BuildEvent::CancelRequested, BuildPhase::CancelRequested),
            (BuildEvent::Stopping, BuildPhase::Cancelling),
        ] {
            assert_eq!(
                step(&store, &claim, &attempt, event).await,
                BuildAdvance::Moved(phase)
            );
        }
        let mut t = store.tenant(f.org).await.expect("tenant");
        let held: bool = sqlx::query_scalar(HAS_SLOT)
            .bind(*attempt.id.as_uuid())
            .fetch_one(&mut *t.tx)
            .await
            .expect("slot");
        assert!(held, "the Job may still run while it is being deleted");
        drop(t);
        let moved = step(&store, &claim, &attempt, BuildEvent::Stopped).await;
        assert_eq!(moved, BuildAdvance::Moved(BuildPhase::Cancelled));
        let mut t = store.tenant(f.org).await.expect("tenant");
        let held: bool = sqlx::query_scalar(HAS_SLOT)
            .bind(*attempt.id.as_uuid())
            .fetch_one(&mut *t.tx)
            .await
            .expect("slot");
        assert!(!held);
        let stored = t
            .build_of_target(f.target, attempt.id)
            .await
            .expect("read")
            .expect("attempt");
        assert!(stored.finished_at.is_some() && stored.failure.is_none());
        assert_eq!(
            t.request_build_cancel(f.target, attempt.id)
                .await
                .expect("cancel"),
            None,
            "a finished build cannot be cancelled"
        );
    }

    #[tokio::test]
    async fn cancelling_wakes_the_build() {
        let Some(store) = pg_store().await else {
            return skip("cancelling_wakes_the_build");
        };
        let f = fixture(&store, "a", 7).await;
        sync(&store, &f, HEAD_A).await;
        let (claim, attempt) = claim_build(&store).await;
        store
            .retry_operation(&claim, Duration::from_hours(1), "Waiting")
            .await
            .expect("park");
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.request_build_cancel(f.target, attempt.id)
                .await
                .expect("cancel"),
            Some(attempt.operation)
        );
        t.commit().await.expect("commit");
        let (again, read) = claim_build(&store).await;
        assert_eq!(again.id, claim.id, "woken before its hour");
        assert!(read.cancel_requested);
    }
}

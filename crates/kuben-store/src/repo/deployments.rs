//! Releases, target configuration revisions, render plans and deployment runs
//! (ADR-026; plan §8.1, §8.5, §8.7, §8.9).
//!
//! [`Tenant::start_deployment`] is the transactional acceptance of plan §8.2.
//! It locks the target, answers a replayed `Idempotency-Key` with the first
//! receipt, decides with the pure rules of [`TargetState`], and only then
//! writes the operation, the raised generation, the superseding of older
//! runs and the new run, all in the caller's transaction.

use std::collections::BTreeMap;

use kuben_core::{
    artifact::Digest,
    ids::{
        ApplicationId, ConfigRevisionId, DeploymentRunId, OperationId, ProjectId, ReleaseId, RenderPlanId,
        TargetId,
    },
    ops::{
        AutodeployRequest, DeployPolicy, Generation, IllegalTransition, Reject, RunEvent, RunPhase,
        SourceEpoch, TargetState,
    },
    policy::ChangeKind,
    scan::GateVerdict,
    time::now_ms,
};
use serde_json::{Value, json};
use uuid::Uuid;

use super::{
    Accepted, Claim, IdempotencyKey, NewAudit, NewOperation, Tenant,
    product::{counter, deploy_policy},
};
use crate::{Store, StoreError};

/// Operation kind and outbox topic of an accepted deployment run.
pub const RUN_KIND: &str = "deployment";
const RUN_TOPIC: &str = "deployment.accepted";

const INSERT_RELEASE: &str = "INSERT INTO releases \
     (id, org_id, project_id, application_id, artifacts, process_contract, portable_config, \
      renderer_schema, source, content_hash, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7::jsonb, $8, $9::jsonb, \
             sha256(convert_to($10, 'UTF8')), $11, $12) \
     ON CONFLICT (application_id, content_hash) DO NOTHING \
     RETURNING id";
const SELECT_RELEASE_BY_CONTENT: &str = "SELECT id FROM releases \
     WHERE org_id = $1 AND project_id = $2 AND application_id = $3 \
       AND content_hash = sha256(convert_to($4, 'UTF8'))";
const NEXT_CONFIG_REVISION: &str = "UPDATE application_targets SET config_revision_seq = config_revision_seq + 1 \
     WHERE id = $1 AND org_id = $2 AND project_id = $3 RETURNING config_revision_seq";
const INSERT_CONFIG_REVISION: &str = "INSERT INTO target_config_revisions \
     (id, org_id, project_id, target_id, revision, config, config_hash, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6::jsonb, sha256(convert_to($7, 'UTF8')), $8, $9)";
const INSERT_RENDER_PLAN: &str = "INSERT INTO render_plans \
     (id, org_id, content_digest, renderer_version, capability_snapshot, resources, created_at) \
     VALUES ($1, $2, sha256(convert_to($3, 'UTF8')), $4, $5::jsonb, $6::jsonb, $7) \
     ON CONFLICT (org_id, content_digest) DO NOTHING \
     RETURNING id";
const SELECT_RENDER_PLAN_BY_CONTENT: &str =
    "SELECT id FROM render_plans WHERE org_id = $1 AND content_digest = sha256(convert_to($2, 'UTF8'))";
const LOCK_TARGET: &str = "SELECT application_id, lifecycle_uid, deleting, desired_generation, source_epoch, \
     build_config_revision, deploy_policy FROM application_targets \
     WHERE id = $1 AND org_id = $2 AND project_id = $3 \
     FOR UPDATE";
const LIVE_RECEIPT: &str = "SELECT request_hash, operation_id FROM idempotency_receipts \
     WHERE org_id = $1 AND actor = $2 AND operation = $3 AND key = $4 AND expires_at > $5";
const RELEASE_OF_APPLICATION: &str = "SELECT EXISTS (SELECT 1 FROM releases \
     WHERE id = $1 AND org_id = $2 AND project_id = $3 AND application_id = $4)";
const REVISION_OF_TARGET: &str = "SELECT EXISTS (SELECT 1 FROM target_config_revisions \
     WHERE id = $1 AND org_id = $2 AND target_id = $3)";
const PLAN_OF_ORG: &str = "SELECT EXISTS (SELECT 1 FROM render_plans WHERE id = $1 AND org_id = $2)";
const RAISE_GENERATION: &str = "UPDATE application_targets SET desired_generation = $3, deploy_policy = $4 \
     WHERE id = $1 AND desired_generation = $2";
const SUPERSEDE_OLDER: &str = "UPDATE deployment_runs SET phase = 'superseded', updated_at = $3 \
     WHERE target_id = $1 AND generation < $2 AND phase <> ALL($4) RETURNING operation_id";
/// Superseded runs settle at once, even when parked for an approval.
const WAKE_SUPERSEDED: &str = "UPDATE operations SET next_attempt_at = kuben_now_ms() \
     WHERE id = ANY($1) AND NOT done AND next_attempt_at > kuben_now_ms()";
const INSERT_RUN: &str = "INSERT INTO deployment_runs \
     (id, org_id, project_id, application_id, target_id, release_id, config_revision_id, render_plan_id, \
      generation, lifecycle_uid, reason, requested_by, operation_id, created_at, updated_at, restarted_at, \
      approvals_required, approval_expires_at, policy_revision, approval_plan_hash, emergency_reason) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, \
       CASE WHEN $11 = 'restart' THEN $14 ELSE (SELECT d.restarted_at FROM deployment_runs d \
         WHERE d.target_id = $5 AND d.org_id = $2 ORDER BY d.generation DESC LIMIT 1) END, \
       $15, $16, $17, \
       CASE WHEN $15 > 0 THEN sha256(convert_to(concat_ws('/', $1::text, $5::text, $6::text, $7::text, \
         $9::text, $10::text, $11::text), 'UTF8')) END, $18)";
const AWAIT_APPROVAL: &str =
    "UPDATE deployment_runs SET phase = $2, updated_at = $3 WHERE id = $1 AND phase = $4";
const PARK_OPERATION: &str = "UPDATE operations SET next_attempt_at = $2 WHERE id = $1 AND NOT done";
const SELECT_RUN: &str = "SELECT phase, generation FROM deployment_runs WHERE id = $1 AND org_id = $2";
const LOCK_RUN_UNDER_FENCE: &str = "SELECT r.phase FROM deployment_runs r \
     JOIN operations o ON o.id = r.operation_id \
     WHERE r.id = $1 AND o.id = $2 AND o.fence = $3 AND NOT o.done \
     FOR UPDATE OF r";
const SET_RUN_PHASE: &str = "UPDATE deployment_runs SET phase = $2, updated_at = $3, \
     outcome = COALESCE(outcome, $4), recovery_outcome = COALESCE(recovery_outcome, $5) WHERE id = $1";

/// Input for [`Tenant::create_release`]: portable and immutable (I03).
#[derive(Clone, Debug)]
pub struct PortableRelease {
    pub application: ApplicationId,
    /// Process name → digest; at least one.
    pub artifacts: BTreeMap<String, Digest>,
    pub process_contract: Value,
    /// Non-secret configuration that travels with the release.
    pub portable_config: Value,
    pub renderer_schema: i32,
    /// Source and build provenance references.
    pub source: Option<Value>,
    pub created_by: String,
}

/// Why a run exists (plan §8.7).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RunReason {
    Deploy,
    /// An older release; pins the target until automatic deploys resume.
    Rollback,
    /// The same release and digests on another target, without a build.
    Promotion,
    /// The same release and configuration with a new restart stamp: every
    /// pod is replaced (an app its cluster's agent delivers, M1.9).
    Restart,
    /// The same release and configuration, now through the cluster's agent,
    /// which adopts the workloads the App controller made (M1.9).
    Handover,
    /// A verified build of the target's current source head, deployed by the
    /// compare-and-set of [`TargetState::try_autodeploy`] (M3).
    Build,
    /// A rollback by a person with a reason that passes approvals, freezes,
    /// the scan gate and a pause (M4.9, [`Tenant::start_emergency_rollback`]).
    Emergency,
    /// The same release and configuration with the current revisions of the
    /// secrets it references (M4.4).
    Rotation,
}

impl RunReason {
    /// What the run changes, for the environment's approval rules.
    #[must_use]
    pub const fn change_kind(self) -> ChangeKind {
        match self {
            Self::Deploy => ChangeKind::Deploy,
            Self::Rollback => ChangeKind::Rollback,
            Self::Promotion => ChangeKind::Promotion,
            Self::Restart => ChangeKind::Restart,
            Self::Handover => ChangeKind::Handover,
            Self::Build => ChangeKind::Build,
            Self::Rotation => ChangeKind::Rotation,
            Self::Emergency => ChangeKind::Emergency,
        }
    }

    /// Whether the run may deliver a release the target does not run yet:
    /// what the scan gate judges. Restarts, handovers and rotations keep the
    /// release, and an emergency must not wait for a feed.
    #[must_use]
    pub const fn carries_new_code(self) -> bool {
        !matches!(self, Self::Restart | Self::Handover | Self::Rotation)
    }

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Deploy => "deploy",
            Self::Rollback => "rollback",
            Self::Promotion => "promotion",
            Self::Restart => "restart",
            Self::Handover => "handover",
            Self::Build => "build",
            Self::Rotation => "rotation",
            Self::Emergency => "emergency",
        }
    }
}

/// Input for [`Tenant::start_deployment`].
#[derive(Clone, Debug)]
pub struct StartDeployment {
    pub project: ProjectId,
    pub target: TargetId,
    pub release: ReleaseId,
    pub config_revision: ConfigRevisionId,
    pub render_plan: Option<RenderPlanId>,
    /// The generation the caller saw: there is no implicit last-writer-wins.
    pub expected_generation: Generation,
    /// The lifecycle UID the caller saw: work for a recreated target is refused.
    pub lifecycle_uid: Uuid,
    pub reason: RunReason,
    pub requested_by: String,
    /// Hash of the canonical request, compared on `Idempotency-Key` replays.
    pub input_hash: Vec<u8>,
}

/// The outcome of [`Tenant::start_deployment`]. Only `Accepted` wrote anything.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Started {
    Accepted {
        operation: OperationId,
        run: DeploymentRunId,
        generation: Generation,
        /// Approvals the environment's policy requires before delivery; the
        /// run waits in `awaitingApproval` when this is not zero.
        approvals_required: u8,
    },
    /// The same key and request were accepted before.
    Replayed(OperationId),
    /// The key was used for another request.
    KeyReused(OperationId),
    /// The target moved on, is pinned, deleting or was recreated.
    Rejected(Reject),
    /// No such target, or the release, revision or plan is not its own.
    NotFound,
    /// The current revision of a secret the configuration references is
    /// revoked: a new value must be set first.
    SecretRevoked,
    /// The environment's scan gate refuses the release (M4.6).
    VulnerabilityBlocked,
    /// The environment is frozen (M4.9): only an emergency rollback passes.
    Frozen,
    /// An untrusted preview (a fork's, M5.1) would bind a secret or a
    /// registry login.
    Untrusted,
}

/// The outcome of [`Store::advance_run`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Advance {
    Moved(RunPhase),
    /// The event is not allowed in the current phase; nothing changed.
    Illegal(IllegalTransition),
    /// The claim's fence moved on or the operation is settled: stop.
    Fenced,
}

#[derive(sqlx::FromRow)]
struct LockedTarget {
    application_id: Uuid,
    lifecycle_uid: Uuid,
    deleting: bool,
    desired_generation: i64,
    source_epoch: i64,
    build_config_revision: i64,
    deploy_policy: String,
}

const fn policy_str(policy: DeployPolicy) -> &'static str {
    match policy {
        DeployPolicy::Auto => "auto",
        DeployPolicy::Manual => "manual",
        DeployPolicy::Pinned => "pinned",
    }
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl LockedTarget {
    fn state(&self) -> Result<TargetState, sqlx::Error> {
        Ok(TargetState {
            lifecycle_uid: self.lifecycle_uid,
            deleting: self.deleting,
            desired_generation: Generation(counter(self.desired_generation)?),
            source_epoch: SourceEpoch(counter(self.source_epoch)?),
            build_config_revision: counter(self.build_config_revision)?,
            policy: deploy_policy(&self.deploy_policy)?,
        })
    }
}

impl Tenant {
    /// Record `release` in `project`. Returns its id and `true`, or the id of
    /// an identical release of the same application and `false`.
    pub async fn create_release(
        &mut self,
        project: ProjectId,
        release: &PortableRelease,
    ) -> Result<(ReleaseId, bool), StoreError> {
        let content = json!({
            "artifacts": release.artifacts,
            "process_contract": release.process_contract,
            "portable_config": release.portable_config,
            "renderer_schema": release.renderer_schema,
        })
        .to_string();
        let org = self.org.to_string();
        let inserted: Option<Uuid> = sqlx::query_scalar(INSERT_RELEASE)
            .bind(*ReleaseId::new().as_uuid())
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(*release.application.as_uuid())
            .bind(json!(release.artifacts).to_string())
            .bind(release.process_contract.to_string())
            .bind(release.portable_config.to_string())
            .bind(release.renderer_schema)
            .bind(release.source.as_ref().map(Value::to_string))
            .bind(&content)
            .bind(&release.created_by)
            .bind(now_ms())
            .fetch_optional(&mut *self.tx)
            .await?;
        if let Some(id) = inserted {
            return Ok((ReleaseId::from_uuid(id), true));
        }
        let existing: Uuid = sqlx::query_scalar(SELECT_RELEASE_BY_CONTENT)
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(*release.application.as_uuid())
            .bind(&content)
            .fetch_one(&mut *self.tx)
            .await?;
        Ok((ReleaseId::from_uuid(existing), false))
    }

    /// Record the next configuration revision of `target`. `None` when the
    /// organization has no such target.
    pub async fn create_config_revision(
        &mut self,
        project: ProjectId,
        target: TargetId,
        config: &Value,
        created_by: &str,
    ) -> Result<Option<(ConfigRevisionId, u64)>, StoreError> {
        let org = self.org.to_string();
        let seq: Option<i64> = sqlx::query_scalar(NEXT_CONFIG_REVISION)
            .bind(*target.as_uuid())
            .bind(&org)
            .bind(*project.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(revision) = seq else {
            return Ok(None);
        };
        let id = ConfigRevisionId::new();
        let text = config.to_string();
        sqlx::query(INSERT_CONFIG_REVISION)
            .bind(*id.as_uuid())
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(*target.as_uuid())
            .bind(revision)
            .bind(&text)
            .bind(&text)
            .bind(created_by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(Some((id, counter(revision)?)))
    }

    /// Freeze a render plan, addressed by its content: the same content in
    /// the same organization is the same plan.
    pub async fn freeze_render_plan(
        &mut self,
        renderer_version: &str,
        capability_snapshot: &Value,
        resources: &Value,
    ) -> Result<RenderPlanId, StoreError> {
        let content = json!({
            "renderer_version": renderer_version,
            "capability_snapshot": capability_snapshot,
            "resources": resources,
        })
        .to_string();
        let org = self.org.to_string();
        let inserted: Option<Uuid> = sqlx::query_scalar(INSERT_RENDER_PLAN)
            .bind(*RenderPlanId::new().as_uuid())
            .bind(&org)
            .bind(&content)
            .bind(renderer_version)
            .bind(capability_snapshot.to_string())
            .bind(resources.to_string())
            .bind(now_ms())
            .fetch_optional(&mut *self.tx)
            .await?;
        let id = match inserted {
            Some(id) => id,
            None => {
                sqlx::query_scalar(SELECT_RENDER_PLAN_BY_CONTENT)
                    .bind(&org)
                    .bind(&content)
                    .fetch_one(&mut *self.tx)
                    .await?
            }
        };
        Ok(RenderPlanId::from_uuid(id))
    }

    /// Accept a deployment run (plan §8.2): nothing is written unless the
    /// result is [`Started::Accepted`], and then the operation, its audit
    /// record and outbox message, the raised generation, the superseding of
    /// older runs and the run commit together with this transaction.
    pub async fn start_deployment(
        &mut self,
        req: &StartDeployment,
        audit: NewAudit,
        idempotency: Option<&IdempotencyKey>,
    ) -> Result<Started, StoreError> {
        let reason = req.reason;
        if reason == RunReason::Emergency {
            return Err(sqlx::Error::Protocol("an emergency rollback needs its reason".into()).into());
        }
        self.start_with(
            req,
            audit,
            idempotency,
            None,
            |state, lifecycle, expected| match reason {
                RunReason::Rollback | RunReason::Emergency => state.rollback(lifecycle, expected),
                RunReason::Deploy
                | RunReason::Promotion
                | RunReason::Restart
                | RunReason::Handover
                | RunReason::Build
                | RunReason::Rotation => state.deploy_explicit(lifecycle, expected),
            },
        )
        .await
    }

    /// Accept an emergency rollback to `req.release` (M4.9): a person's
    /// break-glass that passes approvals, a freeze, the scan gate and a
    /// pause. It pins the target like any rollback. `why` is kept on the run.
    pub async fn start_emergency_rollback(
        &mut self,
        req: &StartDeployment,
        why: &str,
        audit: NewAudit,
    ) -> Result<Started, StoreError> {
        let mut req = req.clone();
        req.reason = RunReason::Emergency;
        self.start_with(&req, audit, None, Some(why), |state, lifecycle, expected| {
            state.rollback(lifecycle, expected)
        })
        .await
    }

    /// Accept the automatic deploy of a verified build (M3). The target must
    /// still be on `source_epoch` and `build_config_revision` and follow
    /// automatic deploys; otherwise the result is [`Started::Rejected`] and
    /// nothing is written. `req.expected_generation` is the generation read
    /// under the same row lock, so only these checks decide.
    pub async fn start_build_deployment(
        &mut self,
        req: &StartDeployment,
        source_epoch: SourceEpoch,
        build_config_revision: u64,
        audit: NewAudit,
    ) -> Result<Started, StoreError> {
        self.start_with(
            req,
            audit,
            None,
            None,
            |state, lifecycle_uid, expected_generation| {
                state.try_autodeploy(AutodeployRequest {
                    lifecycle_uid,
                    source_epoch,
                    build_config_revision,
                    expected_generation,
                })
            },
        )
        .await
    }

    async fn start_with(
        &mut self,
        req: &StartDeployment,
        audit: NewAudit,
        idempotency: Option<&IdempotencyKey>,
        emergency: Option<&str>,
        decide: impl FnOnce(&mut TargetState, Uuid, Generation) -> Result<Generation, Reject>,
    ) -> Result<Started, StoreError> {
        // The row lock serializes every decision about this target.
        let row: Option<LockedTarget> = sqlx::query_as(LOCK_TARGET)
            .bind(*req.target.as_uuid())
            .bind(self.org.to_string())
            .bind(*req.project.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(row) = row else {
            return Ok(Started::NotFound);
        };
        if let Some(replay) = self.live_receipt(idempotency, &req.input_hash).await? {
            return Ok(replay);
        }
        if !self.owns_inputs(req, row.application_id).await? {
            return Ok(Started::NotFound);
        }

        let mut state = row.state()?;
        let generation = match decide(&mut state, req.lifecycle_uid, req.expected_generation) {
            Ok(generation) => generation,
            Err(reject) => return Ok(Started::Rejected(reject)),
        };
        let secrets = self
            .wanted_secrets(req.config_revision, req.target, req.release)
            .await?;
        if secrets.iter().any(|s| s.revoked) {
            return Ok(Started::SecretRevoked);
        }
        if !secrets.is_empty() && self.untrusted_target(req.target).await? {
            return Ok(Started::Untrusted);
        }
        let governed = req.reason.carries_new_code() && emergency.is_none();
        if governed && self.active_freeze(req.target, now_ms()).await?.is_some() {
            return Ok(Started::Frozen);
        }
        if governed
            && let Some(GateVerdict::Block(reasons)) =
                self.scan_verdict(req.target, req.release, now_ms()).await?
        {
            tracing::info!(target = %req.target, release = %req.release, ?reasons, "the scan gate refused a run");
            return Ok(Started::VulnerabilityBlocked);
        }

        let run = DeploymentRunId::new();
        let op = NewOperation {
            kind: RUN_KIND.into(),
            target: Some((req.project, req.target)),
            lifecycle_uid: Some(req.lifecycle_uid),
            generation: Some(generation.0),
            input_hash: req.input_hash.clone(),
            payload: json!({
                "run": run,
                "release": req.release,
                "config_revision": req.config_revision,
                "render_plan": req.render_plan,
                "reason": req.reason.as_str(),
            }),
            requested_by: req.requested_by.clone(),
            deadline_at: None,
            topic: RUN_TOPIC.into(),
        };
        let operation = match self.accept(&op, audit, idempotency).await? {
            Accepted::New(id) => id,
            Accepted::Replayed(id) => return Ok(Started::Replayed(id)),
            Accepted::KeyReused(id) => return Ok(Started::KeyReused(id)),
        };
        let approvals_required = self
            .record_run(
                req,
                row.application_id,
                run,
                operation,
                generation,
                state.policy,
                emergency,
            )
            .await?;
        self.bind_run_secrets(run, &secrets).await?;
        Ok(Started::Accepted {
            operation,
            run,
            generation,
            approvals_required,
        })
    }

    /// A replay answers with the first receipt, even if the generation it
    /// expected has moved on since.
    async fn live_receipt(
        &mut self,
        idempotency: Option<&IdempotencyKey>,
        input_hash: &[u8],
    ) -> Result<Option<Started>, StoreError> {
        let Some(k) = idempotency else {
            return Ok(None);
        };
        let receipt: Option<(Vec<u8>, Uuid)> = sqlx::query_as(LIVE_RECEIPT)
            .bind(self.org.to_string())
            .bind(&k.actor)
            .bind(RUN_KIND)
            .bind(&k.key)
            .bind(now_ms())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(receipt.map(|(hash, operation)| {
            let operation = OperationId::from_uuid(operation);
            if hash == input_hash {
                Started::Replayed(operation)
            } else {
                Started::KeyReused(operation)
            }
        }))
    }

    /// Raise the target's generation, supersede its older unsettled runs and
    /// insert the accepted run under the environment's policy: a run that
    /// needs approvals waits in `awaitingApproval`, its operation parked
    /// until the approval window closes (a decision wakes it). The approvals
    /// required.
    async fn record_run(
        &mut self,
        req: &StartDeployment,
        application: Uuid,
        run: DeploymentRunId,
        operation: OperationId,
        generation: Generation,
        policy: DeployPolicy,
        emergency: Option<&str>,
    ) -> Result<u8, StoreError> {
        let revision = self.policy_of_target(req.target).await?;
        let approvals = revision
            .as_ref()
            .map_or(0, |r| r.policy.approvals_for(req.reason.change_kind()));
        let raised = sqlx::query(RAISE_GENERATION)
            .bind(*req.target.as_uuid())
            .bind(signed(req.expected_generation.0)?)
            .bind(signed(generation.0)?)
            .bind(policy_str(policy))
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        if raised != 1 {
            // The row is locked by this transaction; anything else is a bug.
            return Err(sqlx::Error::RowNotFound.into());
        }
        let now = now_ms();
        let expires_at = revision
            .as_ref()
            .filter(|_| approvals > 0)
            .map(|r| now.saturating_add(i64::from(r.policy.approval_ttl_secs) * 1000));
        let settled: Vec<String> = RunPhase::ALL
            .into_iter()
            .filter(|p| p.is_final())
            .map(|p| p.as_str().to_owned())
            .collect();
        let superseded: Vec<Uuid> = sqlx::query_scalar(SUPERSEDE_OLDER)
            .bind(*req.target.as_uuid())
            .bind(signed(generation.0)?)
            .bind(now)
            .bind(settled)
            .fetch_all(&mut *self.tx)
            .await?;
        if !superseded.is_empty() {
            sqlx::query(WAKE_SUPERSEDED)
                .bind(superseded)
                .execute(&mut *self.tx)
                .await?;
        }
        sqlx::query(INSERT_RUN)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .bind(*req.project.as_uuid())
            .bind(application)
            .bind(*req.target.as_uuid())
            .bind(*req.release.as_uuid())
            .bind(*req.config_revision.as_uuid())
            .bind(req.render_plan.map(|p| *p.as_uuid()))
            .bind(signed(generation.0)?)
            .bind(req.lifecycle_uid)
            .bind(req.reason.as_str())
            .bind(&req.requested_by)
            .bind(*operation.as_uuid())
            .bind(now)
            .bind(i16::from(approvals))
            .bind(expires_at)
            .bind(revision.as_ref().map(|r| signed(r.revision)).transpose()?)
            .bind(emergency)
            .execute(&mut *self.tx)
            .await?;
        if let Some(expires_at) = expires_at {
            let waiting = RunPhase::Planned
                .apply(RunEvent::RequireApproval)
                .map_err(|e| sqlx::Error::Protocol(e.to_string()))?;
            sqlx::query(AWAIT_APPROVAL)
                .bind(*run.as_uuid())
                .bind(waiting.as_str())
                .bind(now)
                .bind(RunPhase::Planned.as_str())
                .execute(&mut *self.tx)
                .await?;
            sqlx::query(PARK_OPERATION)
                .bind(*operation.as_uuid())
                .bind(expires_at)
                .execute(&mut *self.tx)
                .await?;
        }
        Ok(approvals)
    }

    /// The release belongs to the target's application, the revision to the
    /// target and the plan to the organization.
    async fn owns_inputs(&mut self, req: &StartDeployment, application: Uuid) -> Result<bool, StoreError> {
        let org = self.org.to_string();
        let release: bool = sqlx::query_scalar(RELEASE_OF_APPLICATION)
            .bind(*req.release.as_uuid())
            .bind(&org)
            .bind(*req.project.as_uuid())
            .bind(application)
            .fetch_one(&mut *self.tx)
            .await?;
        let revision: bool = sqlx::query_scalar(REVISION_OF_TARGET)
            .bind(*req.config_revision.as_uuid())
            .bind(&org)
            .bind(*req.target.as_uuid())
            .fetch_one(&mut *self.tx)
            .await?;
        let plan = match req.render_plan {
            None => true,
            Some(plan) => {
                sqlx::query_scalar(PLAN_OF_ORG)
                    .bind(*plan.as_uuid())
                    .bind(&org)
                    .fetch_one(&mut *self.tx)
                    .await?
            }
        };
        Ok(release && revision && plan)
    }

    /// The phase and generation of `run`, or `None` when the organization has
    /// no such run.
    pub async fn run_phase(
        &mut self,
        run: DeploymentRunId,
    ) -> Result<Option<(RunPhase, Generation)>, StoreError> {
        let row: Option<(String, i64)> = sqlx::query_as(SELECT_RUN)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|(phase, generation)| Ok((parse_phase(&phase)?, Generation(counter(generation)?))))
            .transpose()
    }
}

/// Test support: set a run's phase and read its release, without the
/// materializer.
#[cfg(any(test, feature = "testing"))]
impl Tenant {
    pub async fn force_run_phase(&mut self, run: DeploymentRunId, phase: RunPhase) -> Result<(), StoreError> {
        sqlx::query("UPDATE deployment_runs SET phase = $2 WHERE id = $1 AND org_id = $3")
            .bind(*run.as_uuid())
            .bind(phase.as_str())
            .bind(self.org.to_string())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    pub async fn run_release(&mut self, run: DeploymentRunId) -> Result<Option<ReleaseId>, StoreError> {
        let id: Option<Uuid> =
            sqlx::query_scalar("SELECT release_id FROM deployment_runs WHERE id = $1 AND org_id = $2")
                .bind(*run.as_uuid())
                .bind(self.org.to_string())
                .fetch_optional(&mut *self.tx)
                .await?;
        Ok(id.map(ReleaseId::from_uuid))
    }
}

const LATEST_CONFIG_REVISION: &str = "SELECT id FROM target_config_revisions \
     WHERE target_id = $1 AND org_id = $2 ORDER BY revision DESC LIMIT 1";
const RUN_OF_TARGET: &str = "SELECT id, operation_id, generation, phase, approvals_required, \
     approval_expires_at, approval_plan_hash FROM deployment_runs \
     WHERE id = $1 AND target_id = $2 AND org_id = $3";
const RUN_OF_OPERATION: &str = "SELECT id, operation_id, generation, phase, approvals_required, \
     approval_expires_at, approval_plan_hash FROM deployment_runs \
     WHERE operation_id = $1 AND org_id = $2";

/// A deployment run as the API shows it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RunSummary {
    pub run: DeploymentRunId,
    pub operation: OperationId,
    pub generation: Generation,
    pub phase: RunPhase,
    /// Approvals the run needs before delivery (M4.1).
    pub approvals_required: u8,
    /// When a run waiting for approval is cancelled.
    pub approval_expires_at: Option<i64>,
    /// What approvers confirm they saw: sha-256 of the run's inputs.
    pub plan_hash: Option<[u8; 32]>,
}

#[derive(sqlx::FromRow)]
struct SummaryRow {
    id: Uuid,
    operation_id: Uuid,
    generation: i64,
    phase: String,
    approvals_required: i16,
    approval_expires_at: Option<i64>,
    approval_plan_hash: Option<Vec<u8>>,
}

fn run_summary(r: SummaryRow) -> Result<RunSummary, sqlx::Error> {
    let decode = |e: std::num::TryFromIntError| sqlx::Error::Decode(e.into());
    Ok(RunSummary {
        run: DeploymentRunId::from_uuid(r.id),
        operation: OperationId::from_uuid(r.operation_id),
        generation: Generation(counter(r.generation)?),
        phase: parse_phase(&r.phase)?,
        approvals_required: u8::try_from(r.approvals_required).map_err(decode)?,
        approval_expires_at: r.approval_expires_at,
        plan_hash: r
            .approval_plan_hash
            .map(|h| {
                <[u8; 32]>::try_from(h).map_err(|_| sqlx::Error::Decode("a plan hash is 32 bytes".into()))
            })
            .transpose()?,
    })
}

impl Tenant {
    /// The newest configuration revision of `target`, if it has one.
    pub async fn latest_config_revision(
        &mut self,
        target: TargetId,
    ) -> Result<Option<ConfigRevisionId>, StoreError> {
        let id: Option<Uuid> = sqlx::query_scalar(LATEST_CONFIG_REVISION)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(id.map(ConfigRevisionId::from_uuid))
    }

    /// Run `run` of `target`, or `None` when it is not one of its runs.
    pub async fn run_of_target(
        &mut self,
        target: TargetId,
        run: DeploymentRunId,
    ) -> Result<Option<RunSummary>, StoreError> {
        let row: Option<SummaryRow> = sqlx::query_as(RUN_OF_TARGET)
            .bind(*run.as_uuid())
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(run_summary).transpose()?)
    }

    /// The run an accepted deployment operation created, for a replayed
    /// request.
    pub async fn run_of_operation(
        &mut self,
        operation: OperationId,
    ) -> Result<Option<RunSummary>, StoreError> {
        let row: Option<SummaryRow> = sqlx::query_as(RUN_OF_OPERATION)
            .bind(*operation.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(run_summary).transpose()?)
    }
}

fn parse_phase(phase: &str) -> Result<RunPhase, sqlx::Error> {
    RunPhase::parse(phase).ok_or_else(|| sqlx::Error::Decode(format!("unknown run phase {phase:?}").into()))
}

impl Store {
    /// Apply `event` to `run` for the worker holding `claim` on its
    /// operation, by the rules of [`RunPhase::apply`].
    pub async fn advance_run(
        &self,
        claim: &Claim,
        run: DeploymentRunId,
        event: RunEvent,
    ) -> Result<Advance, StoreError> {
        let mut tx = self.pool().begin().await?;
        let phase: Option<String> = sqlx::query_scalar(LOCK_RUN_UNDER_FENCE)
            .bind(*run.as_uuid())
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .fetch_optional(&mut *tx)
            .await?;
        let Some(phase) = phase else {
            return Ok(Advance::Fenced);
        };
        let next = match parse_phase(&phase)?.apply(event) {
            Ok(next) => next,
            Err(illegal) => return Ok(Advance::Illegal(illegal)),
        };
        // Written once, apart from the phase: a failed deploy that a newer
        // run supersedes (it can no longer recover) stays a failed deploy.
        let (outcome, recovery_outcome) = outcomes(next);
        sqlx::query(SET_RUN_PHASE)
            .bind(*run.as_uuid())
            .bind(next.as_str())
            .bind(now_ms())
            .bind(outcome)
            .bind(recovery_outcome)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(Advance::Moved(next))
    }
}

/// What reaching `phase` records as the run's outcome and recovery outcome
/// (migration 0006 keeps both apart from the phase; each is written once).
const fn outcomes(phase: RunPhase) -> (Option<&'static str>, Option<&'static str>) {
    match phase {
        RunPhase::Succeeded => (Some("succeeded"), None),
        RunPhase::Failed => (Some("failed"), None),
        RunPhase::Cancelled => (Some("cancelled"), None),
        RunPhase::Recovered => (None, Some("recovered")),
        RunPhase::RecoveryFailed => (None, Some("recoveryFailed")),
        _ => (None, None),
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use kuben_core::ids::OrgId;

    use super::*;
    use crate::testing::{pg_store, skip};

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        application: ApplicationId,
        other_application: ApplicationId,
        target: TargetId,
        lifecycle_uid: Uuid,
    }

    async fn fixture(store: &Store) -> Fixture {
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, "shop-production")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let other_application = t.create_application(project, "api", "API").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
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
            application,
            other_application,
            target,
            lifecycle_uid,
        }
    }

    fn digest(n: u8) -> Digest {
        format!("sha256:{}", format!("{n:02x}").repeat(32))
            .parse()
            .expect("digest")
    }

    fn release(application: ApplicationId, log_level: &str) -> PortableRelease {
        PortableRelease {
            application,
            artifacts: BTreeMap::from([("web".to_owned(), digest(1))]),
            process_contract: json!({ "web": { "port": 8080 } }),
            portable_config: json!({ "LOG_LEVEL": log_level }),
            renderer_schema: 1,
            source: None,
            created_by: "user:alice".into(),
        }
    }

    fn audit() -> NewAudit {
        NewAudit {
            actor_kind: "user".into(),
            actor_id: Some("alice".into()),
            action: "deployment.accepted".into(),
            outcome: "accepted".into(),
            ..NewAudit::default()
        }
    }

    /// A release and configuration revision for the fixture's target.
    async fn inputs(store: &Store, f: &Fixture) -> (ReleaseId, ConfigRevisionId) {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let (release, _) = t
            .create_release(f.project, &release(f.application, "info"))
            .await
            .expect("release");
        let (revision, _) = t
            .create_config_revision(f.project, f.target, &json!({ "replicas": 2 }), "user:alice")
            .await
            .expect("revision")
            .expect("target");
        t.commit().await.expect("commit");
        (release, revision)
    }

    fn start(
        f: &Fixture,
        release: ReleaseId,
        revision: ConfigRevisionId,
        expected: u64,
        reason: RunReason,
    ) -> StartDeployment {
        StartDeployment {
            project: f.project,
            target: f.target,
            release,
            config_revision: revision,
            render_plan: None,
            expected_generation: Generation(expected),
            lifecycle_uid: f.lifecycle_uid,
            reason,
            requested_by: "user:alice".into(),
            input_hash: format!("{release}/{revision}/{expected}").into_bytes(),
        }
    }

    async fn run(store: &Store, org: OrgId, req: &StartDeployment, key: Option<&IdempotencyKey>) -> Started {
        let mut t = store.tenant(org).await.expect("tenant");
        let started = t.start_deployment(req, audit(), key).await.expect("start");
        t.commit().await.expect("commit");
        started
    }

    #[tokio::test]
    async fn releases_are_deduplicated_and_immutable() {
        let Some(store) = pg_store().await else {
            skip("releases");
            return;
        };
        let f = fixture(&store).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        let (first, created) = t
            .create_release(f.project, &release(f.application, "info"))
            .await
            .expect("release");
        assert!(created);
        assert_eq!(
            t.create_release(f.project, &release(f.application, "info"))
                .await
                .expect("again"),
            (first, false),
            "the same content is the same release"
        );
        let (other, created) = t
            .create_release(f.project, &release(f.application, "debug"))
            .await
            .expect("other");
        assert!(created && other != first);
        let (_, first_revision) = t
            .create_config_revision(f.project, f.target, &json!({}), "user:alice")
            .await
            .expect("revision")
            .expect("target");
        let (_, second_revision) = t
            .create_config_revision(f.project, f.target, &json!({}), "user:alice")
            .await
            .expect("revision")
            .expect("target");
        assert_eq!((first_revision, second_revision), (1, 2));
        let plan = t
            .freeze_render_plan("renderer/1", &json!({}), &json!([{ "kind": "Deployment" }]))
            .await
            .expect("plan");
        assert_eq!(
            t.freeze_render_plan("renderer/1", &json!({}), &json!([{ "kind": "Deployment" }]))
                .await
                .expect("plan again"),
            plan
        );
        t.commit().await.expect("commit");
        for table in ["releases", "target_config_revisions", "render_plans"] {
            // Safe to format: one of three fixed table names.
            let sql = format!("UPDATE {table} SET created_at = 0");
            assert!(
                sqlx::query(sqlx::AssertSqlSafe(sql))
                    .execute(store.pool())
                    .await
                    .is_err(),
                "{table} is append-only"
            );
        }
    }

    #[tokio::test]
    async fn deployments_raise_the_generation_and_supersede_older_runs() {
        let Some(store) = pg_store().await else {
            skip("deployment runs");
            return;
        };
        let f = fixture(&store).await;
        let (release_id, revision) = inputs(&store, &f).await;

        let Started::Accepted {
            run: first,
            generation,
            ..
        } = run(
            &store,
            f.org,
            &start(&f, release_id, revision, 0, RunReason::Deploy),
            None,
        )
        .await
        else {
            panic!("the first deploy is accepted");
        };
        assert_eq!(generation, Generation(1));
        assert_eq!(
            run(
                &store,
                f.org,
                &start(&f, release_id, revision, 0, RunReason::Deploy),
                None
            )
            .await,
            Started::Rejected(Reject::GenerationMoved {
                current: 1,
                expected: 0
            }),
            "a stale expected generation"
        );
        let Started::Accepted { generation, .. } = run(
            &store,
            f.org,
            &start(&f, release_id, revision, 1, RunReason::Deploy),
            None,
        )
        .await
        else {
            panic!("the second deploy is accepted");
        };
        assert_eq!(generation, Generation(2));

        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(
            t.run_phase(first).await.expect("read"),
            Some((RunPhase::Superseded, Generation(1))),
            "the newer run owns the target"
        );
        drop(t);

        let Started::Accepted { generation, .. } = run(
            &store,
            f.org,
            &start(&f, release_id, revision, 2, RunReason::Rollback),
            None,
        )
        .await
        else {
            panic!("the rollback is accepted");
        };
        assert_eq!(generation, Generation(3));
        let mut t = store.tenant(f.org).await.expect("tenant");
        let state = t.target_state(f.target).await.expect("read").expect("target");
        assert_eq!(state.desired_generation, Generation(3));
        assert_eq!(state.policy, DeployPolicy::Pinned, "a rollback pins the target");
        drop(t);

        let mut wrong_lifecycle = start(&f, release_id, revision, 3, RunReason::Deploy);
        wrong_lifecycle.lifecycle_uid = Uuid::now_v7();
        assert_eq!(
            run(&store, f.org, &wrong_lifecycle, None).await,
            Started::Rejected(Reject::LifecycleMismatch)
        );

        let mut t = store.tenant(f.org).await.expect("tenant");
        let (foreign, _) = t
            .create_release(f.project, &release(f.other_application, "info"))
            .await
            .expect("release");
        t.commit().await.expect("commit");
        assert_eq!(
            run(
                &store,
                f.org,
                &start(&f, foreign, revision, 3, RunReason::Deploy),
                None
            )
            .await,
            Started::NotFound,
            "a release of another application"
        );
    }

    #[tokio::test]
    async fn a_replayed_key_returns_the_first_receipt() {
        let Some(store) = pg_store().await else {
            skip("idempotent deployments");
            return;
        };
        let f = fixture(&store).await;
        let (release_id, revision) = inputs(&store, &f).await;
        let key = IdempotencyKey {
            actor: "user:alice".into(),
            key: "deploy-1".into(),
            ttl: Duration::from_hours(1),
        };
        let req = start(&f, release_id, revision, 0, RunReason::Deploy);
        let Started::Accepted { operation, .. } = run(&store, f.org, &req, Some(&key)).await else {
            panic!("accepted");
        };
        assert_eq!(
            run(&store, f.org, &req, Some(&key)).await,
            Started::Replayed(operation),
            "the same request again, although generation 0 is stale by now"
        );
        let other = start(&f, release_id, revision, 1, RunReason::Deploy);
        assert_eq!(
            run(&store, f.org, &other, Some(&key)).await,
            Started::KeyReused(operation)
        );
    }

    #[tokio::test]
    async fn run_phases_follow_the_state_machine_under_the_fence() {
        let Some(store) = pg_store().await else {
            skip("run phases");
            return;
        };
        let f = fixture(&store).await;
        let (release_id, revision) = inputs(&store, &f).await;
        let Started::Accepted { run: run_id, .. } = run(
            &store,
            f.org,
            &start(&f, release_id, revision, 0, RunReason::Deploy),
            None,
        )
        .await
        else {
            panic!("accepted");
        };
        let slow = store
            .claim_operation("slow", &[RUN_KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("due");
        assert_eq!(
            store
                .advance_run(&slow, run_id, RunEvent::ReadyForDelivery)
                .await
                .expect("advance"),
            Advance::Moved(RunPhase::PendingDelivery)
        );
        assert!(matches!(
            store
                .advance_run(&slow, run_id, RunEvent::Verified)
                .await
                .expect("advance"),
            Advance::Illegal(_)
        ));

        sqlx::query("UPDATE operations SET lease_until = 0 WHERE id = $1")
            .bind(*slow.id.as_uuid())
            .execute(store.pool())
            .await
            .expect("expire the lease");
        let fast = store
            .claim_operation("fast", &[RUN_KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("taken over");
        assert_eq!(
            store
                .advance_run(&slow, run_id, RunEvent::AcceptedByCluster)
                .await
                .expect("advance"),
            Advance::Fenced
        );
        assert_eq!(
            store
                .advance_run(&fast, run_id, RunEvent::AcceptedByCluster)
                .await
                .expect("advance"),
            Advance::Moved(RunPhase::AcceptedByCluster)
        );
    }
}

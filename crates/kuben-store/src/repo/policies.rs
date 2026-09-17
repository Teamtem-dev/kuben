//! Environment policies and deployment approvals (M4.1, migration 0019).
//!
//! A decision locks its run, applies [`kuben_core::policy::decide`] and, in
//! the same transaction, records the decision and moves the run: enough
//! approvals release it to delivery, one rejection cancels it. Either way
//! its operation is woken, so the materializer acts at once.

use kuben_core::{
    ids::{DeploymentRunId, EnvironmentId, ProjectId, TargetId},
    ops::{RunEvent, RunPhase},
    perm::Role,
    policy::{ApprovalError, Decision, EnvironmentPolicy, Pending, Tally, decide},
    scan::{GateMode, ScanGate, Severity},
    time::now_ms,
};
use uuid::Uuid;

use super::{Tenant, product::counter};
use crate::StoreError;

const POLICY_COLUMNS: &str = "revision, required_approvals, deploy_role, approve_role, approval_ttl_secs, \
     created_by, created_at, scan_mode, scan_severity, scan_require, scan_max_age_secs";
const LOCK_ENVIRONMENT: &str = "SELECT id FROM environments \
     WHERE id = $1 AND org_id = $2 AND project_id = $3 FOR UPDATE";
const NEXT_REVISION: &str = "SELECT COALESCE(max(revision), 0) + 1 FROM environment_policies \
     WHERE environment_id = $1 AND org_id = $2";
const INSERT_POLICY: &str = "INSERT INTO environment_policies \
     (org_id, project_id, environment_id, revision, required_approvals, deploy_role, approve_role, \
      approval_ttl_secs, created_by, created_at, scan_mode, scan_severity, scan_require, scan_max_age_secs) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)";
const LOCK_RUN: &str = "SELECT phase, requested_by, approvals_required, approval_expires_at, \
     approval_plan_hash, operation_id FROM deployment_runs \
     WHERE id = $1 AND target_id = $2 AND org_id = $3 FOR UPDATE";
const RUN_APPROVAL: &str = "SELECT phase, requested_by, approvals_required, approval_expires_at, \
     approval_plan_hash, operation_id FROM deployment_runs \
     WHERE id = $1 AND target_id = $2 AND org_id = $3";
const DECISIONS: &str = "SELECT approver, decision, comment, decided_at FROM run_approvals \
     WHERE run_id = $1 AND org_id = $2 ORDER BY decided_at, approver";
const INSERT_DECISION: &str = "INSERT INTO run_approvals \
     (run_id, org_id, approver, decision, plan_hash, comment, decided_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7)";
const SET_PHASE: &str = "UPDATE deployment_runs SET phase = $2, updated_at = $3, \
     outcome = COALESCE(outcome, $4) WHERE id = $1";
const WAKE: &str = "UPDATE operations SET next_attempt_at = kuben_now_ms() WHERE id = $1 AND NOT done";

/// One revision of an environment's policy.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PolicyRevision {
    pub revision: u64,
    pub policy: EnvironmentPolicy,
    pub created_by: String,
    pub created_at: i64,
}

#[derive(sqlx::FromRow)]
struct PolicyRow {
    revision: i64,
    required_approvals: i16,
    deploy_role: String,
    approve_role: String,
    approval_ttl_secs: i32,
    created_by: String,
    created_at: i64,
    scan_mode: String,
    scan_severity: String,
    scan_require: bool,
    scan_max_age_secs: i32,
}

fn role(value: &str) -> Result<Role, sqlx::Error> {
    value
        .parse()
        .map_err(|e: kuben_core::Error| sqlx::Error::Decode(e.to_string().into()))
}

fn decode<E: std::error::Error + Send + Sync + 'static>(e: E) -> sqlx::Error {
    sqlx::Error::Decode(Box::new(e))
}

impl TryFrom<PolicyRow> for PolicyRevision {
    type Error = sqlx::Error;

    fn try_from(r: PolicyRow) -> Result<Self, Self::Error> {
        Ok(Self {
            revision: counter(r.revision)?,
            policy: EnvironmentPolicy {
                required_approvals: u8::try_from(r.required_approvals).map_err(decode)?,
                deploy_role: role(&r.deploy_role)?,
                approve_role: role(&r.approve_role)?,
                approval_ttl_secs: u32::try_from(r.approval_ttl_secs).map_err(decode)?,
                scan: ScanGate {
                    mode: GateMode::parse(&r.scan_mode).ok_or_else(|| {
                        sqlx::Error::Decode(format!("unknown scan mode {:?}", r.scan_mode).into())
                    })?,
                    severity: Severity::parse(&r.scan_severity).ok_or_else(|| {
                        sqlx::Error::Decode(format!("unknown severity {:?}", r.scan_severity).into())
                    })?,
                    require_scan: r.scan_require,
                    max_age_secs: u32::try_from(r.scan_max_age_secs).map_err(decode)?,
                },
            },
            created_by: r.created_by,
            created_at: r.created_at,
        })
    }
}

/// A recorded approval decision.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ApprovalRecord {
    /// The user who decided.
    pub approver: String,
    pub decision: Decision,
    pub comment: Option<String>,
    pub decided_at: i64,
}

/// A run's approval state, as the API shows it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RunApproval {
    pub phase: RunPhase,
    pub requested_by: String,
    pub required: u8,
    pub expires_at: Option<i64>,
    pub plan_hash: Option<Vec<u8>>,
    pub decisions: Vec<ApprovalRecord>,
}

impl RunApproval {
    /// Distinct approvals recorded.
    #[must_use]
    pub fn approved(&self) -> u8 {
        let n = self
            .decisions
            .iter()
            .filter(|d| d.decision == Decision::Approve)
            .count();
        u8::try_from(n).unwrap_or(u8::MAX)
    }
}

/// The outcome of [`Tenant::decide_run`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Decided {
    /// Recorded; the run moved or waits for more approvals.
    Recorded(Tally),
    /// Nothing was recorded.
    Refused(ApprovalError),
    /// No such run of the target.
    NotFound,
}

#[derive(sqlx::FromRow)]
struct RunRow {
    phase: String,
    requested_by: String,
    approvals_required: i16,
    approval_expires_at: Option<i64>,
    approval_plan_hash: Option<Vec<u8>>,
    operation_id: Uuid,
}

#[derive(sqlx::FromRow)]
struct DecisionRow {
    approver: String,
    decision: String,
    comment: Option<String>,
    decided_at: i64,
}

fn phase_of(value: &str) -> Result<RunPhase, sqlx::Error> {
    RunPhase::parse(value).ok_or_else(|| sqlx::Error::Decode(format!("unknown run phase {value:?}").into()))
}

impl TryFrom<DecisionRow> for ApprovalRecord {
    type Error = sqlx::Error;

    fn try_from(r: DecisionRow) -> Result<Self, Self::Error> {
        let decision = match r.decision.as_str() {
            "approved" => Decision::Approve,
            "rejected" => Decision::Reject,
            other => return Err(sqlx::Error::Decode(format!("unknown decision {other:?}").into())),
        };
        Ok(Self {
            approver: r.approver,
            decision,
            comment: r.comment,
            decided_at: r.decided_at,
        })
    }
}

impl Tenant {
    /// The newest policy revision of `environment`, if it has one.
    pub async fn environment_policy(
        &mut self,
        environment: EnvironmentId,
    ) -> Result<Option<PolicyRevision>, StoreError> {
        let sql = format!(
            "SELECT {POLICY_COLUMNS} FROM environment_policies \
             WHERE environment_id = $1 AND org_id = $2 ORDER BY revision DESC LIMIT 1"
        );
        let row: Option<PolicyRow> = sqlx::query_as(sqlx::AssertSqlSafe(sql))
            .bind(*environment.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(PolicyRevision::try_from).transpose()?)
    }

    /// The newest policy revision of the environment `target` is placed in.
    pub async fn policy_of_target(&mut self, target: TargetId) -> Result<Option<PolicyRevision>, StoreError> {
        let sql = format!(
            "SELECT p.{} FROM application_targets t \
             JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
             JOIN environment_policies p ON p.environment_id = pl.environment_id AND p.org_id = t.org_id \
             WHERE t.id = $1 AND t.org_id = $2 ORDER BY p.revision DESC LIMIT 1",
            POLICY_COLUMNS.replace(", ", ", p.")
        );
        let row: Option<PolicyRow> = sqlx::query_as(sqlx::AssertSqlSafe(sql))
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(row.map(PolicyRevision::try_from).transpose()?)
    }

    /// Record `policy` as the newest revision of `environment`. `None` when
    /// the project has no such environment. The caller validates the policy
    /// and decides who may change it.
    pub async fn set_environment_policy(
        &mut self,
        project: ProjectId,
        environment: EnvironmentId,
        policy: &EnvironmentPolicy,
        created_by: &str,
    ) -> Result<Option<u64>, StoreError> {
        let org = self.org.to_string();
        let locked: Option<Uuid> = sqlx::query_scalar(LOCK_ENVIRONMENT)
            .bind(*environment.as_uuid())
            .bind(&org)
            .bind(*project.as_uuid())
            .fetch_optional(&mut *self.tx)
            .await?;
        if locked.is_none() {
            return Ok(None);
        }
        let revision: i64 = sqlx::query_scalar(NEXT_REVISION)
            .bind(*environment.as_uuid())
            .bind(&org)
            .fetch_one(&mut *self.tx)
            .await?;
        sqlx::query(INSERT_POLICY)
            .bind(&org)
            .bind(*project.as_uuid())
            .bind(*environment.as_uuid())
            .bind(revision)
            .bind(i16::from(policy.required_approvals))
            .bind(policy.deploy_role.to_string())
            .bind(policy.approve_role.to_string())
            .bind(i32::try_from(policy.approval_ttl_secs).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(created_by)
            .bind(now_ms())
            .bind(policy.scan.mode.as_str())
            .bind(policy.scan.severity.as_str())
            .bind(policy.scan.require_scan)
            .bind(i32::try_from(policy.scan.max_age_secs).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .execute(&mut *self.tx)
            .await?;
        Ok(Some(counter(revision)?))
    }

    async fn decisions(&mut self, run: DeploymentRunId) -> Result<Vec<ApprovalRecord>, StoreError> {
        let rows: Vec<DecisionRow> = sqlx::query_as(DECISIONS)
            .bind(*run.as_uuid())
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        Ok(rows
            .into_iter()
            .map(ApprovalRecord::try_from)
            .collect::<Result<_, _>>()?)
    }

    /// The approval state of `run` of `target`.
    pub async fn run_approval(
        &mut self,
        target: TargetId,
        run: DeploymentRunId,
    ) -> Result<Option<RunApproval>, StoreError> {
        let row: Option<RunRow> = sqlx::query_as(RUN_APPROVAL)
            .bind(*run.as_uuid())
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        Ok(Some(RunApproval {
            phase: phase_of(&row.phase)?,
            requested_by: row.requested_by,
            required: u8::try_from(row.approvals_required).map_err(decode)?,
            expires_at: row.approval_expires_at,
            plan_hash: row.approval_plan_hash,
            decisions: self.decisions(run).await?,
        }))
    }

    /// Record `approver`'s `decision` on `run` of `target`, having seen
    /// `plan_hash`, and move the run when the decision settles it.
    pub async fn decide_run(
        &mut self,
        target: TargetId,
        run: DeploymentRunId,
        approver: &str,
        decision: Decision,
        plan_hash: &[u8],
        comment: Option<&str>,
    ) -> Result<Decided, StoreError> {
        let org = self.org.to_string();
        let row: Option<RunRow> = sqlx::query_as(LOCK_RUN)
            .bind(*run.as_uuid())
            .bind(*target.as_uuid())
            .bind(&org)
            .fetch_optional(&mut *self.tx)
            .await?;
        let Some(row) = row else {
            return Ok(Decided::NotFound);
        };
        let phase = phase_of(&row.phase)?;
        let (Some(expires_at), Some(expected), RunPhase::AwaitingApproval) =
            (row.approval_expires_at, row.approval_plan_hash.as_deref(), phase)
        else {
            return Ok(Decided::Refused(ApprovalError::NotAwaiting));
        };
        let decisions = self.decisions(run).await?;
        let approvals = decisions
            .iter()
            .filter(|d| d.decision == Decision::Approve)
            .count();
        let pending = Pending {
            requested_by: &row.requested_by,
            required: u8::try_from(row.approvals_required).map_err(decode)?,
            approved: u8::try_from(approvals).unwrap_or(u8::MAX),
            expires_at,
            plan_hash: expected,
        };
        let decided_before = decisions.iter().any(|d| d.approver == approver);
        let now = now_ms();
        let tally = match decide(&pending, approver, decided_before, decision, plan_hash, now) {
            Ok(tally) => tally,
            Err(refused) => return Ok(Decided::Refused(refused)),
        };
        sqlx::query(INSERT_DECISION)
            .bind(*run.as_uuid())
            .bind(&org)
            .bind(approver)
            .bind(decision.as_str())
            .bind(plan_hash)
            .bind(comment)
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        let (event, outcome) = match tally {
            Tally::Waiting { .. } => return Ok(Decided::Recorded(tally)),
            Tally::Approved => (RunEvent::Approved, None),
            Tally::Rejected => (RunEvent::Rejected, Some("cancelled")),
        };
        let next = phase
            .apply(event)
            .map_err(|e| sqlx::Error::Protocol(e.to_string()))?;
        sqlx::query(SET_PHASE)
            .bind(*run.as_uuid())
            .bind(next.as_str())
            .bind(now)
            .bind(outcome)
            .execute(&mut *self.tx)
            .await?;
        sqlx::query(WAKE)
            .bind(row.operation_id)
            .execute(&mut *self.tx)
            .await?;
        Ok(Decided::Recorded(tally))
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{ApplicationId, ConfigRevisionId, OperationId, OrgId, ReleaseId, UserId},
        model::ScopeKind,
        ops::Generation,
    };
    use serde_json::json;

    use super::*;
    use crate::{
        Store,
        repo::{NewAudit, PortableRelease, RUN_KIND, RunReason, StartDeployment, Started},
        testing::{pg_store, skip},
    };

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    const ALICE: &str = "0192f3a1-0000-7000-8000-00000000000a";
    const BOB: &str = "0192f3a1-0000-7000-8000-00000000000b";
    const CAROL: &str = "0192f3a1-0000-7000-8000-00000000000c";

    struct Fixture {
        org: OrgId,
        project: ProjectId,
        environment: EnvironmentId,
        application: ApplicationId,
        target: TargetId,
        release: ReleaseId,
        revision: ConfigRevisionId,
    }

    async fn fixture(store: &Store, slug: &str, policy: Option<EnvironmentPolicy>) -> Fixture {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        if let Some(policy) = policy {
            t.set_environment_policy(project, environment, &policy, "user:admin")
                .await
                .expect("policy")
                .expect("environment");
        }
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, &format!("{slug}-shop"))
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        let (revision, _) = t
            .create_config_revision(project, target, &json!({}), "user:admin")
            .await
            .expect("revision")
            .expect("target");
        let (release, _) = t
            .create_release(
                project,
                &PortableRelease {
                    application,
                    artifacts: [("web".to_owned(), DIGEST.parse().expect("digest"))].into(),
                    process_contract: json!({}),
                    portable_config: json!({}),
                    renderer_schema: 1,
                    source: None,
                    created_by: "user:admin".into(),
                },
            )
            .await
            .expect("release");
        t.commit().await.expect("commit");
        Fixture {
            org,
            project,
            environment,
            application,
            target,
            release,
            revision,
        }
    }

    /// Start a run of `reason` as `alice`, expecting generation `expected`.
    async fn start(store: &Store, f: &Fixture, expected: u64, reason: RunReason) -> (DeploymentRunId, u8) {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let lifecycle_uid = t
            .target_state(f.target)
            .await
            .expect("read")
            .expect("target")
            .lifecycle_uid;
        let req = StartDeployment {
            project: f.project,
            target: f.target,
            release: f.release,
            config_revision: f.revision,
            render_plan: None,
            expected_generation: Generation(expected),
            lifecycle_uid,
            reason,
            requested_by: format!("user:{ALICE}"),
            input_hash: format!("{expected}").into_bytes(),
        };
        let started = t
            .start_deployment(&req, NewAudit::default(), None)
            .await
            .expect("start");
        let Started::Accepted {
            run,
            approvals_required,
            ..
        } = started
        else {
            panic!("not accepted: {started:?}");
        };
        t.commit().await.expect("commit");
        (run, approvals_required)
    }

    async fn state(store: &Store, f: &Fixture, run: DeploymentRunId) -> RunApproval {
        let mut t = store.tenant(f.org).await.expect("tenant");
        t.run_approval(f.target, run).await.expect("read").expect("run")
    }

    async fn vote(
        store: &Store,
        f: &Fixture,
        run: DeploymentRunId,
        who: &str,
        decision: Decision,
        hash: &[u8],
    ) -> Decided {
        let mut t = store.tenant(f.org).await.expect("tenant");
        let decided = t
            .decide_run(f.target, run, who, decision, hash, Some("looks good"))
            .await
            .expect("decide");
        t.commit().await.expect("commit");
        decided
    }

    async fn claimable(store: &Store) -> bool {
        store
            .claim_operation("w", &[RUN_KIND], std::time::Duration::from_secs(30))
            .await
            .expect("claim")
            .is_some()
    }

    #[tokio::test]
    async fn policies_are_revisions_seen_only_by_their_organization() {
        let Some(store) = pg_store().await else {
            return skip("policies_are_revisions_seen_only_by_their_organization");
        };
        let f = fixture(&store, "a", None).await;
        let mut t = store.tenant(f.org).await.expect("tenant");
        assert_eq!(t.environment_policy(f.environment).await.expect("read"), None);
        let strict = EnvironmentPolicy {
            required_approvals: 2,
            ..EnvironmentPolicy::production()
        };
        for (policy, revision) in [(EnvironmentPolicy::production(), 1), (strict, 2)] {
            let set = t
                .set_environment_policy(f.project, f.environment, &policy, "user:admin")
                .await
                .expect("set");
            assert_eq!(set, Some(revision));
        }
        let newest = t
            .environment_policy(f.environment)
            .await
            .expect("read")
            .expect("policy");
        assert_eq!((newest.revision, newest.policy), (2, strict));
        assert_eq!(
            t.policy_of_target(f.target)
                .await
                .expect("read")
                .map(|p| p.revision),
            Some(2)
        );
        let missing = t
            .set_environment_policy(f.project, EnvironmentId::new(), &strict, "user:admin")
            .await
            .expect("set");
        assert_eq!(missing, None);
        t.commit().await.expect("commit");

        let other = store.create_org("b", "B").await.expect("org").id;
        let mut t = store.tenant(other).await.expect("tenant");
        assert_eq!(
            t.environment_policy(f.environment).await.expect("read"),
            None,
            "RLS"
        );
        let foreign = t
            .set_environment_policy(f.project, f.environment, &EnvironmentPolicy::open(), "user:evil")
            .await
            .expect("set");
        assert_eq!(foreign, None, "another organization's environment is not found");
    }

    #[tokio::test]
    async fn a_protected_deploy_waits_for_another_person() {
        let Some(store) = pg_store().await else {
            return skip("a_protected_deploy_waits_for_another_person");
        };
        let f = fixture(&store, "a", Some(EnvironmentPolicy::production())).await;
        let (run, required) = start(&store, &f, 0, RunReason::Deploy).await;
        assert_eq!(required, 1);
        let waiting = state(&store, &f, run).await;
        assert_eq!(waiting.phase, RunPhase::AwaitingApproval);
        assert!(waiting.expires_at.is_some_and(|at| at > now_ms()));
        let hash = waiting.plan_hash.clone().expect("plan hash");
        assert_eq!(hash.len(), 32);
        assert!(!claimable(&store).await, "parked until a decision");

        assert_eq!(
            vote(&store, &f, run, ALICE, Decision::Approve, &hash).await,
            Decided::Refused(ApprovalError::SelfApproval)
        );
        assert_eq!(
            vote(&store, &f, run, BOB, Decision::Approve, b"stale").await,
            Decided::Refused(ApprovalError::StalePlan)
        );
        assert!(
            state(&store, &f, run).await.decisions.is_empty(),
            "refusals record nothing"
        );
        assert_eq!(
            vote(&store, &f, run, BOB, Decision::Approve, &hash).await,
            Decided::Recorded(Tally::Approved)
        );
        let approved = state(&store, &f, run).await;
        assert_eq!(approved.phase, RunPhase::PendingDelivery);
        assert_eq!(approved.approved(), 1);
        assert_eq!(approved.decisions[0].comment.as_deref(), Some("looks good"));
        assert!(claimable(&store).await, "woken by the approval");
        assert_eq!(
            vote(&store, &f, run, CAROL, Decision::Approve, &hash).await,
            Decided::Refused(ApprovalError::NotAwaiting)
        );
        assert_eq!(
            vote(&store, &f, DeploymentRunId::new(), BOB, Decision::Approve, &hash).await,
            Decided::NotFound
        );
    }

    #[tokio::test]
    async fn one_rejection_cancels_and_two_approvals_need_two_people() {
        let Some(store) = pg_store().await else {
            return skip("one_rejection_cancels_and_two_approvals_need_two_people");
        };
        let two = EnvironmentPolicy {
            required_approvals: 2,
            ..EnvironmentPolicy::production()
        };
        let f = fixture(&store, "a", Some(two)).await;
        let (first, _) = start(&store, &f, 0, RunReason::Deploy).await;
        let hash = state(&store, &f, first).await.plan_hash.expect("hash");
        assert_eq!(
            vote(&store, &f, first, BOB, Decision::Approve, &hash).await,
            Decided::Recorded(Tally::Waiting { remaining: 1 })
        );
        assert_eq!(
            vote(&store, &f, first, BOB, Decision::Approve, &hash).await,
            Decided::Refused(ApprovalError::AlreadyDecided)
        );
        assert_eq!(state(&store, &f, first).await.phase, RunPhase::AwaitingApproval);
        assert_eq!(
            vote(&store, &f, first, CAROL, Decision::Approve, &hash).await,
            Decided::Recorded(Tally::Approved)
        );

        let (second, _) = start(&store, &f, 1, RunReason::Rollback).await;
        let hash = state(&store, &f, second).await.plan_hash.expect("hash");
        assert_eq!(
            vote(&store, &f, second, CAROL, Decision::Reject, &hash).await,
            Decided::Recorded(Tally::Rejected)
        );
        let rejected = state(&store, &f, second).await;
        assert_eq!(rejected.phase, RunPhase::Cancelled);
        let mut t = store.tenant(f.org).await.expect("tenant");
        let outcome: Option<String> = sqlx::query_scalar("SELECT outcome FROM deployment_runs WHERE id = $1")
            .bind(*second.as_uuid())
            .fetch_one(&mut *t.tx)
            .await
            .expect("outcome");
        assert_eq!(outcome.as_deref(), Some("cancelled"));
    }

    #[tokio::test]
    async fn restarts_and_open_environments_need_no_approval() {
        let Some(store) = pg_store().await else {
            return skip("restarts_and_open_environments_need_no_approval");
        };
        let open = fixture(&store, "a", Some(EnvironmentPolicy::open())).await;
        let (run, required) = start(&store, &open, 0, RunReason::Deploy).await;
        assert_eq!(required, 0);
        let s = state(&store, &open, run).await;
        assert_eq!(
            (s.phase, s.plan_hash, s.expires_at),
            (RunPhase::Planned, None, None)
        );

        let unset = fixture(&store, "b", None).await;
        assert_eq!(
            start(&store, &unset, 0, RunReason::Deploy).await.1,
            0,
            "no policy row"
        );

        let protected = fixture(&store, "c", Some(EnvironmentPolicy::production())).await;
        let (restart, required) = start(&store, &protected, 0, RunReason::Restart).await;
        assert_eq!(required, 0);
        assert_eq!(state(&store, &protected, restart).await.phase, RunPhase::Planned);
    }

    #[tokio::test]
    async fn a_newer_run_supersedes_a_waiting_one() {
        let Some(store) = pg_store().await else {
            return skip("a_newer_run_supersedes_a_waiting_one");
        };
        let f = fixture(&store, "a", Some(EnvironmentPolicy::production())).await;
        let (older, _) = start(&store, &f, 0, RunReason::Deploy).await;
        let hash = state(&store, &f, older).await.plan_hash.expect("hash");
        let (newer, _) = start(&store, &f, 1, RunReason::Deploy).await;
        assert_eq!(state(&store, &f, older).await.phase, RunPhase::Superseded);
        assert!(claimable(&store).await, "the superseded run is woken to settle");
        assert_eq!(
            vote(&store, &f, older, BOB, Decision::Approve, &hash).await,
            Decided::Refused(ApprovalError::NotAwaiting)
        );
        let newer_hash = state(&store, &f, newer).await.plan_hash.expect("hash");
        assert_ne!(newer_hash, hash, "every run is approved for itself");
        assert_eq!(
            vote(&store, &f, newer, BOB, Decision::Approve, &hash).await,
            Decided::Refused(ApprovalError::StalePlan),
            "an approval of the older run does not carry over"
        );
    }

    #[tokio::test]
    async fn approval_inputs_and_decisions_never_change() {
        let Some(store) = pg_store().await else {
            return skip("approval_inputs_and_decisions_never_change");
        };
        let f = fixture(&store, "a", Some(EnvironmentPolicy::production())).await;
        let (run, _) = start(&store, &f, 0, RunReason::Deploy).await;
        let hash = state(&store, &f, run).await.plan_hash.expect("hash");
        vote(&store, &f, run, BOB, Decision::Approve, &hash).await;
        for sql in [
            "UPDATE deployment_runs SET approvals_required = 0 WHERE id = $1",
            "UPDATE deployment_runs SET approval_expires_at = approval_expires_at + 1 WHERE id = $1",
            "UPDATE run_approvals SET decision = 'rejected' WHERE run_id = $1",
            "DELETE FROM run_approvals WHERE run_id = $1",
        ] {
            let mut t = store.tenant(f.org).await.expect("tenant");
            let changed = sqlx::query(sql).bind(*run.as_uuid()).execute(&mut *t.tx).await;
            assert!(changed.is_err(), "{sql}");
        }
        let mut t = store.tenant(f.org).await.expect("tenant");
        let changed = sqlx::query("UPDATE environment_policies SET required_approvals = 0")
            .execute(&mut *t.tx)
            .await;
        assert!(changed.is_err(), "policy revisions are append-only");
    }

    #[tokio::test]
    async fn scoped_roles_are_one_per_node_and_only_for_members() {
        let Some(store) = pg_store().await else {
            return skip("scoped_roles_are_one_per_node_and_only_for_members");
        };
        let f = fixture(&store, "a", None).await;
        let member = store
            .create_user("dev@example.com", None, None)
            .await
            .expect("user")
            .id;
        let outsider = store
            .create_user("out@example.com", None, None)
            .await
            .expect("user")
            .id;
        store.add_membership(f.org, member).await.expect("member");
        store
            .bind_org_role(f.org, member, Role::Viewer)
            .await
            .expect("org role");
        let node = *f.project.as_uuid();

        assert!(
            !store
                .bind_scoped_role(f.org, outsider, ScopeKind::Project, node, Role::Admin)
                .await
                .expect("bind"),
            "no membership, no role"
        );
        for role in [Role::Developer, Role::Admin] {
            assert!(
                store
                    .bind_scoped_role(f.org, member, ScopeKind::Project, node, role)
                    .await
                    .expect("bind")
            );
        }
        let scoped = store
            .scoped_members(f.org, ScopeKind::Project, node)
            .await
            .expect("list");
        assert_eq!(scoped.len(), 1, "updated in place");
        assert_eq!(scoped[0].role, Role::Admin);
        let bindings = store.bindings_for_user(member).await.expect("bindings");
        assert_eq!(bindings.len(), 2);
        assert!(bindings.iter().any(
            |b| b.scope_kind == ScopeKind::Project && b.scope_uid.as_deref() == Some(&*node.to_string())
        ));
        assert!(
            store
                .bind_scoped_role(f.org, member, ScopeKind::Org, node, Role::Owner)
                .await
                .is_err(),
            "org roles are not scoped"
        );

        assert!(
            store
                .unbind_scoped_role(f.org, member, ScopeKind::Project, node)
                .await
                .expect("unbind")
        );
        assert!(
            !store
                .unbind_scoped_role(f.org, member, ScopeKind::Project, node)
                .await
                .expect("unbind")
        );
        assert_eq!(store.bindings_for_user(member).await.expect("bindings").len(), 1);
    }

    #[tokio::test]
    async fn removing_a_member_revokes_their_tokens_with_the_membership() {
        let Some(store) = pg_store().await else {
            return skip("removing_a_member_revokes_their_tokens_with_the_membership");
        };
        let f = fixture(&store, "a", None).await;
        let member = store
            .create_user("dev@example.com", None, None)
            .await
            .expect("user")
            .id;
        store.add_membership(f.org, member).await.expect("member");
        store
            .bind_org_role(f.org, member, Role::Developer)
            .await
            .expect("role");
        store
            .bind_scoped_role(
                f.org,
                member,
                ScopeKind::Environment,
                *f.environment.as_uuid(),
                Role::Admin,
            )
            .await
            .expect("scoped");
        let token = store
            .create_token(crate::repo::NewToken {
                id: kuben_core::ids::TokenId::new(),
                org_id: f.org,
                owner: member,
                name: "ci".into(),
                prefix: "kbn_pat_test".into(),
                secret_hash: b"hash".to_vec(),
                scope: kuben_core::model::TokenScope {
                    role: Role::Developer,
                    project: None,
                    environment: None,
                },
                expires_at: None,
            })
            .await
            .expect("token");
        store.remove_member(f.org, member).await.expect("remove");
        assert!(
            store
                .bindings_for_user(member)
                .await
                .expect("bindings")
                .is_empty()
        );
        let revoked = store.find_token(token.id).await.expect("read").expect("token");
        assert!(revoked.revoked_at.is_some());
        let _ = (f.application, OperationId::new(), UserId::new());
    }
}

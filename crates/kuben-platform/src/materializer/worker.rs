//! The materializer's worker (ADR-032): claims `deployment` operations and
//! carries each run through delivery and verification, and claims the
//! lifecycle operations of [`super::lifecycle`].
//!
//! Delivery renders the run's objects, freezes the run's RenderPlan before
//! the first write (never again on a retry), and writes them, the App under the
//! generation fence, then records the write and moves the run to
//! `acceptedByCluster`. Verification follows the App controller's status
//! ([`progress`]) to `succeeded` or `failed`. Claims are fenced in SQL, so
//! any number of replicas may run workers; none needs the leader lease.

use std::{convert::Infallible, fmt, fmt::Debug, time::Duration};

use k8s_openapi::api::core::v1::Namespace;
use kube::{Api, Client, Resource, api::ListParams};
use kuben_core::{
    ids::{OperationId, OrgId},
    ops::{Generation, RunEvent, RunPhase},
};
use kuben_crd::{App, Environment, KubenConfig, Project};
use kuben_store::{
    Store, StoreError,
    repo::{
        Advance, Claim, ENVIRONMENT_APPLY, ENVIRONMENT_DELETE, Materialization, PROJECT_APPLY,
        PROJECT_DELETE, RUN_KIND, TARGET_DELETE,
    },
};
use serde::{Serialize, de::DeserializeOwned};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use super::{
    fence::{Fence, fence},
    progress::{Progress, progress},
    render::{self, render},
    write::{self, MAX_CONFLICTS},
};
use crate::{
    controller::{backoff, platform_of},
    discovery::{self, Facts},
    health::Health,
    render::{self as plans, Capabilities, RenderPlanError},
};

/// Health registry name of the worker loop.
const HEALTH: &str = "materializer";
pub(super) const LEASE: Duration = Duration::from_mins(2);
/// Pause between claims when nothing is due.
const IDLE: Duration = Duration::from_secs(2);
/// Pause between reads while waiting on the cluster.
const POLL: Duration = Duration::from_secs(3);
/// How long delivery waits for the environment controller's namespace.
const NAMESPACE_WAIT: Duration = Duration::from_mins(1);
const VERIFY_DEADLINE: Duration = Duration::from_mins(15);
/// Longest sleep of a run waiting for approval.
const APPROVAL_RECHECK: Duration = Duration::from_hours(1);
/// Longest sleep of a run of a paused target; resuming wakes it at once.
const PAUSE_RECHECK: Duration = Duration::from_mins(5);
/// How long an environment deletion waits before it checks again.
const DELETION_CHECK: Duration = Duration::from_secs(20);
/// Claims of one deployment operation before its run fails for good.
/// Lifecycle operations are idempotent and never give up.
const MAX_ATTEMPTS: i32 = 20;
/// Every operation kind a worker claims.
const KINDS: [&str; 6] = [
    RUN_KIND,
    PROJECT_APPLY,
    ENVIRONMENT_APPLY,
    TARGET_DELETE,
    ENVIRONMENT_DELETE,
    PROJECT_DELETE,
];

/// A failure a later attempt may not repeat.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("kubernetes API: {0}")]
    Kube(#[from] kube::Error),
    #[error("store: {0}")]
    Store(#[from] StoreError),
    #[error("`{0}` kept changing under concurrent writes")]
    Contended(String),
    #[error("namespace `{0}` does not exist yet")]
    NamespacePending(String),
    #[error("the API server returned `{0}` without a UID or generation")]
    Incomplete(String),
    #[error("the run cannot take the next step: {0}")]
    Illegal(String),
    #[error("runs in phase `{0}` wait for a later milestone (approval, blocking, cancellation, recovery)")]
    Unsupported(&'static str),
}

impl Error {
    /// Error code recorded on the operation while it waits for a retry.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::Kube(_) => "KubernetesError",
            Self::Store(_) => "StoreError",
            Self::Contended(_) => "Contended",
            Self::NamespacePending(_) => "NamespacePending",
            Self::Incomplete(_) => "IncompleteObject",
            Self::Illegal(_) => "IllegalTransition",
            Self::Unsupported(_) => "UnsupportedPhase",
        }
    }
}

/// Why the work on a claim stopped.
#[derive(Debug)]
pub(super) enum Stop {
    /// Another worker holds the operation now: drop everything.
    Fenced,
    /// Shutting down: the lease runs out and another worker resumes.
    Shutdown,
    /// The run is settled: finish the operation in this phase.
    Settled(RunPhase, Option<String>),
    /// The run cannot go on: fail it with this code.
    Refused(String),
    /// A newer run owns the target.
    Superseded,
    /// Try again later.
    Retry(Error),
    /// Check again after this long: waiting is not a failure.
    Wait(Duration, &'static str),
}

impl From<Error> for Stop {
    fn from(e: Error) -> Self {
        Self::Retry(e)
    }
}

impl From<kube::Error> for Stop {
    fn from(e: kube::Error) -> Self {
        Self::Retry(e.into())
    }
}

impl From<StoreError> for Stop {
    fn from(e: StoreError) -> Self {
        Self::Retry(e.into())
    }
}

pub(super) fn refused(code: &str) -> Stop {
    Stop::Refused(code.to_owned())
}

/// The failure code of a run whose plan cannot be rendered.
fn plan_refusal(e: &RenderPlanError) -> &'static str {
    match e {
        RenderPlanError::Build(b) => b.reason(),
        RenderPlanError::TooLarge { .. } => "PlanTooLarge",
        RenderPlanError::Incomplete(_) | RenderPlanError::Serialize(_) => "RenderFailed",
    }
}

/// The materializer of one process.
#[derive(Clone)]
pub struct Worker {
    pub(super) store: Store,
    pub(super) client: Client,
    id: String,
    pub(super) verify_deadline: Duration,
    pub(super) deletion_check: Duration,
    /// Hands envelopes to cluster agents; agent-delivered targets wait
    /// without it.
    pub(super) agents: Option<std::sync::Arc<dyn super::agent::AgentDispatch>>,
    /// The cluster's discovered capabilities; plans wait for them. Without a
    /// channel (tests), plans follow `KubenConfig` alone.
    facts: Option<Facts>,
    /// Opens the secret revisions runs are bound to; runs bound to any
    /// fail without it.
    pub(super) keyring: Option<std::sync::Arc<crate::secrets::Keyring>>,
}

impl Debug for Worker {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Worker")
            .field("id", &self.id)
            .finish_non_exhaustive()
    }
}

/// Run `worker` until `token` is cancelled. A database outage returns an
/// error, for the supervisor to restart the loop with backoff.
pub async fn run(worker: Worker, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    while !token.is_cancelled() {
        if worker.work_once(&token).await?.is_none() {
            tokio::select! {
                () = token.cancelled() => {}
                () = tokio::time::sleep(IDLE) => {}
            }
        }
    }
    Ok(())
}

impl Worker {
    /// A worker that claims operations as `id`, unique per process.
    #[must_use]
    pub fn new(store: Store, client: Client, id: impl Into<String>) -> Self {
        Self {
            store,
            client,
            id: id.into(),
            verify_deadline: VERIFY_DEADLINE,
            deletion_check: DELETION_CHECK,
            agents: None,
            facts: None,
            keyring: None,
        }
    }

    /// Render plans against the capabilities discovery publishes on `facts`.
    #[must_use]
    pub fn with_facts(mut self, facts: Facts) -> Self {
        self.facts = Some(facts);
        self
    }

    /// Deliver agent-delivered targets through `agents` (the hub).
    #[must_use]
    pub fn with_agents(mut self, agents: std::sync::Arc<dyn super::agent::AgentDispatch>) -> Self {
        self.agents = Some(agents);
        self
    }

    /// Open managed secret revisions with `keyring`.
    #[must_use]
    pub fn with_keyring(mut self, keyring: std::sync::Arc<crate::secrets::Keyring>) -> Self {
        self.keyring = Some(keyring);
        self
    }

    /// How long an environment deletion waits before it checks again; 20
    /// seconds unless set.
    #[must_use]
    pub const fn with_deletion_check(mut self, after: Duration) -> Self {
        self.deletion_check = after;
        self
    }

    /// How long verification waits for the App to become ready; 15 minutes
    /// unless set.
    #[must_use]
    pub const fn with_verify_deadline(mut self, deadline: Duration) -> Self {
        self.verify_deadline = deadline;
        self
    }

    /// Claim one due operation and carry it as far as it goes. `None` when
    /// nothing was due.
    pub async fn work_once(&self, token: &CancellationToken) -> Result<Option<OperationId>, Error> {
        let Some(claim) = self.store.claim_operation(&self.id, &KINDS, LEASE).await? else {
            return Ok(None);
        };
        let stop = if claim.kind == RUN_KIND {
            self.carry(&claim, token).await
        } else {
            self.carry_lifecycle(&claim).await
        };
        self.settle(&claim, stop).await?;
        Ok(Some(claim.id))
    }

    async fn carry(&self, claim: &Claim, token: &CancellationToken) -> Stop {
        let read = async {
            let mut tenant = self.store.tenant(claim.org).await?;
            tenant.materialization(claim.id).await
        };
        let m = match read.await {
            Ok(Some(m)) => m,
            Ok(None) => return Stop::Settled(RunPhase::Failed, Some("RunMissing".into())),
            Err(e) => return e.into(),
        };
        let stop = match self.drive(claim, &m, token).await {
            Ok(never) => match never {},
            Err(stop) => stop,
        };
        if matches!(stop, Stop::Settled(RunPhase::Succeeded, _))
            && !m.secrets.is_empty()
            && let Err(e) = self.collect_secrets(&m).await
        {
            tracing::warn!(run = %m.run, error = %e, "unused secret revisions stay for now");
        }
        match stop {
            Stop::Refused(code) => self.end(claim, &m, RunEvent::Failed, Some(code)).await,
            Stop::Superseded => self.end(claim, &m, RunEvent::Superseded, None).await,
            Stop::Retry(e) if claim.attempt >= MAX_ATTEMPTS => {
                tracing::warn!(run = %m.run, error = %e, "giving up after {MAX_ATTEMPTS} attempts");
                self.end(claim, &m, RunEvent::Failed, Some("RetriesExhausted".into()))
                    .await
            }
            other => other,
        }
    }

    async fn settle(&self, claim: &Claim, stop: Stop) -> Result<(), Error> {
        let (phase, code) = match stop {
            Stop::Fenced | Stop::Shutdown => return Ok(()),
            Stop::Retry(e) => {
                let delay = backoff(u32::try_from(claim.attempt).unwrap_or(u32::MAX));
                tracing::warn!(operation = %claim.id, error = %e, retry_in_s = delay.as_secs(), "materialization will be retried");
                self.store.retry_operation(claim, delay, e.code()).await?;
                return Ok(());
            }
            Stop::Wait(after, code) => {
                self.store.retry_operation(claim, after, code).await?;
                return Ok(());
            }
            Stop::Settled(phase, code) => (phase.as_str(), code),
            // `carry` turns these into a run event; settle the operation anyway.
            Stop::Refused(code) => ("failed", Some(code)),
            Stop::Superseded => ("superseded", None),
        };
        tracing::info!(operation = %claim.id, phase, code = code.as_deref().unwrap_or_default(), "materialization settled");
        self.store.finish_operation(claim, phase, code.as_deref()).await?;
        Ok(())
    }

    /// Move the run with a last `event` and settle in the resulting phase.
    async fn end(&self, claim: &Claim, m: &Materialization, event: RunEvent, code: Option<String>) -> Stop {
        match self.advance(claim, m, event).await {
            Ok(phase) => Stop::Settled(phase, code),
            Err(stop) => stop,
        }
    }

    async fn drive(
        &self,
        claim: &Claim,
        m: &Materialization,
        token: &CancellationToken,
    ) -> Result<Infallible, Stop> {
        if m.phase.is_final() || m.phase == RunPhase::Failed {
            return Err(Stop::Settled(m.phase, None));
        }
        if m.deleting {
            return Err(refused("TargetDeleting"));
        }
        if m.phase == RunPhase::AwaitingApproval {
            return Err(self.await_approval(claim, m).await);
        }
        if matches!(m.phase, RunPhase::Planned | RunPhase::PendingDelivery) {
            self.hold_if_paused(m).await?;
        }
        if m.delivery == kuben_store::repo::Delivery::Agent {
            return self.drive_agent(claim, m, token).await;
        }
        let mut phase = m.phase;
        if phase == RunPhase::Planned {
            phase = self.advance(claim, m, RunEvent::ReadyForDelivery).await?;
        }
        let written = match phase {
            RunPhase::PendingDelivery => {
                let written = self.deliver(claim, m, token).await?;
                phase = self.advance(claim, m, RunEvent::AcceptedByCluster).await?;
                written
            }
            RunPhase::AcceptedByCluster | RunPhase::Preflight | RunPhase::Applying | RunPhase::Verifying => {
                self.written_generation(m).await?
            }
            other => return Err(Error::Unsupported(other.as_str()).into()),
        };
        self.verify(claim, m, phase, written, token).await
    }

    /// Render and write the run's objects; the `metadata.generation` of the
    /// App object written.
    async fn deliver(
        &self,
        claim: &Claim,
        m: &Materialization,
        token: &CancellationToken,
    ) -> Result<i64, Stop> {
        let rendered = render(m).map_err(|e| refused(e.code()))?;
        if m.render_plan.is_none() {
            self.freeze(claim, m, &rendered.app).await?;
        }
        let project = self
            .ensure(Api::<Project>::all(self.client.clone()), &rendered.project, m.org)
            .await?;
        let mut environment = rendered.environment;
        if !render::set_owner(&mut environment, &project) {
            return Err(Error::Incomplete(format!("Project/{}", m.project_slug)).into());
        }
        self.ensure(Api::<Environment>::all(self.client.clone()), &environment, m.org)
            .await?;
        self.wait_namespace(&m.namespace, token).await?;
        self.write_secrets(m).await?;
        let app = self.write_app(m, &rendered.app).await?;
        let (Some(uid), Some(generation)) = (app.metadata.uid.as_deref(), app.metadata.generation) else {
            return Err(Error::Incomplete(format!("App/{}", m.application_slug)).into());
        };
        if !self
            .store
            .record_materialization(claim, m, uid, generation)
            .await?
        {
            // Fenced off, or a newer generation is recorded already.
            return Err(Stop::Superseded);
        }
        Ok(generation)
    }

    /// Freeze the run's RenderPlan before its first write (ADR-026): the App
    /// rendered against the cluster's capabilities now. A retry finds the
    /// plan frozen and never renders it again.
    pub(super) async fn freeze(&self, claim: &Claim, m: &Materialization, app: &App) -> Result<(), Stop> {
        let facts = match &self.facts {
            Some(rx) => match discovery::current(rx) {
                Some(facts) => Some(facts),
                // A plan is frozen for good: never render it before the
                // cluster's capabilities are known.
                None => return Err(Stop::Wait(POLL, "CapabilitiesPending")),
            },
            None => None,
        };
        let configs = Api::<KubenConfig>::all(self.client.clone())
            .list(&ListParams::default())
            .await?;
        let capabilities = Capabilities::of(&platform_of(&configs.items).gated(facts.as_deref()));
        let plan = plans::render(app, &capabilities).map_err(|e| refused(plan_refusal(&e)))?;
        let (Ok(snapshot), Ok(resources)) = (plan.capabilities_json(), plan.resources()) else {
            return Err(refused("RenderFailed"));
        };
        let mut tenant = self.store.tenant(m.org).await?;
        let Some(id) = tenant
            .freeze_run_plan(claim, m.run, plan.renderer_version(), &snapshot, &resources)
            .await?
        else {
            return Err(Stop::Fenced);
        };
        tenant.commit().await?;
        tracing::info!(run = %m.run, plan = %id, digest = %plan.digest, "render plan frozen");
        Ok(())
    }

    /// Write a project or environment object, which many targets share: only
    /// an object of the same organization is taken over.
    pub(super) async fn ensure<K>(&self, api: Api<K>, desired: &K, org: OrgId) -> Result<K, Stop>
    where
        K: Resource + Clone + Serialize + DeserializeOwned + Debug,
    {
        let name = desired.meta().name.clone().unwrap_or_default();
        for _ in 0..MAX_CONFLICTS {
            let live = api.get_opt(&name).await?;
            if live.as_ref().is_some_and(|l| !write::belongs_to(l.meta(), org)) {
                return Err(refused("NameTaken"));
            }
            match write::put(&api, desired, live.as_ref()).await {
                Ok(written) => return Ok(written),
                Err(e) if write::is_conflict(&e) => {}
                Err(e) => return Err(e.into()),
            }
        }
        Err(Error::Contended(name).into())
    }

    /// Hold the run of a paused target before it writes anything (M4.9);
    /// an emergency rollback is never held. Checked again at the write, so a
    /// pause observed before it stops the write.
    pub(super) async fn hold_if_paused(&self, m: &Materialization) -> Result<(), Stop> {
        if m.emergency {
            return Ok(());
        }
        let paused = {
            let mut tenant = self.store.tenant(m.org).await?;
            tenant.target_paused(m.target).await?
        };
        if paused {
            return Err(Stop::Wait(PAUSE_RECHECK, "Paused"));
        }
        Ok(())
    }

    /// Write the App object under the generation fence.
    pub(super) async fn write_app(&self, m: &Materialization, desired: &App) -> Result<App, Stop> {
        let api = Api::<App>::namespaced(self.client.clone(), &m.namespace);
        for _ in 0..MAX_CONFLICTS {
            let live = api.get_opt(&m.application_slug).await?;
            let meta = live.as_ref().map(|a| a.metadata.clone()).unwrap_or_default();
            if live.is_some() && !write::belongs_to(&meta, m.org) {
                return Err(refused("NameTaken"));
            }
            // SQL is read after the object: see `fence`.
            match fence(m.generation, self.current_generation(m).await?, &meta) {
                Fence::Superseded { .. } => return Err(Stop::Superseded),
                Fence::Forged { live } => tracing::warn!(
                    target = %m.target, live, ours = m.generation.0,
                    "replacing a generation annotation Kuben never wrote"
                ),
                Fence::Write => {}
            }
            self.hold_if_paused(m).await?;
            match write::put(&api, desired, live.as_ref()).await {
                Ok(written) => return Ok(written),
                Err(e) if write::is_conflict(&e) => {}
                Err(e) => return Err(e.into()),
            }
        }
        Err(Error::Contended(m.application_slug.clone()).into())
    }

    async fn current_generation(&self, m: &Materialization) -> Result<Generation, Stop> {
        let mut tenant = self.store.tenant(m.org).await?;
        let state = tenant.target_state(m.target).await?;
        state
            .map(|s| s.desired_generation)
            .ok_or_else(|| refused("TargetMissing"))
    }

    /// The written App's `metadata.generation`, when a run resumes after
    /// its delivery.
    async fn written_generation(&self, m: &Materialization) -> Result<i64, Stop> {
        let mut tenant = self.store.tenant(m.org).await?;
        match tenant.materialized(m.target).await? {
            Some(w) if w.generation.0 == m.generation.0 => Ok(w.resource_generation),
            _ => Err(refused("NotDelivered")),
        }
    }

    pub(super) async fn wait_namespace(&self, name: &str, token: &CancellationToken) -> Result<(), Stop> {
        let api = Api::<Namespace>::all(self.client.clone());
        let deadline = Instant::now() + NAMESPACE_WAIT;
        loop {
            if api.get_opt(name).await?.is_some() {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(Error::NamespacePending(name.to_owned()).into());
            }
            pause(token).await?;
        }
    }

    /// Follow the App controller until the written generation is ready,
    /// failed, or the deadline passes.
    async fn verify(
        &self,
        claim: &Claim,
        m: &Materialization,
        mut phase: RunPhase,
        written: i64,
        token: &CancellationToken,
    ) -> Result<Infallible, Stop> {
        let api = Api::<App>::namespaced(self.client.clone(), &m.namespace);
        let deadline = Instant::now() + self.verify_deadline;
        loop {
            if !self.store.renew_lease(claim, LEASE).await? {
                return Err(Stop::Fenced);
            }
            let app = api
                .get_opt(&m.application_slug)
                .await?
                .ok_or_else(|| refused("AppDeleted"))?;
            match progress(&app, written) {
                Progress::Pending => {}
                Progress::Applied => phase = self.to_verifying(claim, m, phase).await?,
                Progress::Ready => {
                    self.to_verifying(claim, m, phase).await?;
                    let done = self.advance(claim, m, RunEvent::Verified).await?;
                    return Err(Stop::Settled(done, None));
                }
                Progress::Failed(reason) => return Err(Stop::Refused(reason)),
            }
            if Instant::now() >= deadline {
                return Err(refused("VerifyTimeout"));
            }
            pause(token).await?;
        }
    }

    /// The controller applied the written generation: the run is past its
    /// preflight and applying, and is verifying.
    pub(super) async fn to_verifying(
        &self,
        claim: &Claim,
        m: &Materialization,
        mut phase: RunPhase,
    ) -> Result<RunPhase, Stop> {
        loop {
            let event = match phase {
                RunPhase::AcceptedByCluster => RunEvent::PreflightStarted,
                RunPhase::Preflight => RunEvent::PreflightPassed,
                RunPhase::Applying => RunEvent::Applied,
                _ => return Ok(phase),
            };
            phase = self.advance(claim, m, event).await?;
        }
    }

    pub(super) async fn advance(
        &self,
        claim: &Claim,
        m: &Materialization,
        event: RunEvent,
    ) -> Result<RunPhase, Stop> {
        match self.store.advance_run(claim, m.run, event).await? {
            Advance::Moved(phase) => Ok(phase),
            Advance::Fenced => Err(Stop::Fenced),
            // Settled meanwhile, e.g. superseded by a newer run.
            Advance::Illegal(i) if i.terminal => Err(Stop::Settled(
                RunPhase::parse(i.from).unwrap_or(RunPhase::Failed),
                None,
            )),
            Advance::Illegal(i) => Err(Error::Illegal(i.to_string()).into()),
        }
    }
}

impl Worker {
    /// A run waiting for approval (M4.1): cancelled once its window has
    /// closed, otherwise looked at again then. A decision wakes it sooner.
    async fn await_approval(&self, claim: &Claim, m: &Materialization) -> Stop {
        match approval_wait(m.approval_expires_at, kuben_core::time::now_ms()) {
            Some(wait) => Stop::Wait(wait, "AwaitingApproval"),
            None => match self.advance(claim, m, RunEvent::Rejected).await {
                Ok(phase) => Stop::Settled(phase, Some("ApprovalExpired".into())),
                Err(stop) => stop,
            },
        }
    }
}

/// How long a run waiting for approval sleeps at `now`: until its window
/// closes, at most an hour, at least one poll. `None` once it has closed or
/// when the run has no window.
fn approval_wait(expires_at: Option<i64>, now: i64) -> Option<Duration> {
    let left = expires_at?.checked_sub(now).filter(|ms| *ms > 0)?;
    let left = Duration::from_millis(u64::try_from(left).ok()?);
    Some(left.clamp(POLL, APPROVAL_RECHECK))
}

pub(super) async fn pause(token: &CancellationToken) -> Result<(), Stop> {
    tokio::select! {
        () = token.cancelled() => Err(Stop::Shutdown),
        () = tokio::time::sleep(POLL) => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn runs_wait_for_approval_until_their_window_closes() {
        assert_eq!(approval_wait(None, 0), None, "no window: nothing to wait for");
        assert_eq!(approval_wait(Some(1_000), 1_000), None, "closed");
        assert_eq!(approval_wait(Some(1_000), 2_000), None, "closed long ago");
        assert_eq!(approval_wait(Some(1_001), 1_000), Some(POLL), "at least one poll");
        assert_eq!(approval_wait(Some(60_000), 0), Some(Duration::from_mins(1)));
        assert_eq!(
            approval_wait(Some(i64::MAX), 0),
            Some(APPROVAL_RECHECK),
            "at most an hour"
        );
        assert_eq!(approval_wait(Some(i64::MAX), i64::MIN), None, "no overflow");
    }
}

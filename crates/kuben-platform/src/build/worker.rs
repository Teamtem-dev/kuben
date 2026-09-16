//! The build worker (ADR-028): claims `source.sync` and `build` operations.
//!
//! * A sync reads the branch head from the provider and records it
//!   ([`kuben_store::repo::HeadObserved`]); a new head queues a build.
//! * A build takes a slot, creates its [`BuildRun`], fetch-token Secret and
//!   Job ([`super::job`]), follows the Job ([`super::steps`]) and, when the
//!   pod reports a digest that the registry confirms, completes the attempt:
//!   release and, if the target still wants it, a deployment run.
//! * A stop deletes the Job and waits until it is gone. Every final attempt
//!   revokes its fetch token and deletes its objects before its operation is
//!   settled, so a crash in between only repeats the cleanup.
//!
//! Claims are fenced in SQL, so any number of replicas may run workers.

use std::{fmt, sync::Arc, time::Duration};

use k8s_openapi::{
    api::{
        batch::v1::Job,
        core::v1::{Pod, Secret},
    },
    jiff::Timestamp,
};
use kube::{
    Api, Client,
    api::{DeleteParams, ListParams, Patch, PatchParams, PostParams, PropagationPolicy},
};
use kuben_core::{
    artifact::Digest,
    ids::OperationId,
    ops::{BuildEvent, BuildFailure, BuildPhase, outcome::classify},
    time::now_ms,
};
use kuben_crd::BuildRun;
use kuben_store::{
    Store, StoreError,
    repo::{
        BUILD_KIND, BuildAdvance, BuildAttempt, BuildProgress, Claim, Completed, HeadObserved,
        SOURCE_SYNC_KIND, SlotLimits, Tenant,
    },
};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use super::{
    FetchToken, OutputVerifier, ProviderError, SourceProvider, VerifyError,
    job::{self, ATTEMPT_LABEL, BuildSettings},
    observe,
    steps::{self, Next, Plan},
};
use crate::{controller::backoff, health::Health};

/// Health registry name of the worker loop.
const HEALTH: &str = "builds";
const LEASE: Duration = Duration::from_mins(2);
const IDLE: Duration = Duration::from_secs(2);
/// Pause between reads of a running build.
const POLL: Duration = Duration::from_secs(5);
/// Pause before a blocked build asks for a slot again.
const BLOCKED_WAIT: Duration = Duration::from_secs(15);
/// Claims of one sync before it gives up on the provider.
const MAX_SYNC_ATTEMPTS: i32 = 10;
/// How long past its deadline a build may keep failing on infrastructure.
const GIVE_UP_AFTER: Duration = Duration::from_hours(1);
const KINDS: [&str; 2] = [SOURCE_SYNC_KIND, BUILD_KIND];
const MANAGER: &str = "kuben-builds";
const MANAGED_BY: &str = "app.kubernetes.io/managed-by";
/// How long a finished `BuildRun` shows its result.
pub const RETENTION: Duration = Duration::from_hours(24);
const RETENTION_SECS: i64 = 24 * 3600;
/// How often finished `BuildRun`s are swept.
const SWEEP_EVERY: Duration = Duration::from_mins(10);

/// How the work on a claim ended.
#[derive(Debug, PartialEq, Eq)]
enum Outcome {
    /// Settle the operation in this phase.
    Done(&'static str, Option<String>),
    /// Look again after this long; waiting is not a failure.
    Wait(Duration, &'static str),
    /// Something outside failed; try again with backoff.
    Retry(&'static str, String),
    /// Another worker holds the claim now.
    Fenced,
}

impl From<StoreError> for Outcome {
    fn from(e: StoreError) -> Self {
        Self::Retry("StoreError", e.to_string())
    }
}

impl From<kube::Error> for Outcome {
    fn from(e: kube::Error) -> Self {
        Self::Retry("KubernetesError", e.to_string())
    }
}

/// Why the build's objects were not created.
enum CreateError {
    Provider(ProviderError),
    Kube(kube::Error),
    Render(String),
}

impl From<kube::Error> for CreateError {
    fn from(e: kube::Error) -> Self {
        Self::Kube(e)
    }
}

/// The build worker of one process.
#[derive(Clone)]
pub struct BuildWorker {
    store: Store,
    client: Client,
    id: String,
    provider: Arc<dyn SourceProvider>,
    verifier: Arc<dyn OutputVerifier>,
    settings: BuildSettings,
    limits: SlotLimits,
}

impl fmt::Debug for BuildWorker {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("BuildWorker")
            .field("id", &self.id)
            .field("namespace", &self.settings.namespace)
            .finish_non_exhaustive()
    }
}

/// Run `worker` until `token` is cancelled. A database outage returns an
/// error, for the supervisor to restart the loop with backoff.
pub async fn run(worker: BuildWorker, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    let mut next_sweep = tokio::time::Instant::now();
    while !token.is_cancelled() {
        if tokio::time::Instant::now() >= next_sweep {
            next_sweep += SWEEP_EVERY;
            match worker.sweep().await {
                Ok(0) => {}
                Ok(n) => tracing::info!(removed = n, "finished BuildRuns swept"),
                Err(e) => tracing::warn!(error = %e, "finished BuildRuns were not swept"),
            }
        }
        if worker.work_once().await?.is_none() {
            tokio::select! {
                () = token.cancelled() => {}
                () = tokio::time::sleep(IDLE) => {}
            }
        }
    }
    Ok(())
}

fn not_found(e: &kube::Error) -> bool {
    matches!(e, kube::Error::Api(s) if s.code == 404)
}

fn conflict(e: &kube::Error) -> bool {
    matches!(e, kube::Error::Api(s) if s.code == 409)
}

fn invalid(e: &kube::Error) -> bool {
    matches!(e, kube::Error::Api(s) if s.code == 422 || s.code == 400)
}

/// The phase `BuildRun.status.phase` shows.
const fn crd_phase(phase: BuildPhase) -> &'static str {
    match phase {
        BuildPhase::Queued | BuildPhase::Blocked => "Queued",
        BuildPhase::Succeeded => "Succeeded",
        BuildPhase::Failed => "Failed",
        BuildPhase::Cancelled => "Cancelled",
        _ => "Running",
    }
}

impl BuildWorker {
    /// A worker that claims operations as `id`, unique per process.
    #[must_use]
    pub fn new(
        store: Store,
        client: Client,
        id: impl Into<String>,
        provider: Arc<dyn SourceProvider>,
        verifier: Arc<dyn OutputVerifier>,
        settings: BuildSettings,
        limits: SlotLimits,
    ) -> Self {
        Self {
            store,
            client,
            id: id.into(),
            provider,
            verifier,
            settings,
            limits,
        }
    }

    /// Claim one due operation and carry it as far as it goes now. `None`
    /// when nothing was due.
    pub async fn work_once(&self) -> Result<Option<OperationId>, StoreError> {
        let Some(claim) = self.store.claim_operation(&self.id, &KINDS, LEASE).await? else {
            return Ok(None);
        };
        let outcome = if claim.kind == SOURCE_SYNC_KIND {
            self.sync(&claim).await
        } else {
            self.build(&claim).await
        };
        self.settle(&claim, outcome).await?;
        Ok(Some(claim.id))
    }

    async fn settle(&self, claim: &Claim, outcome: Outcome) -> Result<(), StoreError> {
        match outcome {
            Outcome::Done(phase, code) => {
                tracing::info!(operation = %claim.id, kind = %claim.kind, phase, code = code.as_deref().unwrap_or_default(), "build operation settled");
                self.store.finish_operation(claim, phase, code.as_deref()).await?;
            }
            Outcome::Wait(after, code) => {
                self.store.retry_operation(claim, after, code).await?;
            }
            Outcome::Retry(code, detail) => {
                let delay = backoff(u32::try_from(claim.attempt).unwrap_or(u32::MAX));
                tracing::warn!(operation = %claim.id, kind = %claim.kind, code, %detail, retry_in_s = delay.as_secs(), "build operation will be retried");
                self.store.retry_operation(claim, delay, code).await?;
            }
            Outcome::Fenced => {}
        }
        Ok(())
    }

    // ---- source sync ----

    async fn sync(&self, claim: &Claim) -> Outcome {
        let binding = async {
            let mut t = self.store.tenant(claim.org).await?;
            t.sync_binding(claim).await
        };
        let binding = match binding.await {
            Ok(Some(b)) => b,
            Ok(None) => return Outcome::Done("failed", Some("BindingMissing".into())),
            Err(e) => return e.into(),
        };
        if matches!(
            self.store.git_installation_org(binding.installation_id).await,
            Ok(Some((_, true)))
        ) {
            return Outcome::Done("failed", Some(BuildFailure::CredentialsRefused.code().into()));
        }
        let head = match self
            .provider
            .head(binding.installation_id, &binding.repository, &binding.branch)
            .await
        {
            Ok(head) => head,
            Err(ProviderError::NotFound(what)) => {
                tracing::warn!(binding = %binding.id, %what, "the source is gone");
                return Outcome::Done("failed", Some("SourceNotFound".into()));
            }
            Err(ProviderError::Refused(why)) => {
                tracing::warn!(binding = %binding.id, %why, "the provider refused the sync");
                return Outcome::Done("failed", Some(BuildFailure::CredentialsRefused.code().into()));
            }
            Err(ProviderError::Unavailable(why)) if claim.attempt >= MAX_SYNC_ATTEMPTS => {
                tracing::warn!(binding = %binding.id, %why, "giving up on the provider");
                return Outcome::Done("failed", Some("ProviderUnavailable".into()));
            }
            Err(ProviderError::Unavailable(why)) => return Outcome::Retry("ProviderUnavailable", why),
        };
        let observed = async {
            let mut t = self.store.tenant(claim.org).await?;
            let observed = t
                .observe_head(claim, &binding, &head.commit, head.repository_id)
                .await?;
            t.commit().await?;
            Ok::<_, StoreError>(observed)
        };
        match observed.await {
            Err(e) => e.into(),
            Ok(HeadObserved::Unchanged) => Outcome::Done("succeeded", Some("Unchanged".into())),
            Ok(HeadObserved::Queued { attempt, epoch, .. }) => {
                tracing::info!(binding = %binding.id, %attempt, commit = %head.commit, epoch = epoch.0, "build queued");
                Outcome::Done("succeeded", None)
            }
            Ok(HeadObserved::RepositoryChanged) => Outcome::Done("failed", Some("RepositoryChanged".into())),
            Ok(HeadObserved::Gone) => Outcome::Done("failed", Some("TargetGone".into())),
            Ok(HeadObserved::Fenced) => Outcome::Fenced,
        }
    }

    // ---- builds ----

    async fn build(&self, claim: &Claim) -> Outcome {
        let read = async {
            let mut t = self.store.tenant(claim.org).await?;
            t.build_of_operation(claim.id).await
        };
        let attempt = match read.await {
            Ok(Some(a)) => a,
            Ok(None) => return Outcome::Done("failed", Some("BuildMissing".into())),
            Err(e) => return e.into(),
        };
        let outcome = match steps::next(attempt.phase, attempt.cancel_requested) {
            Next::Settle => self.settle_final(&attempt).await,
            Next::CancelQueued => self.cancel_queued(claim, &attempt).await,
            Next::Admit => self.admit(claim, &attempt).await,
            Next::Observe => self.observe(claim, &attempt).await,
        };
        match outcome {
            Outcome::Retry(code, detail) if self.overdue(&attempt) => {
                tracing::warn!(build = %attempt.id, code, %detail, "giving up on the build");
                self.fail(claim, &attempt, &[], "RetriesExhausted", &detail).await
            }
            other => other,
        }
    }

    fn overdue(&self, attempt: &BuildAttempt) -> bool {
        let limit = self.settings.deadline + GIVE_UP_AFTER;
        let age = now_ms().saturating_sub(attempt.created_at);
        u128::try_from(age).unwrap_or(0) > limit.as_millis()
    }

    /// A final attempt: revoke, delete, then settle its operation.
    async fn settle_final(&self, attempt: &BuildAttempt) -> Outcome {
        let code = attempt
            .failure
            .clone()
            .or_else(|| attempt.deploy_decision.clone());
        self.finish(attempt, attempt.phase, code, attempt.digest.as_ref())
            .await
    }

    /// Record `events` in order, with `progress` on each.
    async fn record(
        &self,
        claim: &Claim,
        attempt: &BuildAttempt,
        events: &[BuildEvent],
        progress: &BuildProgress,
    ) -> Result<BuildPhase, Outcome> {
        let mut t = self.store.tenant(claim.org).await?;
        let phase = apply(&mut t, claim, attempt, events, progress).await?;
        t.commit().await?;
        Ok(phase)
    }

    async fn cancel_queued(&self, claim: &Claim, attempt: &BuildAttempt) -> Outcome {
        match self
            .record(
                claim,
                attempt,
                &[BuildEvent::CancelRequested],
                &BuildProgress::default(),
            )
            .await
        {
            Ok(_) => self.finish(attempt, BuildPhase::Cancelled, None, None).await,
            Err(outcome) => outcome,
        }
    }

    async fn admit(&self, claim: &Claim, attempt: &BuildAttempt) -> Outcome {
        let slot = async {
            let mut t = self.store.tenant(claim.org).await?;
            let slot = t.claim_build_slot(claim, attempt.id, self.limits).await?;
            t.commit().await?;
            Ok::<_, StoreError>(slot)
        };
        match slot.await {
            Err(e) => return e.into(),
            Ok(None) => return Outcome::Fenced,
            Ok(Some(false)) => {
                if attempt.phase != BuildPhase::Blocked {
                    let blocked = BuildProgress {
                        blocked_reason: Some("NoBuildCapacity".into()),
                        ..BuildProgress::default()
                    };
                    if let Err(o) = self
                        .record(claim, attempt, &[BuildEvent::Blocked], &blocked)
                        .await
                    {
                        return o;
                    }
                }
                return Outcome::Wait(BLOCKED_WAIT, "NoBuildCapacity");
            }
            Ok(Some(true)) => {}
        }
        let name = match self.create_objects(attempt).await {
            Ok(name) => name,
            Err(CreateError::Provider(ProviderError::Unavailable(why))) => {
                return Outcome::Retry("ProviderUnavailable", why);
            }
            Err(CreateError::Provider(ProviderError::Refused(why))) => {
                return self
                    .fail(claim, attempt, &[], BuildFailure::CredentialsRefused.code(), &why)
                    .await;
            }
            Err(CreateError::Provider(ProviderError::NotFound(what))) => {
                return self
                    .fail(claim, attempt, &[], BuildFailure::SourceUnavailable.code(), &what)
                    .await;
            }
            Err(CreateError::Kube(e)) if invalid(&e) => {
                return self
                    .fail(
                        claim,
                        attempt,
                        &[],
                        BuildFailure::InvalidBudget.code(),
                        &e.to_string(),
                    )
                    .await;
            }
            Err(CreateError::Kube(e)) => return e.into(),
            Err(CreateError::Render(e)) => {
                return self
                    .fail(claim, attempt, &[], BuildFailure::InvalidBudget.code(), &e)
                    .await;
            }
        };
        let mut events = Vec::new();
        if attempt.phase == BuildPhase::Blocked {
            events.push(BuildEvent::Unblocked);
        }
        events.push(BuildEvent::Started);
        let started = BuildProgress {
            job_name: Some(name),
            ..BuildProgress::default()
        };
        match self.record(claim, attempt, &events, &started).await {
            Ok(phase) => {
                self.mirror(attempt, phase, None).await;
                Outcome::Wait(POLL, "Preparing")
            }
            Err(o) => o,
        }
    }

    async fn observe(&self, claim: &Claim, attempt: &BuildAttempt) -> Outcome {
        let seen = match self.observe_job(attempt).await {
            Ok(seen) => seen,
            Err(e) => return e.into(),
        };
        let verdict = classify(&seen);
        let plan = steps::plan(
            attempt.phase,
            attempt.cancel_requested,
            &verdict,
            attempt.reported_digest.as_ref(),
        );
        match plan {
            Plan::Wait(events) => match self
                .record(claim, attempt, &events, &BuildProgress::default())
                .await
            {
                Ok(phase) => {
                    if !events.is_empty() {
                        self.mirror(attempt, phase, None).await;
                    }
                    Outcome::Wait(POLL, "Building")
                }
                Err(o) => o,
            },
            Plan::Verify { events, digest } => {
                let reported = BuildProgress {
                    reported_digest: Some(digest.clone()),
                    ..BuildProgress::default()
                };
                if let Err(o) = self.record(claim, attempt, &events, &reported).await {
                    return o;
                }
                self.verify(claim, attempt, &digest).await
            }
            Plan::Fail {
                events,
                failure,
                detail,
            } => self.fail(claim, attempt, &events, failure.code(), &detail).await,
            Plan::Stop(events) => {
                if let Err(e) = delete(&self.api::<Job>(), &job::name(attempt)).await {
                    return e.into();
                }
                match self
                    .record(claim, attempt, &events, &BuildProgress::default())
                    .await
                {
                    Ok(_) => Outcome::Wait(POLL, "Stopping"),
                    Err(o) => o,
                }
            }
            Plan::Cancelled(events) => {
                if let Err(o) = self
                    .record(claim, attempt, &events, &BuildProgress::default())
                    .await
                {
                    return o;
                }
                self.finish(attempt, BuildPhase::Cancelled, None, None).await
            }
        }
    }

    /// Check `digest` in the registry and complete the attempt.
    async fn verify(&self, claim: &Claim, attempt: &BuildAttempt, digest: &Digest) -> Outcome {
        match self.verifier.verify(&attempt.image_repository, digest).await {
            Ok(()) => {}
            Err(VerifyError::Missing(what)) => {
                return self
                    .fail(claim, attempt, &[], BuildFailure::OutputRejected.code(), &what)
                    .await;
            }
            Err(VerifyError::Unavailable(why)) => return Outcome::Retry("RegistryUnavailable", why),
        }
        let completed = async {
            let mut t = self.store.tenant(claim.org).await?;
            let done = t.complete_build(claim, attempt, digest).await?;
            if matches!(done, Completed::Deployed { .. } | Completed::Kept { .. }) {
                t.commit().await?;
            }
            Ok::<_, StoreError>(done)
        };
        match completed.await {
            Err(e) => e.into(),
            Ok(Completed::Fenced) => Outcome::Fenced,
            Ok(Completed::Illegal(illegal)) => {
                tracing::warn!(build = %attempt.id, %illegal, "the build cannot complete from its phase");
                Outcome::Retry("IllegalTransition", illegal.to_string())
            }
            Ok(Completed::Deployed {
                release,
                run,
                generation,
            }) => {
                tracing::info!(build = %attempt.id, %release, %run, generation = generation.0, %digest, "build deployed");
                self.finish(attempt, BuildPhase::Succeeded, None, Some(digest))
                    .await
            }
            Ok(Completed::Kept { release, decision }) => {
                tracing::info!(build = %attempt.id, %release, %decision, %digest, "build kept without a deploy");
                self.finish(attempt, BuildPhase::Succeeded, Some(decision), Some(digest))
                    .await
            }
        }
    }

    /// Fail the attempt with `code`, queue a retry for a lost worker, and
    /// clean up.
    async fn fail(
        &self,
        claim: &Claim,
        attempt: &BuildAttempt,
        events: &[BuildEvent],
        code: &str,
        detail: &str,
    ) -> Outcome {
        let failed = BuildProgress {
            failure: Some((code.to_owned(), detail.to_owned())),
            ..BuildProgress::default()
        };
        let mut all = events.to_vec();
        all.push(BuildEvent::Failed);
        let retry = async {
            let mut t = self.store.tenant(claim.org).await?;
            let phase = apply(&mut t, claim, attempt, &all, &failed).await;
            let retried = match phase {
                Ok(_) if code == BuildFailure::LostWorker.code() => t.retry_build(attempt).await?,
                Ok(_) => None,
                Err(o) => return Ok(Err(o)),
            };
            t.commit().await?;
            Ok::<_, StoreError>(Ok(retried))
        };
        match retry.await {
            Err(e) => return e.into(),
            Ok(Err(o)) => return o,
            Ok(Ok(Some(next))) => tracing::info!(build = %attempt.id, %next, "a lost build is retried"),
            Ok(Ok(None)) => {}
        }
        tracing::info!(build = %attempt.id, code, detail, "build failed");
        self.finish(attempt, BuildPhase::Failed, Some(code.to_owned()), None)
            .await
    }

    /// Clean up a final attempt and settle its operation.
    async fn finish(
        &self,
        attempt: &BuildAttempt,
        phase: BuildPhase,
        code: Option<String>,
        digest: Option<&Digest>,
    ) -> Outcome {
        let mut shown = attempt.clone();
        if let Some(code) = code.as_ref().filter(|_| phase == BuildPhase::Failed) {
            shown.failure = Some(code.clone());
        }
        match self.cleanup(&shown, phase, digest).await {
            Ok(()) => Outcome::Done(operation_phase(phase), code),
            // The attempt is final; the next claim repeats the cleanup.
            Err(e) => e.into(),
        }
    }

    // ---- cluster ----

    fn api<K>(&self) -> Api<K>
    where
        K: kube::Resource<Scope = k8s_openapi::NamespaceResourceScope>,
        <K as kube::Resource>::DynamicType: Default,
    {
        Api::namespaced(self.client.clone(), &self.settings.namespace)
    }

    async fn create_objects(&self, attempt: &BuildAttempt) -> Result<String, CreateError> {
        let name = job::name(attempt);
        let runs: Api<BuildRun> = self.api();
        let run = match runs.get_opt(&name).await? {
            Some(run) => run,
            None => match runs
                .create(&PostParams::default(), &job::build_run(attempt, &self.settings))
                .await
            {
                Ok(run) => run,
                Err(e) if conflict(&e) => runs.get(&name).await?,
                Err(e) => return Err(e.into()),
            },
        };
        let jobs: Api<Job> = self.api();
        if jobs.get_opt(&name).await?.is_some() {
            return Ok(name);
        }
        let token = self
            .provider
            .fetch_token(attempt.installation_id, &attempt.repository)
            .await
            .map_err(CreateError::Provider)?;
        let secrets: Api<Secret> = self.api();
        let secret_name = job::secret_name(attempt);
        if secrets.get_opt(&secret_name).await?.is_some() {
            // Left by an interrupted attempt to create the Job: replace it.
            self.revoke(attempt).await;
            delete(&secrets, &secret_name).await?;
        }
        let secret = job::source_secret(attempt, &self.settings, &run, &token.token);
        if let Err(e) = secrets.create(&PostParams::default(), &secret).await {
            self.revoke_token(&token).await;
            return Err(e.into());
        }
        let clone_url = self.provider.clone_url(&attempt.repository);
        let job = job::job(attempt, &self.settings, &run, &clone_url)
            .map_err(|e| CreateError::Render(e.to_string()))?;
        match jobs.create(&PostParams::default(), &job).await {
            Ok(_) => {}
            Err(e) if conflict(&e) => {}
            Err(e) => return Err(e.into()),
        }
        tracing::info!(build = %attempt.id, job = %name, commit = %attempt.commit, "build job created");
        Ok(name)
    }

    async fn observe_job(
        &self,
        attempt: &BuildAttempt,
    ) -> kube::Result<kuben_core::ops::outcome::JobObservation> {
        let name = job::name(attempt);
        let jobs: Api<Job> = self.api();
        let found = jobs.get_opt(&name).await?;
        let pods = if found.is_some() {
            let pods: Api<Pod> = self.api();
            let selector = format!("{ATTEMPT_LABEL}={}", attempt.id);
            pods.list(&ListParams::default().labels(&selector)).await?.items
        } else {
            Vec::new()
        };
        Ok(observe::job(found.as_ref(), &pods, Timestamp::now()))
    }

    /// Revoke the fetch token, delete the Job and the Secret, and leave the
    /// final status on the `BuildRun`, which [`BuildWorker::sweep`] removes
    /// after [`RETENTION`].
    async fn cleanup(
        &self,
        attempt: &BuildAttempt,
        phase: BuildPhase,
        digest: Option<&Digest>,
    ) -> kube::Result<()> {
        self.revoke(attempt).await;
        delete(&self.api::<Job>(), &job::name(attempt)).await?;
        delete(&self.api::<Secret>(), &job::secret_name(attempt)).await?;
        self.mirror(attempt, phase, digest.map(Digest::as_str)).await;
        Ok(())
    }

    /// Delete `BuildRun`s that finished more than [`RETENTION`] ago.
    pub async fn sweep(&self) -> kube::Result<usize> {
        let runs: Api<BuildRun> = self.api();
        let list = runs
            .list(&ListParams::default().labels(&format!("{MANAGED_BY}=kuben")))
            .await?;
        let now = Timestamp::now();
        let mut removed = 0;
        for run in list.items {
            if expired(&run, now) {
                delete(&runs, run.metadata.name.as_deref().unwrap_or_default()).await?;
                removed += 1;
            }
        }
        Ok(removed)
    }

    async fn revoke(&self, attempt: &BuildAttempt) {
        let secrets: Api<Secret> = self.api();
        let secret = match secrets.get_opt(&job::secret_name(attempt)).await {
            Ok(Some(secret)) => secret,
            Ok(None) => return,
            Err(e) => {
                tracing::warn!(build = %attempt.id, error = %e, "cannot read the fetch token to revoke it");
                return;
            }
        };
        let token = secret
            .data
            .as_ref()
            .and_then(|d| d.get(job::token_key()))
            .and_then(|t| String::from_utf8(t.0.clone()).ok());
        if let Some(token) = token {
            self.revoke_token(&FetchToken {
                token,
                expires_at: std::time::SystemTime::now(),
            })
            .await;
        }
    }

    async fn revoke_token(&self, token: &FetchToken) {
        // An unrevoked token still expires within the hour.
        if let Err(e) = self.provider.revoke(token).await {
            tracing::warn!(error = %e, "a fetch token could not be revoked; it expires on its own");
        }
    }

    /// Show the attempt's progress on its `BuildRun`; best effort.
    async fn mirror(&self, attempt: &BuildAttempt, phase: BuildPhase, digest: Option<&str>) {
        let runs: Api<BuildRun> = self.api();
        let status = status_patch(attempt, phase, digest, Timestamp::now());
        let params = PatchParams::apply(MANAGER);
        if let Err(e) = runs
            .patch_status(&job::name(attempt), &params, &Patch::Merge(&status))
            .await
            && !not_found(&e)
        {
            tracing::debug!(build = %attempt.id, error = %e, "BuildRun status not updated");
        }
    }
}

/// The `BuildRun` status for `attempt` in `phase` at `now`: the phase,
/// the verified digest and, once final, when it finished and why.
fn status_patch(
    attempt: &BuildAttempt,
    phase: BuildPhase,
    digest: Option<&str>,
    now: Timestamp,
) -> serde_json::Value {
    let now = now.to_string();
    let terminal = phase.is_terminal();
    let (kind, reason, ok) = match phase {
        BuildPhase::Succeeded => ("Succeeded", "Verified", true),
        BuildPhase::Failed => ("Succeeded", attempt.failure.as_deref().unwrap_or("Failed"), false),
        BuildPhase::Cancelled => ("Succeeded", "Cancelled", false),
        other => ("Progressing", other.as_str(), true),
    };
    let mut status = json!({
        "phase": crd_phase(phase),
        "jobName": job::name(attempt),
        "conditions": [{
            "type": kind,
            "status": if ok { "True" } else { "False" },
            "reason": reason,
            "message": attempt.failure_detail,
            "lastTransitionTime": now,
        }],
    });
    if let Some(digest) = digest {
        status["imageDigest"] = json!(digest);
    }
    if phase == BuildPhase::Preparing {
        status["startedAt"] = json!(now);
    }
    if terminal {
        status["finishedAt"] = json!(now);
    }
    json!({ "status": status })
}

/// Whether `run` finished more than [`RETENTION`] before `now`.
fn expired(run: &BuildRun, now: Timestamp) -> bool {
    run.status
        .as_ref()
        .and_then(|s| s.finished_at.as_deref())
        .and_then(|t| t.parse::<Timestamp>().ok())
        .is_some_and(|finished| now.as_second() - finished.as_second() > RETENTION_SECS)
}

/// The operation phase of a final attempt.
const fn operation_phase(phase: BuildPhase) -> &'static str {
    match phase {
        BuildPhase::Succeeded => "succeeded",
        BuildPhase::Cancelled => "cancelled",
        _ => "failed",
    }
}

async fn delete<K>(api: &Api<K>, name: &str) -> kube::Result<()>
where
    K: kube::Resource + Clone + serde::de::DeserializeOwned + fmt::Debug,
{
    let params = DeleteParams {
        propagation_policy: Some(PropagationPolicy::Background),
        ..DeleteParams::default()
    };
    match api.delete(name, &params).await {
        Ok(_) => Ok(()),
        Err(e) if not_found(&e) => Ok(()),
        Err(e) => Err(e),
    }
}

/// Apply `events` to `attempt` in `t`, in order.
async fn apply(
    t: &mut Tenant,
    claim: &Claim,
    attempt: &BuildAttempt,
    events: &[BuildEvent],
    progress: &BuildProgress,
) -> Result<BuildPhase, Outcome> {
    let mut phase = attempt.phase;
    for event in events {
        match t.advance_build(claim, attempt.id, *event, progress).await? {
            BuildAdvance::Moved(next) => phase = next,
            BuildAdvance::Fenced => return Err(Outcome::Fenced),
            BuildAdvance::Illegal(illegal) => {
                return Err(Outcome::Retry("IllegalTransition", illegal.to_string()));
            }
        }
    }
    Ok(phase)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_build_run_shows_coarse_phases() {
        assert_eq!(crd_phase(BuildPhase::Blocked), "Queued");
        assert_eq!(crd_phase(BuildPhase::Publishing), "Running");
        assert_eq!(crd_phase(BuildPhase::Cancelling), "Running");
        assert_eq!(crd_phase(BuildPhase::Cancelled), "Cancelled");
    }

    fn attempt() -> BuildAttempt {
        crate::build::job::tests::attempt(kuben_core::source::BuildRecipe::default())
    }

    #[test]
    fn final_status_carries_the_digest_and_the_reason() {
        let now: Timestamp = "2026-09-17T12:00:00Z".parse().expect("time");
        let digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        let done = status_patch(&attempt(), BuildPhase::Succeeded, Some(digest), now);
        assert_eq!(done["status"]["phase"], "Succeeded");
        assert_eq!(done["status"]["imageDigest"], digest);
        assert_eq!(done["status"]["finishedAt"], "2026-09-17T12:00:00Z");
        assert_eq!(done["status"]["conditions"][0]["status"], "True");
        let status: kuben_crd::BuildRunStatus =
            serde_json::from_value(done["status"].clone()).expect("a valid BuildRunStatus");
        assert_eq!(status.image_digest.as_deref(), Some(digest));

        let mut oom = attempt();
        oom.failure = Some("OutOfMemory".into());
        let failed = status_patch(&oom, BuildPhase::Failed, None, now);
        assert_eq!(failed["status"]["conditions"][0]["reason"], "OutOfMemory");
        assert_eq!(failed["status"]["conditions"][0]["status"], "False");
        assert!(failed["status"].get("imageDigest").is_none());

        let started = status_patch(&attempt(), BuildPhase::Preparing, None, now);
        assert_eq!(started["status"]["phase"], "Running");
        assert!(started["status"].get("finishedAt").is_none());
        assert_eq!(started["status"]["startedAt"], "2026-09-17T12:00:00Z");
    }

    #[test]
    fn finished_build_runs_expire_after_a_day() {
        let a = attempt();
        let settings = crate::build::job::tests::settings();
        let mut run = crate::build::job::build_run(&a, &settings);
        let now: Timestamp = "2026-09-18T12:00:01Z".parse().expect("time");
        assert!(!expired(&run, now), "a running build never expires");
        run.status = Some(kuben_crd::BuildRunStatus {
            finished_at: Some("2026-09-17T12:00:00Z".into()),
            ..kuben_crd::BuildRunStatus::default()
        });
        assert!(expired(&run, now));
        let earlier: Timestamp = "2026-09-18T11:59:59Z".parse().expect("time");
        assert!(!expired(&run, earlier));
        assert_eq!(operation_phase(BuildPhase::Cancelled), "cancelled");
        assert_eq!(operation_phase(BuildPhase::Failed), "failed");
    }

    #[test]
    fn api_errors_are_told_apart() {
        let api = |code: u16| {
            kube::Error::Api(
                kube::core::Status {
                    code,
                    ..kube::core::Status::default()
                }
                .boxed(),
            )
        };
        assert!(not_found(&api(404)));
        assert!(conflict(&api(409)));
        assert!(invalid(&api(422)));
        assert!(!invalid(&api(500)));
    }
}

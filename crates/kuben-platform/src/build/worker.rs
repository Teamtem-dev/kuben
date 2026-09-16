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
    while !token.is_cancelled() {
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
        if let Err(e) = self.cleanup(attempt).await {
            return e.into();
        }
        let phase = match attempt.phase {
            BuildPhase::Succeeded => "succeeded",
            BuildPhase::Cancelled => "cancelled",
            _ => "failed",
        };
        let code = attempt
            .failure
            .clone()
            .or_else(|| attempt.deploy_decision.clone());
        Outcome::Done(phase, code)
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
            Ok(_) => Outcome::Done("cancelled", None),
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
                if let Err(e) = self.delete_objects(attempt).await {
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
                self.finish(attempt, "cancelled", None).await
            }
        }
    }

    /// Check `digest` in the registry and complete the attempt.
    async fn verify(
        &self,
        claim: &Claim,
        attempt: &BuildAttempt,
        digest: &kuben_core::artifact::Digest,
    ) -> Outcome {
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
                self.finish(attempt, "succeeded", None).await
            }
            Ok(Completed::Kept { release, decision }) => {
                tracing::info!(build = %attempt.id, %release, %decision, %digest, "build kept without a deploy");
                self.finish(attempt, "succeeded", Some(decision)).await
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
        self.finish(attempt, "failed", Some(code.to_owned())).await
    }

    /// Clean up a final attempt and settle its operation.
    async fn finish(&self, attempt: &BuildAttempt, phase: &'static str, code: Option<String>) -> Outcome {
        match self.cleanup(attempt).await {
            Ok(()) => Outcome::Done(phase, code),
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

    async fn delete_objects(&self, attempt: &BuildAttempt) -> kube::Result<()> {
        let name = job::name(attempt);
        delete(&self.api::<Job>(), &name).await?;
        delete(&self.api::<BuildRun>(), &name).await
    }

    /// Revoke the fetch token and delete the build's objects.
    async fn cleanup(&self, attempt: &BuildAttempt) -> kube::Result<()> {
        self.revoke(attempt).await;
        self.delete_objects(attempt).await?;
        delete(&self.api::<Secret>(), &job::secret_name(attempt)).await
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
        let status = json!({ "status": {
            "phase": crd_phase(phase),
            "jobName": job::name(attempt),
            "imageDigest": digest,
            "startedAt": Timestamp::now().to_string(),
            "conditions": [{
                "type": "Progressing", "status": "True", "reason": phase.as_str(),
            }],
        }});
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

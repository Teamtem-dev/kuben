//! Leader election for the controller role (ADR-023). Every replica serves
//! the API and keeps its own projections, but only the holder of the
//! `coordination.k8s.io/v1` Lease runs the reconcilers.
//!
//! Every write is a compare-and-swap on the Lease's `resourceVersion`, so two
//! candidates can never win the same term. Expiry is judged by how long
//! *this* replica has seen the record unchanged, never by comparing
//! `renewTime` with the local clock, so clock skew between nodes cannot
//! produce two leaders.

use std::{
    future::Future,
    time::{Duration, Instant},
};

use k8s_openapi::{
    api::coordination::v1::{Lease, LeaseSpec},
    apimachinery::pkg::apis::meta::v1::MicroTime,
    jiff::Timestamp,
};
use kube::{
    Api, Client,
    api::{ObjectMeta, PostParams},
};
use tokio_util::sync::CancellationToken;

use crate::health::Health;

/// Name of the Lease guarding the controllers.
pub const LEASE_NAME: &str = "kuben-controller";
/// How long a holder stays leader without renewing (seconds, as stored).
pub const LEASE_SECONDS: i32 = 15;
/// [`LEASE_SECONDS`] as a duration.
pub const LEASE_DURATION: Duration = Duration::from_secs(15);
/// A leader that could not renew for this long steps down, before its lease
/// can expire for the others.
pub const RENEW_DEADLINE: Duration = Duration::from_secs(10);
/// How often candidates retry and the leader renews.
pub const RETRY_PERIOD: Duration = Duration::from_secs(2);

/// Where the Lease lives and who is campaigning.
#[derive(Clone, Debug)]
pub struct Election {
    pub namespace: String,
    /// Unique per process, e.g. `<pod name>_<random>`.
    pub identity: String,
}

/// What a candidate may do with the Lease as it currently is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Decision {
    /// No Lease exists yet.
    Create,
    /// We hold it: extend it.
    Renew,
    /// Free or expired: claim it.
    TakeOver,
    /// Someone else holds a live lease.
    Wait,
}

/// The decision rule. `unchanged_for` is how long this replica has observed
/// the Lease without any change to it.
#[must_use]
pub fn decide(spec: Option<&LeaseSpec>, me: &str, unchanged_for: Duration) -> Decision {
    let Some(spec) = spec else {
        return Decision::Create;
    };
    match spec.holder_identity.as_deref() {
        Some(holder) if holder == me => Decision::Renew,
        None | Some("") => Decision::TakeOver,
        Some(_) => {
            let duration = spec
                .lease_duration_seconds
                .and_then(|s| u64::try_from(s).ok())
                .map_or(LEASE_DURATION, Duration::from_secs);
            if unchanged_for >= duration {
                Decision::TakeOver
            } else {
                Decision::Wait
            }
        }
    }
}

struct Elector {
    api: Api<Lease>,
    identity: String,
    /// `resourceVersion` last seen, and when this replica first saw it.
    observed: Option<(String, Instant)>,
}

impl Elector {
    fn new(client: Client, election: &Election) -> Self {
        Self {
            api: Api::namespaced(client, &election.namespace),
            identity: election.identity.clone(),
            observed: None,
        }
    }

    /// One round of the protocol. `Ok(true)` when we hold the Lease afterwards.
    async fn try_acquire_or_renew(&mut self) -> Result<bool, kube::Error> {
        let current = self.api.get_opt(LEASE_NAME).await?;
        let seen_at = Instant::now();
        let version = current
            .as_ref()
            .and_then(|l| l.metadata.resource_version.clone())
            .unwrap_or_default();
        if self.observed.as_ref().is_none_or(|(v, _)| *v != version) {
            self.observed = Some((version, seen_at));
        }
        let unchanged_for = self
            .observed
            .as_ref()
            .map_or(Duration::ZERO, |(_, t)| seen_at.duration_since(*t));
        let spec = current.as_ref().map(|l| l.spec.clone().unwrap_or_default());
        let now = MicroTime(Timestamp::now());

        let written = match (decide(spec.as_ref(), &self.identity, unchanged_for), current) {
            (Decision::Wait, _) => return Ok(false),
            (Decision::Create, _) => {
                let lease = Lease {
                    metadata: ObjectMeta {
                        name: Some(LEASE_NAME.into()),
                        ..ObjectMeta::default()
                    },
                    spec: Some(LeaseSpec {
                        holder_identity: Some(self.identity.clone()),
                        lease_duration_seconds: Some(LEASE_SECONDS),
                        acquire_time: Some(now.clone()),
                        renew_time: Some(now),
                        lease_transitions: Some(0),
                        ..LeaseSpec::default()
                    }),
                };
                self.api.create(&PostParams::default(), &lease).await
            }
            (decision, Some(mut lease)) => {
                let spec = lease.spec.get_or_insert_with(LeaseSpec::default);
                if decision == Decision::TakeOver {
                    spec.holder_identity = Some(self.identity.clone());
                    spec.acquire_time = Some(now.clone());
                    spec.lease_transitions = Some(spec.lease_transitions.unwrap_or(0).saturating_add(1));
                }
                spec.lease_duration_seconds = Some(LEASE_SECONDS);
                spec.renew_time = Some(now);
                // `replace` carries the resourceVersion we read: a concurrent
                // writer makes this a 409 instead of a second leader.
                self.api.replace(LEASE_NAME, &PostParams::default(), &lease).await
            }
            // `decide` only returns Renew/TakeOver for an existing Lease.
            (_, None) => return Ok(false),
        };
        match written {
            Ok(lease) => {
                self.observed = lease.metadata.resource_version.map(|v| (v, Instant::now()));
                Ok(true)
            }
            Err(kube::Error::Api(s)) if s.code == 409 => Ok(false),
            Err(e) => Err(e),
        }
    }

    /// Hand the Lease back so a standby replica takes over at once instead
    /// of waiting for it to expire.
    async fn release(&self) {
        let Ok(Some(mut lease)) = self.api.get_opt(LEASE_NAME).await else {
            return;
        };
        let spec = lease.spec.get_or_insert_with(LeaseSpec::default);
        if spec.holder_identity.as_deref() != Some(self.identity.as_str()) {
            return;
        }
        spec.holder_identity = None;
        spec.lease_duration_seconds = Some(1);
        spec.renew_time = Some(MicroTime(Timestamp::now()));
        match self.api.replace(LEASE_NAME, &PostParams::default(), &lease).await {
            Ok(_) => tracing::info!("released the controller lease"),
            Err(e) => {
                tracing::warn!(error = %e, "could not release the controller lease; it expires on its own")
            }
        }
    }
}

/// Campaign for the Lease and run `work` while holding it.
///
/// Returns `Ok(())` once `token` is cancelled (after releasing the Lease),
/// and an error when leadership is lost or `work` fails, so the supervisor
/// restarts the campaign with backoff. `work` is always stopped before this
/// returns: a replica that is not the leader never reconciles.
pub async fn run_as_leader<F, Fut>(
    client: Client,
    election: &Election,
    health: Health,
    token: CancellationToken,
    work: F,
) -> anyhow::Result<()>
where
    F: FnOnce(CancellationToken) -> Fut,
    Fut: Future<Output = anyhow::Result<()>> + Send + 'static,
{
    let mut elector = Elector::new(client, election);
    health.standby("controllers");
    tracing::info!(
        identity = %election.identity,
        namespace = %election.namespace,
        lease = LEASE_NAME,
        "waiting for the controller lease"
    );
    loop {
        match elector.try_acquire_or_renew().await {
            Ok(true) => break,
            Ok(false) => health.standby("controllers"),
            // E.g. missing RBAC for leases: surface it in /healthz/details.
            Err(e) => {
                health.degraded("controllers", &format!("leader election: {e}"));
                tracing::warn!(error = %e, "leader election: cannot read or write the lease");
            }
        }
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(RETRY_PERIOD) => {}
        }
    }
    tracing::info!(identity = %election.identity, "acquired the controller lease; starting controllers");
    metrics::gauge!("kuben_leader").set(1.0);

    let work_token = token.child_token();
    let mut work = tokio::spawn(work(work_token.clone()));
    let mut renewed = Instant::now();
    let mut tick = tokio::time::interval(RETRY_PERIOD);
    tick.tick().await; // the first tick completes immediately
    let outcome = loop {
        tokio::select! {
            joined = &mut work => break match joined {
                Ok(result) => result,
                Err(join) => Err(anyhow::anyhow!("controllers stopped: {join}")),
            },
            () = token.cancelled() => break Ok(()),
            _ = tick.tick() => match elector.try_acquire_or_renew().await {
                Ok(true) => renewed = Instant::now(),
                Ok(false) => break Err(anyhow::anyhow!("lost the controller lease to another replica")),
                Err(e) if renewed.elapsed() < RENEW_DEADLINE => {
                    tracing::warn!(error = %e, "could not renew the controller lease; retrying");
                }
                Err(e) => break Err(anyhow::anyhow!(
                    "could not renew the controller lease for {}s: {e}",
                    RENEW_DEADLINE.as_secs()
                )),
            },
        }
    };

    work_token.cancel();
    if !work.is_finished() {
        let _ = tokio::time::timeout(Duration::from_secs(10), &mut work).await;
    }
    metrics::gauge!("kuben_leader").set(0.0);
    elector.release().await;
    outcome
}

#[cfg(test)]
mod tests {
    use super::*;

    fn held_by(holder: Option<&str>, seconds: Option<i32>) -> LeaseSpec {
        LeaseSpec {
            holder_identity: holder.map(str::to_owned),
            lease_duration_seconds: seconds,
            ..LeaseSpec::default()
        }
    }

    #[test]
    fn decisions() {
        let me = "pod-a_1";
        let fresh = Duration::from_secs(1);
        let stale = Duration::from_secs(16);

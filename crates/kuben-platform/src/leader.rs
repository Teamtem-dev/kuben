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

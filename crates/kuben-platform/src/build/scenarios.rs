//! Failure injection for builds (M3 exit criteria): the worker's decisions
//! ([`classify`], [`steps::next`], [`steps::plan`]) driven through scripted
//! cluster and registry behaviour, without a cluster.
//!
//! [`Sim`] acts like the worker: it creates the Job on admission, deletes it
//! on a stop (later reads see it gone), verifies reported digests against a
//! fake registry, and queues a new attempt after a lost worker, up to
//! [`MAX_BUILD_ATTEMPTS`]. Every event it records must be legal for the
//! phase it is in, as the database would enforce.

use std::collections::BTreeSet;

use kuben_core::{
    artifact::Digest,
    ops::{
        BuildEvent as E, BuildFailure as F, BuildPhase as P,
        outcome::{ContainerExit, JobObservation, PodObservation, UNSCHEDULABLE_LIMIT_SECS, classify},
    },
};
use kuben_store::repo::MAX_BUILD_ATTEMPTS;
use proptest::prelude::*;

use super::steps::{self, Next, Plan};

const GOOD: &str = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
const FORGED: &str = "sha256:2222222222222222222222222222222222222222222222222222222222222222";

/// One scripted thing that happens between two worker claims.
#[derive(Clone, Debug)]
enum Step {
    /// The Job shows this (while it exists).
    Cluster(JobObservation),
    /// Someone asks the build to stop.
    Cancel,
    /// The registry becomes (un)reachable.
    Registry(bool),
}

#[derive(Debug)]
struct Sim {
    phase: P,
    cancel: bool,
    attempt: u32,
    job_created: bool,
    job_deleted: bool,
    reported: Option<Digest>,
    verified: Option<Digest>,
    failure: Option<F>,
    registry: BTreeSet<String>,
    registry_up: bool,
    /// Every phase the attempts went through, for assertions.
    trail: Vec<P>,
}

impl Sim {
    fn new() -> Self {
        Self {
            phase: P::Queued,
            cancel: false,
            attempt: 1,
            job_created: false,
            job_deleted: false,
            reported: None,
            verified: None,
            failure: None,
            registry: BTreeSet::from([GOOD.to_owned()]),
            registry_up: true,
            trail: vec![P::Queued],
        }
    }

    fn apply(&mut self, events: &[E]) {
        for event in events {
            self.phase = self
                .phase
                .apply(*event)
                .unwrap_or_else(|e| panic!("attempt {}: {e}", self.attempt));
            self.trail.push(self.phase);
        }
    }

    /// One claim of the build operation; true when it settled for good.
    fn claim(&mut self, cluster: &JobObservation) -> bool {
        match steps::next(self.phase, self.cancel) {
            Next::Settle => true,
            Next::CancelQueued => {
                self.apply(&[E::CancelRequested]);
                true
            }
            Next::Admit => {
                self.job_created = true;
                self.apply(&[E::Started]);
                false
            }
            Next::Observe => {
                let seen = if self.job_deleted || !self.job_created {
                    JobObservation::default()
                } else {
                    cluster.clone()
                };
                let plan = steps::plan(self.phase, self.cancel, &classify(&seen), self.reported.as_ref());
                self.carry(plan)
            }
        }
    }

    fn carry(&mut self, plan: Plan) -> bool {
        match plan {
            Plan::Wait(events) => {
                self.apply(&events);
                false
            }
            Plan::Stop(events) => {
                self.job_deleted = true;
                self.apply(&events);
                false
            }
            Plan::Cancelled(events) => {
                self.apply(&events);
                true
            }
            Plan::Verify { events, digest } => {
                self.apply(&events);
                self.reported.get_or_insert(digest.clone());
                if !self.registry_up {
                    return false;
                }
                if self.registry.contains(digest.as_str()) {
                    self.apply(&[E::Verified]);
                    self.verified = Some(digest);
                    true
                } else {
                    self.fail(&[], F::OutputRejected)
                }
            }
            Plan::Fail { events, failure, .. } => self.fail(&events, failure),
        }
    }

    fn fail(&mut self, events: &[E], failure: F) -> bool {
        self.apply(events);
        self.apply(&[E::Failed]);
        self.failure = Some(failure);
        if failure.retryable() && self.attempt < MAX_BUILD_ATTEMPTS {
            // The worker queues the next attempt with the same inputs.
            *self = Self {
                attempt: self.attempt + 1,
                registry: std::mem::take(&mut self.registry),
                registry_up: self.registry_up,
                trail: std::mem::take(&mut self.trail),
                cancel: self.cancel,
                ..Self::new()
            };
            self.trail.push(P::Queued);
            return false;
        }
        true
    }

    /// Run `script`; the last cluster state repeats until the build settles
    /// or `limit` claims pass.
    fn run(mut self, script: &[Step]) -> Self {
        let mut cluster = pending();
        let mut steps = script.iter();
        for _ in 0..200 {
            if let Some(step) = steps.next() {
                match step {
                    Step::Cluster(seen) => cluster = seen.clone(),
                    Step::Cancel => self.cancel = true,
                    Step::Registry(up) => self.registry_up = *up,
                }
            }
            if self.claim(&cluster) {
                return self;
            }
        }
        self
    }
}

fn job(pod: PodObservation) -> JobObservation {
    JobObservation {
        exists: true,
        pod: Some(pod),
        ..JobObservation::default()
    }
}

fn exit(name: &str, code: i32, reason: &str, message: Option<&str>) -> ContainerExit {
    ContainerExit {
        name: name.into(),
        exit_code: code,
        reason: Some(reason.into()),
        message: message.map(Into::into),
    }
}

fn pending() -> JobObservation {
    job(PodObservation {
        phase: "Pending".into(),
        ..PodObservation::default()
    })
}

fn fetching() -> JobObservation {
    job(PodObservation {
        phase: "Pending".into(),
        running: vec!["fetch".into()],
        ..PodObservation::default()
    })
}

fn building() -> JobObservation {
    job(PodObservation {
        phase: "Running".into(),
        exits: vec![
            exit("fetch", 0, "Completed", None),
            exit("plan", 0, "Completed", None),
        ],
        running: vec!["build".into()],
        ..PodObservation::default()
    })
}

fn finished(digest: &str) -> JobObservation {
    let report = format!(r#"{{"digest":"{digest}","strategy":"dockerfile"}}"#);
    JobObservation {
        succeeded: true,
        ..job(PodObservation {
            phase: "Succeeded".into(),
            exits: vec![
                exit("fetch", 0, "Completed", None),
                exit("build", 0, "Completed", Some(&report)),
            ],
            ..PodObservation::default()
        })
    }
}

fn failed_pod(pod: PodObservation) -> JobObservation {
    JobObservation {
        failed: true,
        failed_reason: Some("BackoffLimitExceeded".into()),
        ..job(pod)
    }
}

fn oom() -> JobObservation {
    failed_pod(PodObservation {
        phase: "Failed".into(),
        exits: vec![exit("build", 137, "OOMKilled", None)],
        ..PodObservation::default()
    })
}

fn deadline() -> JobObservation {
    JobObservation {
        exists: true,
        failed: true,
        failed_reason: Some("DeadlineExceeded".into()),
        ..JobObservation::default()
    }
}

fn disk_eviction() -> JobObservation {
    failed_pod(PodObservation {
        phase: "Failed".into(),
        reason: Some("Evicted".into()),
        message: Some("Pod ephemeral local storage usage exceeds the total limit of containers 10Gi.".into()),
        ..PodObservation::default()
    })
}

fn enospc() -> JobObservation {
    failed_pod(PodObservation {
        phase: "Failed".into(),
        exits: vec![exit(
            "build",
            1,
            "Error",
            Some("error: failed to copy: write /home/user/.local/share/buildkit/x: no space left on device"),
        )],
        ..PodObservation::default()
    })
}

fn node_lost() -> JobObservation {
    job(PodObservation {
        phase: "Unknown".into(),
        reason: Some("NodeLost".into()),
        ..PodObservation::default()
    })
}

fn unschedulable() -> JobObservation {
    job(PodObservation {
        phase: "Pending".into(),
        unschedulable_secs: Some(UNSCHEDULABLE_LIMIT_SECS + 1),
        message: Some("0/1 nodes are available: 1 Insufficient memory.".into()),
        ..PodObservation::default()
    })
}

fn source_missing() -> JobObservation {
    failed_pod(PodObservation {
        phase: "Failed".into(),
        exits: vec![exit(
            "fetch",
            128,
            "Error",
            Some("fatal: remote error: upload-pack: not our ref"),
        )],
        ..PodObservation::default()
    })
}

use Step::{Cancel, Cluster, Registry};

#[test]
fn a_build_goes_from_queued_to_a_verified_image() {
    let sim = Sim::new().run(&[
        Cluster(pending()),
        Cluster(fetching()),
        Cluster(building()),
        Cluster(finished(GOOD)),
    ]);
    assert_eq!(sim.phase, P::Succeeded);
    assert_eq!(sim.verified.as_ref().map(Digest::as_str), Some(GOOD));
    for phase in [P::Preparing, P::Running, P::Publishing, P::VerifyingOutput] {
        assert!(sim.trail.contains(&phase), "{phase:?} in {:?}", sim.trail);
    }
}

#[test]
fn out_of_memory_fails_the_build_without_a_retry() {
    let sim = Sim::new().run(&[Cluster(building()), Cluster(oom())]);
    assert_eq!(
        (sim.phase, sim.failure, sim.attempt),
        (P::Failed, Some(F::OutOfMemory), 1)
    );
}

#[test]
fn the_deadline_fails_the_build() {
    let sim = Sim::new().run(&[Cluster(building()), Cluster(deadline())]);
    assert_eq!((sim.phase, sim.failure), (P::Failed, Some(F::DeadlineExceeded)));
}

#[test]
fn a_full_disk_fails_the_build_by_eviction_or_enospc() {
    for full in [disk_eviction(), enospc()] {
        let sim = Sim::new().run(&[Cluster(building()), Cluster(full)]);
        assert_eq!((sim.phase, sim.failure), (P::Failed, Some(F::DiskFull)));
    }
}

#[test]
fn an_unreadable_commit_fails_at_fetch() {
    let sim = Sim::new().run(&[Cluster(fetching()), Cluster(source_missing())]);
    assert_eq!((sim.phase, sim.failure), (P::Failed, Some(F::SourceUnavailable)));
}

#[test]
fn a_build_no_node_can_take_fails_as_unschedulable() {
    let sim = Sim::new().run(&[Cluster(pending()), Cluster(unschedulable())]);
    assert_eq!((sim.phase, sim.failure), (P::Failed, Some(F::Unschedulable)));
}

#[test]
fn cancelling_a_queued_build_needs_no_job() {
    let sim = Sim::new().run(&[Cancel]);
    assert_eq!(sim.phase, P::Cancelled);
    assert!(!sim.job_created, "nothing ran");
}

#[test]
fn cancelling_a_running_build_deletes_its_job_first() {
    let sim = Sim::new().run(&[
        Cluster(building()),
        Cluster(building()),
        Cancel,
        Cluster(building()),
    ]);
    assert_eq!(sim.phase, P::Cancelled);
    assert!(sim.job_deleted);
    assert!(sim.trail.contains(&P::Cancelling), "{:?}", sim.trail);
    assert_eq!(sim.failure, None, "a stop is not a failure");
}

#[test]
fn a_job_that_dies_while_being_stopped_is_cancelled() {
    // The kill of the stop shows up as an OOM-looking exit before the Job is gone.
    let mut sim = Sim::new().run(&[Cluster(building()), Cluster(building())]);
    sim.cancel = true;
    let plan = steps::plan(sim.phase, true, &classify(&oom()), None);
    assert!(sim.carry(plan), "settled");
    assert_eq!((sim.phase, sim.failure), (P::Cancelled, None));
}

#[test]
fn output_pushed_before_the_stop_is_kept() {
    let sim = Sim::new().run(&[
        Cluster(building()),
        Cluster(building()),
        Cluster(finished(GOOD)),
        Cancel,
    ]);
    // The worker saw the finished Job on the same claim the stop arrived.
    assert!(
        matches!(sim.phase, P::Succeeded | P::Cancelled),
        "{:?}",
        sim.phase
    );
    let mut racing = Sim::new().run(&[Cluster(building()), Cluster(building())]);
    racing.cancel = true;
    let plan = steps::plan(racing.phase, true, &classify(&finished(GOOD)), None);
    assert!(racing.carry(plan));
    assert_eq!(
        racing.phase,
        P::Succeeded,
        "the artifact exists; the epoch decides the deploy"
    );
}

#[test]
fn a_lost_node_is_retried_as_a_new_attempt() {
    let sim = Sim::new().run(&[
        Cluster(building()),
        Cluster(node_lost()),
        Cluster(pending()),
        Cluster(building()),
        Cluster(finished(GOOD)),
    ]);
    assert_eq!((sim.phase, sim.attempt), (P::Succeeded, 2));
    assert!(sim.trail.contains(&P::Failed), "the first attempt failed");
}

#[test]
fn lost_workers_are_retried_a_bounded_number_of_times() {
    let sim = Sim::new().run(&[Cluster(node_lost())]);
    assert_eq!(sim.phase, P::Failed);
    assert_eq!(sim.attempt, MAX_BUILD_ATTEMPTS);
    assert_eq!(sim.failure, Some(F::LostWorker));
}

#[test]
fn a_job_that_vanishes_mid_build_is_a_lost_worker() {
    let mut sim = Sim::new().run(&[Cluster(building()), Cluster(building())]);
    sim.job_deleted = true; // someone deleted it by hand
    let plan = steps::plan(sim.phase, false, &classify(&JobObservation::default()), None);
    assert!(!sim.carry(plan), "a retry was queued");
    assert_eq!((sim.phase, sim.attempt), (P::Queued, 2));
}

#[test]
fn a_digest_the_registry_does_not_hold_is_rejected() {
    let sim = Sim::new().run(&[Cluster(building()), Cluster(finished(FORGED))]);
    assert_eq!((sim.phase, sim.failure), (P::Failed, Some(F::OutputRejected)));
    assert_eq!(sim.verified, None);
}

#[test]
fn an_unreachable_registry_delays_verification_only() {
    let sim = Sim::new().run(&[
        Registry(false),
        Cluster(building()),
        Cluster(finished(GOOD)),
        Cluster(JobObservation::default()), // the Job was cleaned up meanwhile
        Cluster(JobObservation::default()),
        Registry(true),
    ]);
    assert_eq!(sim.phase, P::Succeeded, "{:?}", sim.trail);
    assert_eq!(sim.reported.as_ref().map(Digest::as_str), Some(GOOD));
}

fn observation() -> impl Strategy<Value = Step> {
    prop_oneof![
        4 => Just(Cluster(pending())),
        4 => Just(Cluster(fetching())),
        6 => Just(Cluster(building())),
        3 => Just(Cluster(finished(GOOD))),
        2 => Just(Cluster(finished(FORGED))),
        1 => Just(Cluster(oom())),
        1 => Just(Cluster(deadline())),
        1 => Just(Cluster(disk_eviction())),
        1 => Just(Cluster(node_lost())),
        1 => Just(Cluster(JobObservation::default())),
        1 => Just(Cancel),
        1 => Just(Registry(false)),
        2 => Just(Registry(true)),
    ]
}

proptest! {
    /// Whatever the cluster and registry do, every recorded event is legal,
    /// a build succeeds only with a digest the registry holds, is cancelled
    /// only when asked, and retries stay bounded.
    #[test]
    fn builds_stay_consistent_under_any_failure(script in prop::collection::vec(observation(), 1..60)) {
        let asked = script.iter().any(|s| matches!(s, Cancel));
        let sim = Sim::new().run(&script);
        prop_assert!(sim.attempt <= MAX_BUILD_ATTEMPTS);
        if sim.phase == P::Succeeded {
            prop_assert_eq!(sim.verified.as_ref().map(Digest::as_str), Some(GOOD));
        } else {
            prop_assert!(sim.verified.is_none());
        }
        if sim.phase == P::Cancelled {
            prop_assert!(asked);
        }
        if sim.phase == P::Failed {
            prop_assert!(sim.failure.is_some());
        }
    }
}

//! What the build worker does next, decided without I/O.
//!
//! [`next`] picks the kind of work for an attempt's phase; [`plan`] turns
//! what the Job shows into the events to record, in order, and what follows
//! them. The events are the ones [`BuildPhase::apply`] accepts on the way, so
//! a worker that crashes between two of them resumes from the recorded phase.

use kuben_core::{
    artifact::Digest,
    ops::{BuildEvent as E, BuildFailure, BuildPhase as P, JobVerdict},
};

/// The kind of work an attempt needs.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Next {
    /// The attempt is final: settle its operation and clean up.
    Settle,
    /// Nothing runs yet and a stop was asked: cancel without a Job.
    CancelQueued,
    /// Take a build slot and create the Job.
    Admit,
    /// Read the Job.
    Observe,
}

/// The work for an attempt in `phase`.
#[must_use]
pub const fn next(phase: P, cancel_requested: bool) -> Next {
    match phase {
        P::Succeeded | P::Failed | P::Cancelled => Next::Settle,
        P::Queued | P::Blocked if cancel_requested => Next::CancelQueued,
        P::Queued | P::Blocked => Next::Admit,
        _ => Next::Observe,
    }
}

/// What follows the events of a [`Plan`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Plan {
    /// Record the events and look again later.
    Wait(Vec<E>),
    /// Record the events, then check `digest` in the registry.
    Verify { events: Vec<E>, digest: Digest },
    /// Record the events, then fail with `failure`.
    Fail {
        events: Vec<E>,
        failure: BuildFailure,
        detail: String,
    },
    /// Record the events, delete the build's objects and look again later.
    Stop(Vec<E>),
    /// Record the events: the attempt is cancelled.
    Cancelled(Vec<E>),
}

/// The events that move a running attempt to `Cancelling`.
fn to_cancelling(phase: P) -> Vec<E> {
    match phase {
        P::Preparing | P::Running | P::Publishing | P::VerifyingOutput => {
            vec![E::CancelRequested, E::Stopping]
        }
        P::CancelRequested => vec![E::Stopping],
        _ => Vec::new(),
    }
}

/// The events that bring `phase` up to `VerifyingOutput`.
fn to_verifying(phase: P) -> Vec<E> {
    match phase {
        P::Preparing => vec![E::Building, E::Publishing, E::Published],
        P::Running => vec![E::Publishing, E::Published],
        P::Publishing => vec![E::Published],
        _ => Vec::new(),
    }
}

/// The plan for an attempt in the observing `phase`, given what its Job
/// shows and the digest it reported earlier.
#[must_use]
pub fn plan(phase: P, cancel_requested: bool, verdict: &JobVerdict, reported: Option<&Digest>) -> Plan {
    if phase == P::VerifyingOutput && !cancel_requested {
        return reported.map_or_else(
            || Plan::Fail {
                events: Vec::new(),
                failure: BuildFailure::OutputRejected,
                detail: "no digest was reported before verification".into(),
            },
            |digest| Plan::Verify {
                events: Vec::new(),
                digest: digest.clone(),
            },
        );
    }
    if cancel_requested || matches!(phase, P::CancelRequested | P::Cancelling) {
        return plan_stop(phase, verdict, reported);
    }
    match verdict {
        JobVerdict::Building if phase == P::Preparing => Plan::Wait(vec![E::Building]),
        JobVerdict::Pending | JobVerdict::Fetching | JobVerdict::Building => Plan::Wait(Vec::new()),
        JobVerdict::Finished(report) => Plan::Verify {
            events: to_verifying(phase),
            digest: report.digest.clone(),
        },
        JobVerdict::Failed { failure, detail } => Plan::Fail {
            events: Vec::new(),
            failure: *failure,
            detail: detail.clone(),
        },
        JobVerdict::Gone => Plan::Fail {
            events: Vec::new(),
            failure: BuildFailure::LostWorker,
            detail: "the build Job disappeared".into(),
        },
    }
}

/// A stop was asked while the attempt may hold a Job. An output that was
/// already finished is still verified: the artifact exists (ADR-028).
fn plan_stop(phase: P, verdict: &JobVerdict, reported: Option<&Digest>) -> Plan {
    let finished = match verdict {
        JobVerdict::Finished(report) => Some(&report.digest),
        _ => reported.filter(|_| phase == P::VerifyingOutput),
    };
    if let Some(digest) = finished {
        let events = match phase {
            P::CancelRequested | P::Cancelling => Vec::new(),
            _ => vec![E::CancelRequested],
        };
        return Plan::Verify {
            events,
            digest: digest.clone(),
        };
    }
    let mut events = to_cancelling(phase);
    match verdict {
        // The Job is gone, or ended on its own while being stopped.
        JobVerdict::Gone | JobVerdict::Failed { .. } => {
            events.push(E::Stopped);
            Plan::Cancelled(events)
        }
        _ => Plan::Stop(events),
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::ops::outcome::BuildReport;

    use super::{Plan::Wait, *};

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn digest() -> Digest {
        DIGEST.parse().expect("digest")
    }

    fn finished() -> JobVerdict {
        JobVerdict::Finished(BuildReport {
            digest: digest(),
            strategy: "dockerfile".into(),
        })
    }

    fn failed(failure: BuildFailure) -> JobVerdict {
        JobVerdict::Failed {
            failure,
            detail: failure.explain().into(),
        }
    }

    /// Apply `events` from `phase`; every event must be legal.
    fn replay(phase: P, events: &[E]) -> P {
        events
            .iter()
            .fold(phase, |p, e| p.apply(*e).unwrap_or_else(|err| panic!("{err}")))
    }

    const OBSERVED: [P; 6] = [
        P::Preparing,
        P::Running,
        P::Publishing,
        P::VerifyingOutput,
        P::CancelRequested,
        P::Cancelling,
    ];

    #[test]
    fn work_follows_the_phase() {
        assert_eq!(next(P::Queued, false), Next::Admit);
        assert_eq!(next(P::Blocked, false), Next::Admit);
        assert_eq!(next(P::Blocked, true), Next::CancelQueued);
        assert_eq!(next(P::Running, true), Next::Observe);
        for p in [P::Succeeded, P::Failed, P::Cancelled] {
            assert_eq!(next(p, true), Next::Settle);
        }
    }

    #[test]
    fn a_finished_job_is_verified_from_any_running_phase() {
        for phase in [P::Preparing, P::Running, P::Publishing] {
            let Plan::Verify { events, digest: d } = plan(phase, false, &finished(), None) else {
                panic!("{phase:?}");
            };
            assert_eq!(replay(phase, &events), P::VerifyingOutput);
            assert_eq!(d, digest());
            assert_eq!(replay(P::VerifyingOutput, &[E::Verified]), P::Succeeded);
        }
    }

    #[test]
    fn verification_resumes_from_the_recorded_digest() {
        let gone = plan(P::VerifyingOutput, false, &JobVerdict::Gone, Some(&digest()));
        assert_eq!(
            gone,
            Plan::Verify {
                events: vec![],
                digest: digest()
            },
            "the Job may be gone after the report"
        );
        assert!(matches!(
            plan(P::VerifyingOutput, false, &JobVerdict::Gone, None),
            Plan::Fail {
                failure: BuildFailure::OutputRejected,
                ..
            }
        ));
    }

    #[test]
    fn progress_only_moves_forward() {
        assert_eq!(
            plan(P::Preparing, false, &JobVerdict::Pending, None),
            Wait(vec![])
        );
        assert_eq!(
            plan(P::Preparing, false, &JobVerdict::Fetching, None),
            Wait(vec![])
        );
        assert_eq!(
            plan(P::Preparing, false, &JobVerdict::Building, None),
            Wait(vec![E::Building])
        );
        assert_eq!(plan(P::Running, false, &JobVerdict::Building, None), Wait(vec![]));
    }

    #[test]
    fn failures_keep_their_class() {
        for failure in [
            BuildFailure::OutOfMemory,
            BuildFailure::DiskFull,
            BuildFailure::DeadlineExceeded,
            BuildFailure::BuildError,
            BuildFailure::SourceUnavailable,
            BuildFailure::Unschedulable,
        ] {
            for phase in [P::Preparing, P::Running, P::Publishing] {
                let Plan::Fail {
                    events, failure: f, ..
                } = plan(phase, false, &failed(failure), None)
                else {
                    panic!("{failure:?} in {phase:?}");
                };
                assert_eq!(f, failure);
                assert_eq!(replay(replay(phase, &events), &[E::Failed]), P::Failed);
            }
        }
        let lost = plan(P::Running, false, &JobVerdict::Gone, None);
        assert!(matches!(
            lost,
            Plan::Fail {
                failure: BuildFailure::LostWorker,
                ..
            }
        ));
    }

    #[test]
    fn a_stop_deletes_the_job_then_waits_for_it_to_go() {
        for phase in [P::Preparing, P::Running, P::Publishing, P::CancelRequested] {
            let Plan::Stop(events) = plan(phase, true, &JobVerdict::Building, None) else {
                panic!("{phase:?}");
            };
            assert_eq!(replay(phase, &events), P::Cancelling, "{phase:?}");
        }
        assert_eq!(
            plan(P::Cancelling, true, &JobVerdict::Building, None),
            Plan::Stop(vec![])
        );
        for phase in OBSERVED.into_iter().filter(|p| *p != P::VerifyingOutput) {
            let Plan::Cancelled(events) = plan(phase, true, &JobVerdict::Gone, None) else {
                panic!("{phase:?}");
            };
            assert_eq!(replay(phase, &events), P::Cancelled, "{phase:?}");
        }
    }

    #[test]
    fn a_job_killed_by_the_stop_is_cancelled_not_failed() {
        let killed = failed(BuildFailure::LostWorker);
        let Plan::Cancelled(events) = plan(P::Cancelling, true, &killed, None) else {
            panic!("not cancelled");
        };
        assert_eq!(replay(P::Cancelling, &events), P::Cancelled);
        let Plan::Cancelled(events) = plan(P::Running, true, &failed(BuildFailure::OutOfMemory), None) else {
            panic!("not cancelled");
        };
        assert_eq!(replay(P::Running, &events), P::Cancelled);
    }

    #[test]
    fn output_finished_before_the_stop_still_succeeds() {
        for phase in [P::Running, P::Publishing, P::CancelRequested, P::Cancelling] {
            let Plan::Verify { events, .. } = plan(phase, true, &finished(), None) else {
                panic!("{phase:?}");
            };
            assert_eq!(
                replay(replay(phase, &events), &[E::Verified]),
                P::Succeeded,
                "{phase:?}"
            );
        }
        let Plan::Verify { events, .. } = plan(P::VerifyingOutput, true, &JobVerdict::Gone, Some(&digest()))
        else {
            panic!("verifying");
        };
        assert_eq!(
            replay(replay(P::VerifyingOutput, &events), &[E::Verified]),
            P::Succeeded
        );
    }

    #[test]
    fn every_plan_is_legal_from_every_observed_phase() {
        let verdicts = [
            JobVerdict::Pending,
            JobVerdict::Fetching,
            JobVerdict::Building,
            finished(),
            failed(BuildFailure::OutOfMemory),
            JobVerdict::Gone,
        ];
        for phase in OBSERVED {
            for cancel in [false, true] {
                for verdict in &verdicts {
                    for reported in [None, Some(digest())] {
                        let events = match plan(phase, cancel, verdict, reported.as_ref()) {
                            Plan::Wait(e) | Plan::Stop(e) | Plan::Cancelled(e) => e,
                            Plan::Verify { events, .. } | Plan::Fail { events, .. } => events,
                        };
                        replay(phase, &events);
                    }
                }
            }
        }
    }
}

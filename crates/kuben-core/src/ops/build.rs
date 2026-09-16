//! `BuildAttempt` phases (ADR-028).
//!
//! ```text
//! Queued ⇄ Blocked
//! Queued → Preparing → Running → Publishing → VerifyingOutput → Succeeded
//!   any active → Failed
//!   Queued/Blocked + cancel → Cancelled            (nothing is running yet)
//!   running phases + cancel → CancelRequested → Cancelling → Cancelled
//! ```
//!
//! Cancellation is a request, not an undo: if the executor reports a verified
//! output before the stop took effect, the attempt is `Succeeded` (the
//! artifact exists; the target's source epoch decides whether it deploys).
//! An infrastructure retry is a new attempt; a terminal attempt never runs
//! again.

use serde::{Deserialize, Serialize};

use super::IllegalTransition;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum BuildPhase {
    Queued,
    /// Admission found no capacity or quota; re-evaluated later.
    Blocked,
    Preparing,
    Running,
    Publishing,
    VerifyingOutput,
    CancelRequested,
    Cancelling,
    Succeeded,
    Failed,
    Cancelled,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum BuildEvent {
    /// Admission cannot place the build now.
    Blocked,
    /// Capacity or quota is available again.
    Unblocked,
    /// The Job exists and the source is being fetched.
    Started,
    /// BuildKit is building.
    Building,
    /// The push to the registry started.
    Publishing,
    /// The push finished; the digest is being checked against the registry.
    Published,
    /// The output manifest was verified in the registry.
    Verified,
    /// User error, OOM, deadline, lost worker, rejected output.
    Failed,
    /// A user, or a newer commit, asked to stop.
    CancelRequested,
    /// The executor saw the request and is deleting the Job.
    Stopping,
    /// The Job is gone.
    Stopped,
}

impl BuildPhase {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Blocked => "blocked",
            Self::Preparing => "preparing",
            Self::Running => "running",
            Self::Publishing => "publishing",
            Self::VerifyingOutput => "verifyingOutput",
            Self::CancelRequested => "cancelRequested",
            Self::Cancelling => "cancelling",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    pub const ALL: [Self; 11] = [
        Self::Queued,
        Self::Blocked,
        Self::Preparing,
        Self::Running,
        Self::Publishing,
        Self::VerifyingOutput,
        Self::CancelRequested,
        Self::Cancelling,
        Self::Succeeded,
        Self::Failed,
        Self::Cancelled,
    ];

    /// The phase named by [`BuildPhase::as_str`], as stored.
    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|p| p.as_str() == s)
    }

    /// No event changes a terminal phase.
    #[must_use]
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Succeeded | Self::Failed | Self::Cancelled)
    }

    /// The attempt may hold a build slot and a Job.
    #[must_use]
    pub const fn holds_slot(self) -> bool {
        matches!(
            self,
            Self::Preparing
                | Self::Running
                | Self::Publishing
                | Self::VerifyingOutput
                | Self::CancelRequested
                | Self::Cancelling
        )
    }

    /// The next phase after `event`, or the rejected event.
    pub fn apply(self, event: BuildEvent) -> Result<Self, IllegalTransition> {
        use BuildEvent as E;
        use BuildPhase as P;
        let next = match (self, event) {
            // admission
            (P::Queued | P::Blocked, E::Blocked) => P::Blocked,
            (P::Blocked, E::Unblocked) => P::Queued,
            // progress
            (P::Queued, E::Started) => P::Preparing,
            (P::Preparing, E::Building) => P::Running,
            (P::Running, E::Publishing) => P::Publishing,
            (P::Publishing, E::Published) => P::VerifyingOutput,
            (P::VerifyingOutput | P::CancelRequested | P::Cancelling, E::Verified) => P::Succeeded,
            // Cancellation: before a Job exists it is immediate; afterwards it is a
            // request until the Job is observed gone. A Job we are killing ends as
            // cancelled, not failed. Late progress reports while stopping change
            // nothing.
            (P::Queued | P::Blocked, E::CancelRequested)
            | (P::CancelRequested | P::Cancelling, E::Stopped)
            | (P::Cancelling, E::Failed) => P::Cancelled,
            (
                P::Preparing | P::Running | P::Publishing | P::VerifyingOutput | P::CancelRequested,
                E::CancelRequested,
            )
            | (P::CancelRequested, E::Started | E::Building | E::Publishing | E::Published) => {
                P::CancelRequested
            }
            (P::CancelRequested, E::Stopping)
            | (
                P::Cancelling,
                E::CancelRequested | E::Stopping | E::Started | E::Building | E::Publishing | E::Published,
            ) => P::Cancelling,
            (
                P::Queued
                | P::Blocked
                | P::Preparing
                | P::Running
                | P::Publishing
                | P::VerifyingOutput
                | P::CancelRequested,
                E::Failed,
            ) => P::Failed,
            _ => {
                return Err(IllegalTransition {
                    machine: "BuildAttempt",
                    from: self.as_str(),
                    event: event.as_str(),
                    terminal: self.is_terminal(),
                });
            }
        };
        Ok(next)
    }
}

impl BuildEvent {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Blocked => "blocked",
            Self::Unblocked => "unblocked",
            Self::Started => "started",
            Self::Building => "building",
            Self::Publishing => "publishing",
            Self::Published => "published",
            Self::Verified => "verified",
            Self::Failed => "failed",
            Self::CancelRequested => "cancelRequested",
            Self::Stopping => "stopping",
            Self::Stopped => "stopped",
        }
    }

    pub const ALL: [Self; 11] = [
        Self::Blocked,
        Self::Unblocked,
        Self::Started,
        Self::Building,
        Self::Publishing,
        Self::Published,
        Self::Verified,
        Self::Failed,
        Self::CancelRequested,
        Self::Stopping,
        Self::Stopped,
    ];
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::{BuildEvent as E, BuildPhase as P, *};

    fn run(events: &[BuildEvent]) -> Result<BuildPhase, IllegalTransition> {
        events.iter().try_fold(P::Queued, |p, e| p.apply(*e))
    }

    #[test]
    fn phases_round_trip_through_their_names() {
        for phase in BuildPhase::ALL {
            assert_eq!(BuildPhase::parse(phase.as_str()), Some(phase));
        }
        assert_eq!(BuildPhase::parse("done"), None);
    }

    #[test]
    fn happy_path_reaches_succeeded() {
        let p = run(&[E::Started, E::Building, E::Publishing, E::Published, E::Verified]);
        assert_eq!(p, Ok(P::Succeeded));
    }

    #[test]
    fn cancel_before_start_needs_no_worker() {
        assert_eq!(run(&[E::CancelRequested]), Ok(P::Cancelled));
        assert_eq!(run(&[E::Blocked, E::CancelRequested]), Ok(P::Cancelled));
    }

    #[test]
    fn cancel_while_running_is_cancelled_only_after_the_job_stops() {
        let p = run(&[E::Started, E::Building, E::CancelRequested]).expect("legal");
        assert_eq!(p, P::CancelRequested);
        assert!(!p.is_terminal(), "not cancelled until termination is observed");
        let p = p.apply(E::Stopping).and_then(|p| p.apply(E::Stopped));
        assert_eq!(p, Ok(P::Cancelled));
    }

    #[test]
    fn output_verified_before_the_stop_is_recorded_as_succeeded() {
        let p = run(&[
            E::Started,
            E::Building,
            E::Publishing,
            E::Published,
            E::CancelRequested,
            E::Verified,
        ]);
        assert_eq!(p, Ok(P::Succeeded));
    }

    #[test]
    fn a_job_killed_by_cancel_is_cancelled_not_failed() {
        let p = run(&[E::Started, E::CancelRequested, E::Stopping, E::Failed]);
        assert_eq!(p, Ok(P::Cancelled));
    }

    #[test]
    fn terminal_attempts_never_run_again() {
        let err = run(&[E::Started, E::Failed, E::Started]).expect_err("terminal");
        assert!(err.terminal);
        assert_eq!(err.from, "failed");
    }

    #[test]
    fn blocked_attempts_do_not_hold_a_slot() {
        assert!(!P::Blocked.holds_slot());
        assert!(!P::Queued.holds_slot());
        assert!(P::Running.holds_slot());
        assert!(P::Cancelling.holds_slot(), "the Job may still be running");
    }

    proptest! {
        /// Whatever the executor reports, a terminal phase is absorbing and an
        /// illegal event leaves the stored phase unchanged.
        #[test]
        fn terminal_phases_are_absorbing(events in prop::collection::vec(prop::sample::select(BuildEvent::ALL.to_vec()), 0..40)) {
            let mut phase = P::Queued;
            let mut reached_terminal: Option<BuildPhase> = None;
            for event in events {
                match phase.apply(event) {
                    Ok(next) => {
                        if let Some(t) = reached_terminal {
                            prop_assert_eq!(next, t, "terminal phase changed");
                        }
                        phase = next;
                    }
                    Err(e) => prop_assert_eq!(e.terminal, phase.is_terminal()),
                }
                if phase.is_terminal() {
                    reached_terminal.get_or_insert(phase);
                }
            }
        }

        /// `Cancelled` is only reachable through a cancel request.
        #[test]
        fn cancelled_requires_a_cancel_request(events in prop::collection::vec(prop::sample::select(BuildEvent::ALL.to_vec()), 0..40)) {
            let mut phase = P::Queued;
            let mut asked = false;
            for event in events {
                asked |= event == E::CancelRequested;
                if let Ok(next) = phase.apply(event) {
                    phase = next;
                }
            }
            if phase == P::Cancelled {
                prop_assert!(asked);
            }
        }
    }
}

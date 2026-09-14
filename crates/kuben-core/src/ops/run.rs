//! `DeploymentRun` phases (ADR-026).
//!
//! ```text
//! Planned → AwaitingApproval → PendingDelivery → AcceptedByCluster
//!         → Preflight ⇄ Blocked → Applying → Verifying → Succeeded
//! any non-final → Superseded            (a newer run owns the target)
//! before delivery + cancel → Cancelled
//! after delivery + cancel → CancelRequested → Cancelled | RecoveryRequested
//! Failed | CancelRequested → RecoveryRequested → Recovering
//!                          → Recovered | RecoveryFailed | ManualActionRequired
//! ```
//!
//! `Superseded` means the run lost the right to decide the target, not that
//! its pods are gone. `Failed` is settled but may still request its one
//! recorded compensation; everything else final is absorbing.

use serde::{Deserialize, Serialize};

use super::IllegalTransition;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum RunPhase {
    Planned,
    AwaitingApproval,
    PendingDelivery,
    AcceptedByCluster,
    Preflight,
    /// A removable condition (capacity, quota) stops progress; re-checked later.
    Blocked,
    Applying,
    Verifying,
    Succeeded,
    Failed,
    Superseded,
    CancelRequested,
    Cancelled,
    RecoveryRequested,
    Recovering,
    Recovered,
    RecoveryFailed,
    ManualActionRequired,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum RunEvent {
    RequireApproval,
    /// The plan is frozen and needs no (more) approval.
    ReadyForDelivery,
    Approved,
    Rejected,
    /// The agent persisted and validated the execution envelope.
    AcceptedByCluster,
    PreflightStarted,
    PreflightPassed,
    Blocked,
    Unblocked,
    Applied,
    Verified,
    Failed,
    /// A newer run took the target (generation CAS).
    Superseded,
    CancelRequested,
    /// Execution stopped; nothing will be compensated.
    Stopped,
    RecoveryRequested,
    RecoveryStarted,
    Recovered,
    RecoveryFailed,
    ManualActionRequired,
}

impl RunPhase {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Planned => "planned",
            Self::AwaitingApproval => "awaitingApproval",
            Self::PendingDelivery => "pendingDelivery",
            Self::AcceptedByCluster => "acceptedByCluster",
            Self::Preflight => "preflight",
            Self::Blocked => "blocked",
            Self::Applying => "applying",
            Self::Verifying => "verifying",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Superseded => "superseded",
            Self::CancelRequested => "cancelRequested",
            Self::Cancelled => "cancelled",
            Self::RecoveryRequested => "recoveryRequested",
            Self::Recovering => "recovering",
            Self::Recovered => "recovered",
            Self::RecoveryFailed => "recoveryFailed",
            Self::ManualActionRequired => "manualActionRequired",
        }
    }

    /// Every phase, in declaration order.
    pub const ALL: [Self; 18] = [
        Self::Planned,
        Self::AwaitingApproval,
        Self::PendingDelivery,
        Self::AcceptedByCluster,
        Self::Preflight,
        Self::Blocked,
        Self::Applying,
        Self::Verifying,
        Self::Succeeded,
        Self::Failed,
        Self::Superseded,
        Self::CancelRequested,
        Self::Cancelled,
        Self::RecoveryRequested,
        Self::Recovering,
        Self::Recovered,
        Self::RecoveryFailed,
        Self::ManualActionRequired,
    ];

    /// The phase named by [`RunPhase::as_str`], as stored.
    #[must_use]
    pub fn parse(s: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|p| p.as_str() == s)
    }

    /// Absorbing phases: no event changes them.
    #[must_use]
    pub const fn is_final(self) -> bool {
        matches!(
            self,
            Self::Succeeded
                | Self::Superseded
                | Self::Cancelled
                | Self::Recovered
                | Self::RecoveryFailed
                | Self::ManualActionRequired
        )
    }

    /// The run may still change the target's resources.
    #[must_use]
    pub const fn may_write(self) -> bool {
        matches!(
            self,
            Self::AcceptedByCluster
                | Self::Preflight
                | Self::Blocked
                | Self::Applying
                | Self::Verifying
                | Self::CancelRequested
                | Self::RecoveryRequested
                | Self::Recovering
        )
    }

    /// Nothing of this run has reached a cluster yet.
    const fn undelivered(self) -> bool {
        matches!(
            self,
            Self::Planned | Self::AwaitingApproval | Self::PendingDelivery
        )
    }

    pub fn apply(self, event: RunEvent) -> Result<Self, IllegalTransition> {
        use RunEvent as E;
        use RunPhase as P;
        let next = match (self, event) {
            // planning and approval
            (P::Planned, E::RequireApproval) => P::AwaitingApproval,
            (P::Planned, E::ReadyForDelivery) | (P::AwaitingApproval, E::Approved) => P::PendingDelivery,
            // delivery and progress
            (P::PendingDelivery, E::AcceptedByCluster) => P::AcceptedByCluster,
            (P::AcceptedByCluster, E::PreflightStarted) | (P::Blocked, E::Unblocked) => P::Preflight,
            (P::Preflight | P::Applying | P::Blocked, E::Blocked) => P::Blocked,
            (P::Preflight, E::PreflightPassed) => P::Applying,
            (P::Applying, E::Applied) => P::Verifying,
            (P::Verifying | P::CancelRequested, E::Verified) => P::Succeeded,
            // a newer run owns the target: every non-final run loses its rights
            (p, E::Superseded) if !p.is_final() => P::Superseded,
            // cancellation
            (p, E::CancelRequested) if p.undelivered() => P::Cancelled,
            (P::AwaitingApproval, E::Rejected) | (P::CancelRequested, E::Stopped) => P::Cancelled,
            (
                P::AcceptedByCluster
                | P::Preflight
                | P::Blocked
                | P::Applying
                | P::Verifying
                | P::CancelRequested,
                E::CancelRequested,
            )
            | (
                P::CancelRequested,
                E::AcceptedByCluster | E::PreflightStarted | E::PreflightPassed | E::Applied,
            ) => P::CancelRequested,
            // failure and recovery
            (
                P::Planned
                | P::AwaitingApproval
                | P::PendingDelivery
                | P::AcceptedByCluster
                | P::Preflight
                | P::Blocked
                | P::Applying
                | P::Verifying
                | P::CancelRequested,
                E::Failed,
            ) => P::Failed,
            (P::Failed | P::CancelRequested, E::RecoveryRequested) => P::RecoveryRequested,
            (P::RecoveryRequested, E::RecoveryStarted) => P::Recovering,
            (P::Recovering, E::Recovered) => P::Recovered,
            (P::Recovering, E::RecoveryFailed) => P::RecoveryFailed,
            (P::RecoveryRequested | P::Recovering, E::ManualActionRequired) => P::ManualActionRequired,
            _ => {
                return Err(IllegalTransition {
                    machine: "DeploymentRun",
                    from: self.as_str(),
                    event: event.as_str(),
                    terminal: self.is_final(),
                });
            }
        };
        Ok(next)
    }
}

impl RunEvent {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::RequireApproval => "requireApproval",
            Self::ReadyForDelivery => "readyForDelivery",
            Self::Approved => "approved",
            Self::Rejected => "rejected",
            Self::AcceptedByCluster => "acceptedByCluster",
            Self::PreflightStarted => "preflightStarted",
            Self::PreflightPassed => "preflightPassed",
            Self::Blocked => "blocked",
            Self::Unblocked => "unblocked",
            Self::Applied => "applied",
            Self::Verified => "verified",
            Self::Failed => "failed",
            Self::Superseded => "superseded",
            Self::CancelRequested => "cancelRequested",
            Self::Stopped => "stopped",
            Self::RecoveryRequested => "recoveryRequested",
            Self::RecoveryStarted => "recoveryStarted",
            Self::Recovered => "recovered",
            Self::RecoveryFailed => "recoveryFailed",
            Self::ManualActionRequired => "manualActionRequired",
        }
    }

    pub const ALL: [Self; 20] = [
        Self::RequireApproval,
        Self::ReadyForDelivery,
        Self::Approved,
        Self::Rejected,
        Self::AcceptedByCluster,
        Self::PreflightStarted,
        Self::PreflightPassed,
        Self::Blocked,
        Self::Unblocked,
        Self::Applied,
        Self::Verified,
        Self::Failed,
        Self::Superseded,
        Self::CancelRequested,
        Self::Stopped,
        Self::RecoveryRequested,
        Self::RecoveryStarted,
        Self::Recovered,
        Self::RecoveryFailed,
        Self::ManualActionRequired,
    ];
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::{RunEvent as E, RunPhase as P, *};

    #[test]
    fn all_holds_every_phase_once_and_parse_inverts_as_str() {
        // Exhaustive: adding a phase fails to compile until it has an index,
        // and the assertions below then fail until `ALL` lists it.
        const fn index(p: RunPhase) -> usize {
            match p {
                P::Planned => 0,
                P::AwaitingApproval => 1,
                P::PendingDelivery => 2,
                P::AcceptedByCluster => 3,
                P::Preflight => 4,
                P::Blocked => 5,
                P::Applying => 6,
                P::Verifying => 7,
                P::Succeeded => 8,
                P::Failed => 9,
                P::Superseded => 10,
                P::CancelRequested => 11,
                P::Cancelled => 12,
                P::RecoveryRequested => 13,
                P::Recovering => 14,
                P::Recovered => 15,
                P::RecoveryFailed => 16,
                P::ManualActionRequired => 17,
            }
        }
        assert_eq!(RunPhase::ALL.len(), index(P::ManualActionRequired) + 1);
        for (i, phase) in RunPhase::ALL.into_iter().enumerate() {
            assert_eq!(index(phase), i);
            assert_eq!(RunPhase::parse(phase.as_str()), Some(phase));
        }
        assert_eq!(RunPhase::parse("finished"), None);
    }

    fn run(events: &[RunEvent]) -> Result<RunPhase, IllegalTransition> {
        events.iter().try_fold(P::Planned, |p, e| p.apply(*e))
    }

    const DELIVERED: [RunEvent; 3] = [E::ReadyForDelivery, E::AcceptedByCluster, E::PreflightStarted];

    #[test]
    fn happy_path_with_approval() {
        let p = run(&[
            E::RequireApproval,
            E::Approved,
            E::AcceptedByCluster,
            E::PreflightStarted,
            E::PreflightPassed,
            E::Applied,
            E::Verified,
        ]);
        assert_eq!(p, Ok(P::Succeeded));
    }

    #[test]
    fn capacity_block_is_removable() {
        let mut events = DELIVERED.to_vec();
        events.extend([
            E::Blocked,
            E::Unblocked,
            E::PreflightPassed,
            E::Applied,
            E::Verified,
        ]);
        assert_eq!(run(&events), Ok(P::Succeeded));
    }

    #[test]
    fn cancel_before_delivery_is_immediate_after_delivery_it_is_a_request() {
        assert_eq!(run(&[E::CancelRequested]), Ok(P::Cancelled));
        let mut events = DELIVERED.to_vec();
        events.push(E::CancelRequested);
        let p = run(&events).expect("legal");
        assert_eq!(p, P::CancelRequested);
        assert!(p.may_write(), "a stop still touches the cluster");
    }

    #[test]
    fn a_failed_run_can_request_its_recovery_once() {
        let mut events = DELIVERED.to_vec();
        events.extend([
            E::PreflightPassed,
            E::Failed,
            E::RecoveryRequested,
            E::RecoveryStarted,
            E::Recovered,
        ]);
        assert_eq!(run(&events), Ok(P::Recovered));
        let again = P::Recovered.apply(E::RecoveryRequested).expect_err("final");
        assert!(again.terminal);
    }

    #[test]
    fn a_superseded_run_can_no_longer_recover_or_write() {
        let mut events = DELIVERED.to_vec();
        events.extend([E::PreflightPassed, E::Failed, E::Superseded]);
        let p = run(&events).expect("legal");
        assert_eq!(p, P::Superseded);
        assert!(!p.may_write());
        assert!(p.apply(E::RecoveryRequested).is_err());
    }

    #[test]
    fn failure_is_not_succeeded_after_recovery() {
        // The outcome of the deploy stays "failed"; the recovery has its own phase.
        let mut events = DELIVERED.to_vec();
        events.extend([E::Failed, E::RecoveryRequested, E::RecoveryStarted, E::Recovered]);
        assert_ne!(run(&events), Ok(P::Succeeded));
    }

    proptest! {
        #[test]
        fn final_phases_are_absorbing(events in prop::collection::vec(prop::sample::select(RunEvent::ALL.to_vec()), 0..50)) {
            let mut phase = P::Planned;
            let mut settled: Option<RunPhase> = None;
            for event in events {
                if let Ok(next) = phase.apply(event) {
                    if let Some(f) = settled {
                        prop_assert_eq!(next, f);
                    }
                    phase = next;
                }
                if phase.is_final() {
                    settled.get_or_insert(phase);
                }
            }
        }

        /// A run only reaches `Succeeded` through `Verified`.
        #[test]
        fn succeeded_requires_verification(events in prop::collection::vec(prop::sample::select(RunEvent::ALL.to_vec()), 0..50)) {
            let mut phase = P::Planned;
            let mut verified = false;
            for event in events {
                if let Ok(next) = phase.apply(event) {
                    if next == P::Succeeded && phase != P::Succeeded {
                        verified = event == E::Verified;
                    }
                    phase = next;
                }
            }
            if phase == P::Succeeded {
                prop_assert!(verified);
            }
        }
    }
}

//! Environment policy and deployment approvals (M4.1; plan §8.2, §13).
//!
//! An environment's policy is an immutable revision: who may start
//! deployments there, how many distinct people must approve a change
//! before it is delivered, who may approve, and how long a request waits.
//! Protection is policy, never inferred from a name. The rules here are
//! pure; the store applies them inside the transaction that locks the run.
//!
//! ```text
//! planned ──(approvals required)──► awaitingApproval ──(enough approvals)──► pendingDelivery
//!                                        │ reject / expiry
//!                                        ▼
//!                                    cancelled
//! ```

use serde::{Deserialize, Serialize};

use crate::perm::{Perm, Role};

/// Most approvals a policy may require.
pub const MAX_APPROVALS: u8 = 5;
/// Shortest and longest time a change may wait for its approvals.
pub const MIN_APPROVAL_TTL_SECS: u32 = 5 * 60;
pub const MAX_APPROVAL_TTL_SECS: u32 = 30 * 24 * 3600;
/// Default wait for approvals: one week.
pub const DEFAULT_APPROVAL_TTL_SECS: u32 = 7 * 24 * 3600;
/// Longest approval comment kept.
pub const MAX_COMMENT_CHARS: usize = 1024;

/// Why a policy was refused.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum PolicyError {
    #[error("at most {MAX_APPROVALS} approvals can be required")]
    TooManyApprovals,
    #[error("the deploy role `{0}` cannot deploy")]
    DeployRole(Role),
    #[error("the approve role `{0}` cannot approve releases")]
    ApproveRole(Role),
    #[error("approvals must wait between {MIN_APPROVAL_TTL_SECS} and {MAX_APPROVAL_TTL_SECS} seconds")]
    ApprovalTtl,
}

/// One revision of an environment's policy.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentPolicy {
    /// Distinct approvers a change needs before delivery; 0 for none.
    pub required_approvals: u8,
    /// The weakest role that may start deployments here.
    pub deploy_role: Role,
    /// The weakest role that may approve deployments here.
    pub approve_role: Role,
    /// How long a change waits for its approvals before it is cancelled.
    pub approval_ttl_secs: u32,
}

/// What a deployment run changes, as far as approval is concerned.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ChangeKind {
    Deploy,
    Rollback,
    Promotion,
    /// An automatic deploy of a verified build.
    Build,
    /// The same release and configuration, pods replaced.
    Restart,
    /// The same release and configuration, delivered by the agent.
    Handover,
    /// The same release and configuration with a secret's new revision: new
    /// values reach production like any other change.
    Rotation,
}

impl EnvironmentPolicy {
    /// No approvals; developers deploy.
    #[must_use]
    pub const fn open() -> Self {
        Self {
            required_approvals: 0,
            deploy_role: Role::Developer,
            approve_role: Role::Admin,
            approval_ttl_secs: DEFAULT_APPROVAL_TTL_SECS,
        }
    }

    /// One approval by an admin who did not ask for the change.
    #[must_use]
    pub const fn production() -> Self {
        Self {
            required_approvals: 1,
            ..Self::open()
        }
    }

    /// The first policy of a new environment.
    #[must_use]
    pub const fn initial(production: bool) -> Self {
        if production {
            Self::production()
        } else {
            Self::open()
        }
    }

    pub fn validate(&self) -> Result<(), PolicyError> {
        if self.required_approvals > MAX_APPROVALS {
            return Err(PolicyError::TooManyApprovals);
        }
        if !self.deploy_role.grants(Perm::AppDeploy) {
            return Err(PolicyError::DeployRole(self.deploy_role));
        }
        if !self.approve_role.grants(Perm::ReleaseApprove) {
            return Err(PolicyError::ApproveRole(self.approve_role));
        }
        if !(MIN_APPROVAL_TTL_SECS..=MAX_APPROVAL_TTL_SECS).contains(&self.approval_ttl_secs) {
            return Err(PolicyError::ApprovalTtl);
        }
        Ok(())
    }

    /// Approvals a change of `kind` needs. Restarts and handovers change
    /// neither the release nor the configuration.
    #[must_use]
    pub const fn approvals_for(&self, kind: ChangeKind) -> u8 {
        match kind {
            ChangeKind::Restart | ChangeKind::Handover => 0,
            _ => self.required_approvals,
        }
    }

    /// Whether a caller whose strongest role on the environment is `role`
    /// may start a deployment there.
    #[must_use]
    pub fn may_deploy(&self, role: Option<Role>) -> bool {
        role.is_some_and(|r| r.rank() >= self.deploy_role.rank() && r.grants(Perm::AppDeploy))
    }

    /// Whether a caller whose strongest role on the environment is `role`
    /// may approve there.
    #[must_use]
    pub fn may_approve(&self, role: Option<Role>) -> bool {
        role.is_some_and(|r| r.rank() >= self.approve_role.rank() && r.grants(Perm::ReleaseApprove))
    }

    /// Whether `self` protects less than `current` in any respect. Weakening
    /// production protection is an owner's decision.
    #[must_use]
    pub const fn weakens(&self, current: &Self) -> bool {
        self.required_approvals < current.required_approvals
            || self.deploy_role.rank() < current.deploy_role.rank()
            || self.approve_role.rank() < current.approve_role.rank()
            || self.approval_ttl_secs > current.approval_ttl_secs
    }
}

impl Default for EnvironmentPolicy {
    fn default() -> Self {
        Self::open()
    }
}

/// An approver's answer.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Decision {
    Approve,
    Reject,
}

impl Decision {
    /// The stored name of the decision.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Approve => "approved",
            Self::Reject => "rejected",
        }
    }
}

/// Why a decision was refused. Nothing is recorded for a refusal.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum ApprovalError {
    #[error("the deployment is not waiting for approval")]
    NotAwaiting,
    #[error("the approval window of the deployment has closed")]
    Expired,
    #[error("whoever asked for a deployment cannot approve it")]
    SelfApproval,
    #[error("this approver has already decided on the deployment")]
    AlreadyDecided,
    #[error("the deployment changed since it was shown: reload and decide again")]
    StalePlan,
}

/// What a run waiting for approval looks like to a decision.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pending<'a> {
    /// `kind:principal` of whoever asked for the run.
    pub requested_by: &'a str,
    pub required: u8,
    /// Distinct approvals recorded so far.
    pub approved: u8,
    pub expires_at: i64,
    pub plan_hash: &'a [u8],
}

/// The outcome of a valid decision.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tally {
    /// More approvals are needed.
    Waiting { remaining: u8 },
    /// Enough approvals: the run may be delivered.
    Approved,
    /// One rejection cancels the run.
    Rejected,
}

/// The principal part of `kind:principal`.
#[must_use]
pub fn principal(actor: &str) -> &str {
    actor.split_once(':').map_or(actor, |(_, id)| id)
}

/// Decide whether `approver` (a user id) may record `decision` on `run` at
/// `now`, having seen `plan_hash`, and what the run becomes.
///
/// # Errors
///
/// The [`ApprovalError`] that refuses the decision.
pub fn decide(
    run: &Pending<'_>,
    approver: &str,
    decided_before: bool,
    decision: Decision,
    plan_hash: &[u8],
    now: i64,
) -> Result<Tally, ApprovalError> {
    if now >= run.expires_at {
        return Err(ApprovalError::Expired);
    }
    // A requester may withdraw nothing and approve nothing: only others decide.
    if principal(run.requested_by) == approver {
        return Err(ApprovalError::SelfApproval);
    }
    if decided_before {
        return Err(ApprovalError::AlreadyDecided);
    }
    if plan_hash != run.plan_hash {
        return Err(ApprovalError::StalePlan);
    }
    Ok(match decision {
        Decision::Reject => Tally::Rejected,
        Decision::Approve => {
            let count = run.approved.saturating_add(1);
            if count >= run.required {
                Tally::Approved
            } else {
                Tally::Waiting {
                    remaining: run.required - count,
                }
            }
        }
    })
}

/// Hex of a plan hash, as the API shows it.
#[must_use]
pub fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
            let _ = write!(s, "{b:02x}");
            s
        })
}

/// A plan hash given as hex; `None` when it is not hex.
#[must_use]
pub fn unhex(text: &str) -> Option<Vec<u8>> {
    if !text.len().is_multiple_of(2) || text.len() > 128 {
        return None;
    }
    (0..text.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(text.get(i..i + 2)?, 16).ok())
        .collect()
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::*;

    const HASH: &[u8] = &[1, 2, 3];

    fn pending(required: u8, approved: u8) -> Pending<'static> {
        Pending {
            requested_by: "user:alice",
            required,
            approved,
            expires_at: 1_000,
            plan_hash: HASH,
        }
    }

    #[test]
    fn production_needs_one_admin_approval() {
        let p = EnvironmentPolicy::initial(true);
        assert_eq!(p.required_approvals, 1);
        assert_eq!(p.approve_role, Role::Admin);
        assert_eq!(EnvironmentPolicy::initial(false).required_approvals, 0);
        p.validate().expect("valid");
        EnvironmentPolicy::default().validate().expect("valid");
    }

    #[test]
    fn invalid_policies_are_refused() {
        let base = EnvironmentPolicy::production();
        let cases = [
            (
                EnvironmentPolicy {
                    required_approvals: 6,
                    ..base
                },
                PolicyError::TooManyApprovals,
            ),
            (
                EnvironmentPolicy {
                    deploy_role: Role::Viewer,
                    ..base
                },
                PolicyError::DeployRole(Role::Viewer),
            ),
            (
                EnvironmentPolicy {
                    approve_role: Role::Developer,
                    ..base
                },
                PolicyError::ApproveRole(Role::Developer),
            ),
            (
                EnvironmentPolicy {
                    approval_ttl_secs: 10,
                    ..base
                },
                PolicyError::ApprovalTtl,
            ),
            (
                EnvironmentPolicy {
                    approval_ttl_secs: MAX_APPROVAL_TTL_SECS + 1,
                    ..base
                },
                PolicyError::ApprovalTtl,
            ),
        ];
        for (policy, error) in cases {
            assert_eq!(policy.validate(), Err(error), "{policy:?}");
        }
    }

    #[test]
    fn restarts_and_handovers_need_no_approval() {
        let p = EnvironmentPolicy {
            required_approvals: 2,
            ..EnvironmentPolicy::production()
        };
        for kind in [
            ChangeKind::Deploy,
            ChangeKind::Rollback,
            ChangeKind::Promotion,
            ChangeKind::Build,
            ChangeKind::Rotation,
        ] {
            assert_eq!(p.approvals_for(kind), 2, "{kind:?}");
        }
        assert_eq!(p.approvals_for(ChangeKind::Restart), 0);
        assert_eq!(p.approvals_for(ChangeKind::Handover), 0);
        assert_eq!(EnvironmentPolicy::open().approvals_for(ChangeKind::Deploy), 0);
    }

    #[test]
    fn roles_are_checked_against_the_policy() {
        let p = EnvironmentPolicy {
            deploy_role: Role::Admin,
            ..EnvironmentPolicy::production()
        };
        assert!(!p.may_deploy(None));
        assert!(!p.may_deploy(Some(Role::Developer)));
        assert!(p.may_deploy(Some(Role::Admin)));
        assert!(p.may_approve(Some(Role::Owner)));
        assert!(!p.may_approve(Some(Role::Developer)));
        assert!(!p.may_approve(Some(Role::Viewer)));
    }

    #[test]
    fn weakening_is_detected_field_by_field() {
        let p = EnvironmentPolicy::production();
        assert!(!p.weakens(&p));
        assert!(EnvironmentPolicy::open().weakens(&p));
        assert!(
            !EnvironmentPolicy {
                required_approvals: 2,
                ..p
            }
            .weakens(&p)
        );
        assert!(
            EnvironmentPolicy {
                approve_role: Role::Admin,
                ..p
            }
            .weakens(&EnvironmentPolicy {
                approve_role: Role::Owner,
                ..p
            })
        );
        assert!(
            EnvironmentPolicy {
                approval_ttl_secs: p.approval_ttl_secs + 1,
                ..p
            }
            .weakens(&p)
        );
        assert!(
            !EnvironmentPolicy {
                deploy_role: Role::Owner,
                ..p
            }
            .weakens(&p)
        );
    }

    #[test]
    fn requesters_never_approve_their_own_change() {
        let run = pending(1, 0);
        assert_eq!(
            decide(&run, "alice", false, Decision::Approve, HASH, 0),
            Err(ApprovalError::SelfApproval)
        );
        assert_eq!(
            decide(&run, "alice", false, Decision::Reject, HASH, 0),
            Err(ApprovalError::SelfApproval),
            "nor reject it: withdrawing is cancellation"
        );
        let by_token = Pending {
            requested_by: "token:alice",
            ..run
        };
        assert_eq!(
            decide(&by_token, "alice", false, Decision::Approve, HASH, 0),
            Err(ApprovalError::SelfApproval),
            "a token acts for its owner"
        );
    }

    #[test]
    fn decisions_need_the_shown_plan_and_an_open_window() {
        let run = pending(1, 0);
        assert_eq!(
            decide(&run, "bob", false, Decision::Approve, &[9], 0),
            Err(ApprovalError::StalePlan)
        );
        assert_eq!(
            decide(&run, "bob", false, Decision::Approve, HASH, 1_000),
            Err(ApprovalError::Expired)
        );
        assert_eq!(
            decide(&run, "bob", true, Decision::Approve, HASH, 0),
            Err(ApprovalError::AlreadyDecided)
        );
        assert_eq!(
            decide(&run, "bob", false, Decision::Approve, HASH, 999),
            Ok(Tally::Approved)
        );
    }

    #[test]
    fn approvals_are_counted_and_one_rejection_cancels() {
        let run = pending(3, 1);
        assert_eq!(
            decide(&run, "bob", false, Decision::Approve, HASH, 0),
            Ok(Tally::Waiting { remaining: 1 })
        );
        assert_eq!(
            decide(&pending(3, 2), "carol", false, Decision::Approve, HASH, 0),
            Ok(Tally::Approved)
        );
        assert_eq!(
            decide(&pending(3, 2), "carol", false, Decision::Reject, HASH, 0),
            Ok(Tally::Rejected)
        );
    }

    #[test]
    fn plan_hashes_round_trip_as_hex() {
        assert_eq!(hex(&[0, 171, 255]), "00abff");
        assert_eq!(unhex("00abff"), Some(vec![0, 171, 255]));
        assert_eq!(unhex("00ABFF"), Some(vec![0, 171, 255]));
        assert_eq!(unhex("abc"), None);
        assert_eq!(unhex("zz"), None);
        assert_eq!(unhex(&"a".repeat(130)), None);
        assert_eq!(principal("user:u1"), "u1");
        assert_eq!(principal("plain"), "plain");
    }

    proptest! {
        /// However many approvers vote, a run is approved exactly when the
        /// required number of distinct non-requesters approved and nobody
        /// rejected first.
        #[test]
        fn a_run_is_approved_only_by_enough_other_people(
            required in 1u8..=MAX_APPROVALS,
            votes in prop::collection::vec((0usize..6, any::<bool>()), 0..20),
        ) {
            let people = ["alice", "bob", "carol", "dave", "erin", "frank"];
            let mut decided = std::collections::BTreeSet::new();
            let mut approved = 0u8;
            let mut outcome = None;
            for (who, approve) in votes {
                if outcome.is_some() {
                    break;
                }
                let run = Pending { approved, ..pending(required, 0) };
                let decision = if approve { Decision::Approve } else { Decision::Reject };
                match decide(&run, people[who], decided.contains(&who), decision, HASH, 0) {
                    Ok(Tally::Waiting { remaining }) => {
                        decided.insert(who);
                        approved += 1;
                        prop_assert_eq!(remaining, required - approved);
                    }
                    Ok(done) => {
                        decided.insert(who);
                        if done == Tally::Approved {
                            approved += 1;
                        }
                        outcome = Some(done);
                    }
                    Err(e) => prop_assert!(matches!(e, ApprovalError::SelfApproval | ApprovalError::AlreadyDecided)),
                }
            }
            prop_assert!(!decided.contains(&0), "alice asked for the change");
            if outcome == Some(Tally::Approved) {
                prop_assert_eq!(approved, required);
            }
            prop_assert!(approved <= required);
        }
    }
}

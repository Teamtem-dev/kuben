//! The application target's control state: the monotonic desired generation,
//! the source epoch and the deploy policy (ADR-026).
//!
//! Only these methods may raise the generation. A late build (older source
//! epoch), a stale request (older expected generation), a pinned target or a
//! recreated target (different lifecycle UID) cannot move it:
//!
//! ```text
//! A accepted: epoch 41 → build A
//! B observed: epoch 42 → build B
//! B finishes → CAS epoch=42 → generation 90
//! A finishes → CAS epoch=41 fails → artifact kept, run Superseded, no deploy
//! ```
//!
//! In the product these checks run inside one SQL transaction with the
//! target row locked, so the in-memory model here is the specification the
//! store implements.

use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// Monotonic desired-state counter of one target.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(transparent)]
pub struct Generation(pub u64);

/// Counter of source heads observed for a source binding. A force-push is a
/// new head too; commit timestamps never decide order.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(transparent)]
pub struct SourceEpoch(pub u64);

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub enum DeployPolicy {
    /// Successful builds of the current source head deploy themselves.
    #[default]
    Auto,
    /// Builds produce releases; a human or API call deploys them.
    Manual,
    /// Held on a chosen release (after a rollback) until resumed explicitly.
    Pinned,
}

/// Why a change to the target was refused.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum Reject {
    #[error("the target was deleted and recreated (lifecycle mismatch)")]
    LifecycleMismatch,
    #[error("the target is being deleted")]
    Deleting,
    #[error("a newer source head exists (epoch {current}, request {requested})")]
    StaleSource { current: u64, requested: u64 },
    #[error("the build configuration changed since the build started")]
    BuildConfigChanged,
    #[error("the target moved on (generation {current}, expected {expected})")]
    GenerationMoved { current: u64, expected: u64 },
    #[error("automatic deploys are off for this target ({0:?})")]
    NotAutomatic(DeployPolicy),
    #[error("the generation counter is exhausted")]
    Exhausted,
}

/// An automatic deploy proposed by a finished build.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct AutodeployRequest {
    pub lifecycle_uid: Uuid,
    pub source_epoch: SourceEpoch,
    pub build_config_revision: u64,
    pub expected_generation: Generation,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetState {
    pub lifecycle_uid: Uuid,
    pub deleting: bool,
    pub desired_generation: Generation,
    pub source_epoch: SourceEpoch,
    pub build_config_revision: u64,
    pub policy: DeployPolicy,
}

impl TargetState {
    #[must_use]
    pub fn new(lifecycle_uid: Uuid) -> Self {
        Self {
            lifecycle_uid,
            deleting: false,
            desired_generation: Generation(0),
            source_epoch: SourceEpoch(0),
            build_config_revision: 0,
            policy: DeployPolicy::Auto,
        }
    }

    /// A new head was read from the provider (not merely a webhook arriving).
    pub fn observe_source_head(&mut self) -> SourceEpoch {
        self.source_epoch = SourceEpoch(self.source_epoch.0.saturating_add(1));
        self.source_epoch
    }

    /// The build configuration changed; builds of the old configuration may
    /// no longer deploy automatically.
    pub fn change_build_config(&mut self) {
        self.build_config_revision = self.build_config_revision.saturating_add(1);
    }

    /// Compare-and-set for a build's automatic deploy.
    pub fn try_autodeploy(&mut self, req: AutodeployRequest) -> Result<Generation, Reject> {
        self.guard(req.lifecycle_uid, req.expected_generation)?;
        if self.policy != DeployPolicy::Auto {
            return Err(Reject::NotAutomatic(self.policy));
        }
        if req.source_epoch != self.source_epoch {
            return Err(Reject::StaleSource {
                current: self.source_epoch.0,
                requested: req.source_epoch.0,
            });
        }
        if req.build_config_revision != self.build_config_revision {
            return Err(Reject::BuildConfigChanged);
        }
        self.bump()
    }

    /// An explicit deploy or promotion chosen by a person or an API client.
    /// Works under any policy; it does not unpin.
    pub fn deploy_explicit(
        &mut self,
        lifecycle_uid: Uuid,
        expected: Generation,
    ) -> Result<Generation, Reject> {
        self.guard(lifecycle_uid, expected)?;
        self.bump()
    }

    /// A rollback is a new generation that pins the target, so late webhooks
    /// and builds cannot move it off the chosen release.
    pub fn rollback(&mut self, lifecycle_uid: Uuid, expected: Generation) -> Result<Generation, Reject> {
        self.guard(lifecycle_uid, expected)?;
        let generation = self.bump()?;
        self.policy = DeployPolicy::Pinned;
        Ok(generation)
    }

    /// Explicitly resume automatic deploys after a pin.
    pub fn resume_auto(&mut self, lifecycle_uid: Uuid) -> Result<(), Reject> {
        if lifecycle_uid != self.lifecycle_uid {
            return Err(Reject::LifecycleMismatch);
        }
        self.policy = DeployPolicy::Auto;
        Ok(())
    }

    pub fn begin_deletion(&mut self) {
        self.deleting = true;
    }

    fn guard(&self, lifecycle_uid: Uuid, expected: Generation) -> Result<(), Reject> {
        if lifecycle_uid != self.lifecycle_uid {
            return Err(Reject::LifecycleMismatch);
        }
        if self.deleting {
            return Err(Reject::Deleting);
        }
        if expected != self.desired_generation {
            return Err(Reject::GenerationMoved {
                current: self.desired_generation.0,
                expected: expected.0,
            });
        }
        Ok(())
    }

    fn bump(&mut self) -> Result<Generation, Reject> {
        let next = self
            .desired_generation
            .0
            .checked_add(1)
            .ok_or(Reject::Exhausted)?;
        self.desired_generation = Generation(next);
        Ok(self.desired_generation)
    }
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::*;

    fn target() -> TargetState {
        TargetState::new(Uuid::from_u128(7))
    }

    fn autodeploy(t: &TargetState, epoch: SourceEpoch) -> AutodeployRequest {
        AutodeployRequest {
            lifecycle_uid: t.lifecycle_uid,
            source_epoch: epoch,
            build_config_revision: t.build_config_revision,
            expected_generation: t.desired_generation,
        }
    }

    #[test]
    fn a_late_build_of_an_older_head_cannot_move_the_target() {
        let mut t = target();
        let a = t.observe_source_head(); // epoch 1 → build A
        let b = t.observe_source_head(); // epoch 2 → build B
        let gen_b = t.try_autodeploy(autodeploy(&t, b)).expect("B deploys");
        let late_a = t.try_autodeploy(autodeploy(&t, a));
        assert_eq!(
            late_a,
            Err(Reject::StaleSource {
                current: 2,
                requested: 1
            })
        );
        assert_eq!(t.desired_generation, gen_b);
    }

    #[test]
    fn a_stale_expected_generation_is_rejected() {
        let mut t = target();
        let head = t.observe_source_head();
        let stale = autodeploy(&t, head);
        t.deploy_explicit(t.lifecycle_uid, t.desired_generation)
            .expect("manual deploy");
        assert!(matches!(
            t.try_autodeploy(stale),
            Err(Reject::GenerationMoved { .. })
        ));
    }

    #[test]
    fn rollback_pins_until_resumed() {
        let mut t = target();
        let head = t.observe_source_head();
        t.rollback(t.lifecycle_uid, t.desired_generation)
            .expect("rollback");
        assert_eq!(
            t.try_autodeploy(autodeploy(&t, head)),
            Err(Reject::NotAutomatic(DeployPolicy::Pinned))
        );
        t.resume_auto(t.lifecycle_uid).expect("resume");
        assert!(t.try_autodeploy(autodeploy(&t, head)).is_ok());
    }

    #[test]
    fn a_recreated_target_ignores_work_for_the_old_one() {
        let mut old = target();
        let head = old.observe_source_head();
        let request = autodeploy(&old, head);
        let mut recreated = TargetState::new(Uuid::from_u128(8));
        recreated.observe_source_head();
        assert_eq!(recreated.try_autodeploy(request), Err(Reject::LifecycleMismatch));
    }

    #[test]
    fn a_deleting_target_accepts_nothing() {
        let mut t = target();
        let head = t.observe_source_head();
        t.begin_deletion();
        assert_eq!(t.try_autodeploy(autodeploy(&t, head)), Err(Reject::Deleting));
        assert_eq!(
            t.rollback(t.lifecycle_uid, t.desired_generation),
            Err(Reject::Deleting)
        );
    }

    #[test]
    fn a_build_of_the_old_configuration_does_not_autodeploy() {
        let mut t = target();
        let head = t.observe_source_head();
        let request = autodeploy(&t, head);
        t.change_build_config();
        assert_eq!(t.try_autodeploy(request), Err(Reject::BuildConfigChanged));
    }

    #[derive(Clone, Debug)]
    enum Op {
        ObserveHead,
        ChangeConfig,
        /// A build finishing for the head `lag` epochs behind the current one.
        BuildFinished {
            lag: u64,
        },
        Explicit {
            stale: bool,
        },
        Rollback {
            stale: bool,
        },
        Resume,
        Delete,
    }

    fn op() -> impl Strategy<Value = Op> {
        prop_oneof![
            3 => Just(Op::ObserveHead),
            1 => Just(Op::ChangeConfig),
            5 => (0u64..3).prop_map(|lag| Op::BuildFinished { lag }),
            2 => any::<bool>().prop_map(|stale| Op::Explicit { stale }),
            1 => any::<bool>().prop_map(|stale| Op::Rollback { stale }),
            1 => Just(Op::Resume),
            1 => Just(Op::Delete),
        ]
    }

    proptest! {
        /// Generations never decrease; an automatic deploy is accepted only for
        /// the current head, configuration and expected generation of a live,
        /// unpinned target.
        #[test]
        fn only_current_work_moves_the_target(ops in prop::collection::vec(op(), 0..80)) {
            let mut t = target();
            for op in ops {
                let before = t.clone();
                // The epoch a finished build carries (lag saturates at the first head).
                let mut build_epoch = None;
                let result = match op {
                    Op::ObserveHead => { t.observe_source_head(); Ok(t.desired_generation) }
                    Op::ChangeConfig => { t.change_build_config(); Ok(t.desired_generation) }
                    Op::BuildFinished { lag } => {
                        let epoch = SourceEpoch(t.source_epoch.0.saturating_sub(lag));
                        build_epoch = Some(epoch);
                        t.try_autodeploy(autodeploy(&t, epoch))
                    }
                    Op::Explicit { stale } => {
                        let expected = if stale { Generation(t.desired_generation.0.wrapping_sub(1)) } else { t.desired_generation };
                        t.deploy_explicit(t.lifecycle_uid, expected)
                    }
                    Op::Rollback { stale } => {
                        let expected = if stale { Generation(t.desired_generation.0.wrapping_sub(1)) } else { t.desired_generation };
                        t.rollback(t.lifecycle_uid, expected)
                    }
                    Op::Resume => t.resume_auto(t.lifecycle_uid).map(|()| t.desired_generation),
                    Op::Delete => { t.begin_deletion(); Ok(t.desired_generation) }
                };

                prop_assert!(t.desired_generation >= before.desired_generation, "generation decreased");
                if t.desired_generation != before.desired_generation {
                    prop_assert!(result.is_ok());
                    prop_assert_eq!(t.desired_generation.0, before.desired_generation.0 + 1, "one step per change");
                    prop_assert!(!before.deleting, "a deleting target moved");
                }
                if let (Some(epoch), Ok(_)) = (build_epoch, &result) {
                    prop_assert_eq!(epoch, before.source_epoch, "a stale build deployed");
                    prop_assert_eq!(before.policy, DeployPolicy::Auto, "a pinned or manual target auto-deployed");
                }
                if result.is_err() {
                    prop_assert_eq!(t.desired_generation, before.desired_generation, "a rejected change moved the target");
                }
            }
        }
    }
}

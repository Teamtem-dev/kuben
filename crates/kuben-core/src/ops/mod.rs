//! Durable operation state machines (ADR-026, ADR-027).
//!
//! Every long-running operation is a record with a typed phase. The rules for
//! moving between phases live here as pure functions, so they are unit- and
//! property-tested without a database or a cluster. Controllers persist the
//! phase that [`BuildPhase::apply`], [`RunPhase::apply`] or
//! [`TargetState`]'s methods return; nothing else may write it (I-19).

pub mod build;
pub mod outcome;
pub mod run;
pub mod target;

pub use build::{BuildEvent, BuildPhase};
pub use outcome::{BuildFailure, JobVerdict};
pub use run::{RunEvent, RunPhase};
pub use target::{AutodeployRequest, DeployPolicy, Generation, Reject, SourceEpoch, TargetState};

/// An event that the current phase does not accept. Returned unchanged to the
/// caller, which records it as evidence instead of guessing a phase.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{machine}: event `{event}` is not allowed in phase `{from}`{}", if *.terminal { " (terminal)" } else { "" })]
pub struct IllegalTransition {
    pub machine: &'static str,
    pub from: &'static str,
    pub event: &'static str,
    /// The phase is final; no event can change it.
    pub terminal: bool,
}

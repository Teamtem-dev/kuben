//! The materializer (ADR-032): SQL is the only desired-state writer, and the
//! existing resources are its output.
//!
//! * [`render`] — the `Project`, `Environment` and `App` objects of SQL rows.
//! * [`fence`] — when a run may write its target's App object.
//! * [`write`] — conditional writes: a `resourceVersion` precondition, or a
//!   create that fails when the object appeared meanwhile.
//! * [`progress`] — the App controller's status as run progress.
//! * [`worker`] — claims operations and carries deployment runs from
//!   `planned` to `succeeded` or `failed`.
//! * [`lifecycle`] — writes projects and environments as soon as they exist,
//!   and removes what is being deleted.
//! * [`drift`] — reports changes someone else made to a materialized App
//!   object and writes it again.

pub mod agent;
pub mod drift;
pub mod fence;
pub mod lifecycle;
pub mod progress;
pub mod render;
pub mod worker;
pub mod write;

pub use agent::AgentDispatch;
pub use worker::{Error, Worker, run};

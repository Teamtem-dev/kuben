//! The materializer (ADR-032): SQL is the only desired-state writer, and the
//! existing resources are its output.
//!
//! * [`render`] — the `Project`, `Environment` and `App` objects of an
//!   accepted deployment run, from SQL alone.
//! * [`fence`] — when a run may write its target's App object.
//! * [`write`] — conditional writes: a `resourceVersion` precondition, or a
//!   create that fails when the object appeared meanwhile.
//! * [`progress`] — the App controller's status as run progress.
//! * [`worker`] — claims `deployment` operations and carries their runs from
//!   `planned` to `succeeded` or `failed`.

pub mod fence;
pub mod progress;
pub mod render;
pub mod worker;
pub mod write;

pub use worker::{Error, Worker, run};

//! The materializer (ADR-032): SQL is the only desired-state writer, and the
//! existing resources are its output.
//!
//! * [`render`] — the `Project`, `Environment` and `App` objects of an
//!   accepted deployment run, from SQL alone.
//! * [`fence`] — when a run may write its target's App object.

pub mod fence;
pub mod render;

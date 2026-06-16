//! Kubernetes platform layer.
//!
//! * [`registry`] — immutable set of cluster clients (Invariant I-11: no
//!   global mutable "current context").
//! * [`projection`] — informers feed small, UI-shaped read models instead of
//!   caching raw objects (ADR-004).
//! * [`controller`] — reconcilers (server-side apply, level-triggered).

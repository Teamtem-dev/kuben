//! Kubernetes platform layer.
//!
//! * [`registry`] — immutable set of cluster clients (Invariant I-11: no
//!   global mutable "current context").
//! * [`projection`] — informers feed small, UI-shaped read models instead of
//!   caching raw objects (ADR-004).
//! * [`controller`] — reconcilers (server-side apply, level-triggered).
//! * [`discovery`] — what the cluster can do (Gateway API, cert-manager,
//!   metrics); features are gated on it (ADR-031).
//! * [`doctor`] — why an app is or is not reachable, check by check.
//! * [`materializer`] — writes the resources of accepted deployment runs
//!   from SQL, the only desired-state writer (ADR-032).
//! * [`build`] — Git sources and isolated builds (ADR-028).
//! * [`secrets`] — managed secret values sealed at rest (ADR-030).
//! * [`usage`] — bounded CPU and memory of apps from the Metrics API.
//! * [`supervise`] / [`health`] — every long-running task is supervised and
//!   reports into a health registry; a panic in one subsystem never takes
//!   down the API.

pub mod agentlink;
pub mod build;
pub mod controller;
pub mod discovery;
pub mod doctor;
pub mod duration;
pub mod evidence;
pub mod health;
pub mod leader;
pub mod local_agent;
pub mod materializer;
pub mod projection;
pub mod registry;
pub mod render;
pub mod secrets;
pub mod supervise;
pub mod usage;

#[cfg(feature = "activator")]
pub mod activator;

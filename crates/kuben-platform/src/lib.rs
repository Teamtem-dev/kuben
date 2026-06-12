//! Kubernetes platform layer.
//!
//! * [`registry`] — immutable set of cluster clients (Invariant I-11: no
//!   global mutable "current context").

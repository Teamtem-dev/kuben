//! REST handlers. Every mutating handler requires an `AuthzProof`
//! (Invariant I-1) and is recorded by the audit middleware (scenario 2).

pub mod apps;
pub mod audit;
pub mod environments;
pub mod health;

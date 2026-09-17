//! REST handlers. Every mutating handler requires an `AuthzProof`
//! (Invariant I-1) and is recorded by the audit middleware (scenario 2).

pub mod access;
pub mod apps;
pub mod audit;
pub mod ci;
pub mod environments;
pub mod git;
pub mod health;
pub mod members;
pub mod policy;
pub mod projects;
pub mod registries;
pub mod request;
pub mod scope;
pub mod secrets;
pub mod templates;
pub mod tokens;
pub mod validate;

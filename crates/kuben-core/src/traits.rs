//! Core traits. Phase 0 ships a static implementation for each; later phases
//! swap in OIDC, Cedar, Lease-based leader election, S3 blob storage, etc.

use async_trait::async_trait;

use crate::{
    Error, Result,
    authz::{AuthzProof, ScopeChain, ScopeRef},
    ids::UserId,
    perm::{Perm, Role},
};

/// A resolved role binding: `role` on `scope`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Binding {
    pub scope: ScopeRef,
    pub role: Role,
}

/// The authenticated principal with its resolved bindings.
#[derive(Clone, Debug)]
pub struct Subject {
    pub user: UserId,
    pub bindings: Vec<Binding>,
}

/// Decides whether a subject may perform an action. The only way to obtain an
/// [`AuthzProof`] is through [`PolicyEngine::check`].
pub trait PolicyEngine: Send + Sync {
    fn allowed(&self, subject: &Subject, perm: Perm, chain: &ScopeChain) -> bool;


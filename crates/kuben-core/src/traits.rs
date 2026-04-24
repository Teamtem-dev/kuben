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

    fn check(&self, subject: &Subject, perm: Perm, chain: &ScopeChain) -> Result<AuthzProof> {
        if self.allowed(subject, perm, chain) {
            Ok(AuthzProof::new(subject.user, perm, chain.leaf()))
        } else {
            Err(Error::Forbidden)
        }
    }
}

/// Role-based policy: a binding on any ancestor of the target scope applies.
#[derive(Clone, Copy, Debug, Default)]
pub struct StaticPolicy;

impl PolicyEngine for StaticPolicy {
    fn allowed(&self, subject: &Subject, perm: Perm, chain: &ScopeChain) -> bool {
        subject
            .bindings
            .iter()
            .any(|b| chain.contains(&b.scope) && b.role.grants(perm))
    }
}

/// Leader election abstraction. `--roles=all` uses [`NoopLeader`]; HA mode
/// uses a Kubernetes Lease (phase 3).
#[async_trait]
pub trait LeaderElector: Send + Sync {
    /// Resolves once this instance becomes leader; returns a guard that is
    /// held while leadership is retained.
    async fn acquire(&self) -> Result<LeaderGuard>;
    fn is_leader(&self) -> bool;
}

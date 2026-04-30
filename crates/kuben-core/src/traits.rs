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

/// RAII guard held while leading. Dropping it releases leadership.
#[derive(Debug)]
pub struct LeaderGuard {
    _priv: (),
}

/// Always-leader implementation for single-replica deployments.
#[derive(Clone, Copy, Debug, Default)]
pub struct NoopLeader;

#[async_trait]
impl LeaderElector for NoopLeader {
    async fn acquire(&self) -> Result<LeaderGuard> {
        Ok(LeaderGuard { _priv: () })
    }

    fn is_leader(&self) -> bool {
        true
    }
}

#[cfg(test)]
mod tests {
    use uuid::Uuid;

    use super::*;
    use crate::ids::OrgId;

    fn subject(role: Role, scope: ScopeRef) -> Subject {
        Subject {
            user: UserId::new(),
            bindings: vec![Binding { scope, role }],
        }
    }

    #[test]
    fn org_binding_applies_to_nested_project() {
        let org = OrgId::new();
        let project = Uuid::now_v7();
        let s = subject(Role::Developer, ScopeRef::Org(org));
        let chain = ScopeChain::project(org, project);
        let proof = StaticPolicy.check(&s, Perm::AppDeploy, &chain).expect("allowed");
        assert_eq!(proof.scope(), &ScopeRef::Project(project));
    }

    #[test]
    fn binding_on_other_org_is_rejected() {
        let s = subject(Role::Owner, ScopeRef::Org(OrgId::new()));
        let chain = ScopeChain::org(OrgId::new());
        assert!(matches!(
            StaticPolicy.check(&s, Perm::OrgRead, &chain),
            Err(Error::Forbidden)
        ));
    }

    #[test]
    fn viewer_cannot_exec() {
        let org = OrgId::new();
        let s = subject(Role::Viewer, ScopeRef::Org(org));
        assert!(!StaticPolicy.allowed(&s, Perm::AppExec, &ScopeChain::org(org)));
    }
}

//! Authorization primitives.
//!
//! **Invariant I-1:** no subscription (log/terminal/event stream) and no
//! mutation may be executed without an [`AuthzProof`]. The proof can only be
//! minted by a [`crate::traits::PolicyEngine`], which lives in this crate, so
//! the type system enforces the check.

use uuid::Uuid;

use crate::{ids::OrgId, ids::UserId, perm::Perm};

/// A single node in the resource hierarchy.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum ScopeRef {
    Org(OrgId),
    Project(Uuid),
    Environment(Uuid),
    App(Uuid),
}

/// The full ancestry of the resource being accessed: `Org → Project →
/// Environment → App`. A role binding on any ancestor applies to the leaf.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ScopeChain {
    pub org: OrgId,
    pub project: Option<Uuid>,
    pub environment: Option<Uuid>,
    pub app: Option<Uuid>,
}

impl ScopeChain {
    #[must_use]
    pub const fn org(org: OrgId) -> Self {
        Self {
            org,
            project: None,
            environment: None,
            app: None,
        }
    }

    #[must_use]
    pub const fn project(org: OrgId, project: Uuid) -> Self {
        Self {
            org,
            project: Some(project),
            environment: None,
            app: None,
        }
    }

    /// Whether `scope` is one of the nodes on this chain.
    #[must_use]
    pub fn contains(&self, scope: &ScopeRef) -> bool {
        match scope {
            ScopeRef::Org(o) => *o == self.org,
            ScopeRef::Project(p) => self.project == Some(*p),
            ScopeRef::Environment(e) => self.environment == Some(*e),
            ScopeRef::App(a) => self.app == Some(*a),
        }
    }

    /// The most specific node of the chain.
    #[must_use]
    pub fn leaf(&self) -> ScopeRef {
        if let Some(a) = self.app {
            ScopeRef::App(a)
        } else if let Some(e) = self.environment {
            ScopeRef::Environment(e)
        } else if let Some(p) = self.project {
            ScopeRef::Project(p)
        } else {
            ScopeRef::Org(self.org)
        }
    }
}

/// Proof that `user` holds `perm` on `scope`. Construct only via
/// [`crate::traits::PolicyEngine::check`].
#[derive(Clone, Debug)]
pub struct AuthzProof {
    user: UserId,
    perm: Perm,
    scope: ScopeRef,
}

impl AuthzProof {
    pub(crate) const fn new(user: UserId, perm: Perm, scope: ScopeRef) -> Self {

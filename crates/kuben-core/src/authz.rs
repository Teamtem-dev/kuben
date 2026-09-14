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
    /// Other names of nodes on this chain: the Kubernetes UIDs of resources
    /// whose SQL rows now name them (ADR-032), so role bindings made on those
    /// UIDs keep applying until the importer rewrites them.
    pub aliases: Vec<ScopeRef>,
}

impl ScopeChain {
    #[must_use]
    pub const fn org(org: OrgId) -> Self {
        Self {
            org,
            project: None,
            environment: None,
            app: None,
            aliases: Vec::new(),
        }
    }

    #[must_use]
    pub const fn project(org: OrgId, project: Uuid) -> Self {
        Self {
            org,
            project: Some(project),
            environment: None,
            app: None,
            aliases: Vec::new(),
        }
    }

    /// Whether `scope` is one of the nodes on this chain, by its name of
    /// record or by an alias of the same level.
    #[must_use]
    pub fn contains(&self, scope: &ScopeRef) -> bool {
        let named = match scope {
            ScopeRef::Org(o) => *o == self.org,
            ScopeRef::Project(p) => self.project == Some(*p),
            ScopeRef::Environment(e) => self.environment == Some(*e),
            ScopeRef::App(a) => self.app == Some(*a),
        };
        named || self.aliases.contains(scope)
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
        Self { user, perm, scope }
    }

    #[must_use]
    pub const fn user(&self) -> &UserId {
        &self.user
    }

    #[must_use]
    pub const fn perm(&self) -> Perm {
        self.perm
    }

    #[must_use]
    pub const fn scope(&self) -> &ScopeRef {
        &self.scope
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn aliases_keep_bindings_on_imported_resources_applying() {
        let org = OrgId::new();
        let (sql, legacy) = (Uuid::now_v7(), Uuid::now_v7());
        let chain = ScopeChain {
            aliases: vec![ScopeRef::Project(legacy)],
            ..ScopeChain::project(org, sql)
        };
        assert!(chain.contains(&ScopeRef::Project(sql)));
        assert!(chain.contains(&ScopeRef::Project(legacy)));
        assert!(!chain.contains(&ScopeRef::Project(Uuid::now_v7())));
        assert!(
            !chain.contains(&ScopeRef::Environment(legacy)),
            "an alias keeps its level"
        );
        assert_eq!(
            chain.leaf(),
            ScopeRef::Project(sql),
            "the SQL id is the name of record"
        );
    }
}

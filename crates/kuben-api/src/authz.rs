//! Authorization extractor. Resolves the current user's role bindings into a
//! [`Subject`] and exposes `require(perm, chain) -> AuthzProof`. For API
//! tokens the bindings are restricted to the token's org, capped at its role
//! and optionally re-rooted at its project or environment (scenario 3).

use axum::{extract::FromRequestParts, http::request::Parts};
use kuben_core::{
    Error,
    authz::{AuthzProof, ScopeChain, ScopeRef},
    ids::OrgId,
    model::{ScopeKind, TokenScope},
    perm::{Perm, Role},
    traits::{Binding, Subject},
};

use crate::{auth::CurrentUser, error::ApiError, state::ApiState};

#[derive(Clone, Debug)]
pub struct Authz {
    pub current: CurrentUser,
    pub subject: Subject,
    orgs: Vec<OrgId>,
}

impl Authz {
    pub fn require(&self, state: &ApiState, perm: Perm, chain: &ScopeChain) -> Result<AuthzProof, ApiError> {
        state.policy.check(&self.subject, perm, chain).map_err(ApiError)
    }

    /// The orgs this subject belongs to (for a token: exactly its own org).
    #[must_use]
    pub fn org_ids(&self) -> Vec<OrgId> {
        self.orgs.clone()
    }

    /// Strongest org-level role in `org` (`None` for scoped tokens).
    #[must_use]
    pub fn org_role(&self, org: OrgId) -> Option<Role> {
        self.subject
            .bindings
            .iter()
            .filter(|b| b.scope == ScopeRef::Org(org))
            .map(|b| b.role)
            .max_by_key(|r| r.rank())
    }

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

    /// Account management is session-only: a leaked token must not be able to
    /// mint more tokens, add members or change passwords.
    pub fn forbid_token(&self) -> Result<(), ApiError> {
        if self.current.token.is_some() {
            Err(ApiError(Error::Forbidden))
        } else {
            Ok(())
        }
    }
}

/// Effective bindings of an API token: every role is capped at the token's
/// role; a project/environment scope re-roots the owner's org-level authority
/// at that node, and bindings below org level are dropped.
#[must_use]
pub fn restrict(bindings: Vec<Binding>, scope: &TokenScope) -> Vec<Binding> {
    let narrowed = scope
        .environment
        .map(ScopeRef::Environment)
        .or_else(|| scope.project.map(ScopeRef::Project));
    bindings
        .into_iter()
        .filter_map(|b| {
            let role = b.role.weaker(scope.role);
            match (&narrowed, &b.scope) {
                (None, _) => Some(Binding { scope: b.scope, role }),
                (Some(node), ScopeRef::Org(_)) => Some(Binding {
                    scope: node.clone(),
                    role,
                }),
                (Some(_), _) => None,
            }
        })
        .collect()
}

impl FromRequestParts<ApiState> for Authz {
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, state: &ApiState) -> Result<Self, Self::Rejection> {
        let current = parts
            .extensions
            .get::<CurrentUser>()
            .cloned()

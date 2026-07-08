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
            .ok_or(ApiError(Error::Unauthorized))?;
        // Invited users must replace their temporary password (scenario 4)
        // before any authorized action; `/me` and `/me/password` do not use
        // this extractor.
        if current.user.must_change_password {
            return Err(ApiError(Error::Forbidden));
        }
        let mut rows = state.store.bindings_for_user(current.user.id).await?;
        if let Some(grant) = &current.token {
            rows.retain(|b| b.org_id == grant.org);
        }
        let mut orgs: Vec<OrgId> = rows.iter().map(|b| b.org_id).collect();
        orgs.sort();
        orgs.dedup();
        let mut bindings: Vec<Binding> = rows
            .into_iter()
            .filter_map(|b| {
                let scope = match (b.scope_kind, b.scope_uid.as_deref().and_then(|u| u.parse().ok())) {
                    (ScopeKind::Org, _) => ScopeRef::Org(b.org_id),
                    (ScopeKind::Project, Some(uid)) => ScopeRef::Project(uid),
                    (ScopeKind::Environment, Some(uid)) => ScopeRef::Environment(uid),
                    (ScopeKind::App, Some(uid)) => ScopeRef::App(uid),
                    _ => return None,
                };
                Some(Binding { scope, role: b.role })
            })
            .collect();
        if let Some(grant) = &current.token {
            bindings = restrict(bindings, &grant.scope);
        }
        Ok(Self {
            subject: Subject {
                user: current.user.id,
                bindings,
            },
            current,
            orgs,
        })
    }
}

#[cfg(test)]
mod tests {
    use kuben_core::traits::{PolicyEngine, StaticPolicy};
    use uuid::Uuid;

    use super::*;

    #[test]
    fn token_scope_caps_and_re_roots_authority() {
        let org = OrgId::new();
        let (project, other) = (Uuid::now_v7(), Uuid::now_v7());
        let owner = vec![Binding {
            scope: ScopeRef::Org(org),
            role: Role::Owner,
        }];
        let subject = |bindings| Subject {
            user: kuben_core::ids::UserId::new(),
            bindings,
        };

        let viewer = restrict(
            owner.clone(),
            &TokenScope {
                role: Role::Viewer,
                project: None,
                environment: None,
            },
        );
        let s = subject(viewer);
        assert!(StaticPolicy.allowed(&s, Perm::AppRead, &ScopeChain::project(org, project)));
        assert!(!StaticPolicy.allowed(&s, Perm::AppDeploy, &ScopeChain::project(org, project)));

        let scoped = restrict(
            owner,
            &TokenScope {
                role: Role::Developer,
                project: Some(project),
                environment: None,
            },
        );
        let s = subject(scoped);
        assert!(StaticPolicy.allowed(&s, Perm::AppDeploy, &ScopeChain::project(org, project)));
        assert!(!StaticPolicy.allowed(&s, Perm::AppDeploy, &ScopeChain::project(org, other)));
        assert!(
            !StaticPolicy.allowed(&s, Perm::OrgRead, &ScopeChain::org(org)),
            "a project token has no org-wide authority"
        );
    }
}

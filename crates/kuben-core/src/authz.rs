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

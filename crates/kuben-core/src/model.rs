//! Persistent identity/audit models (owned by SQL, see ADR-015).

use serde::{Deserialize, Serialize};

use crate::{
    ids::{AuditId, OrgId, TokenId, UserId},
    perm::Role,
};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Organization {
    pub id: OrgId,
    pub slug: String,
    pub name: String,
    pub created_at: i64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct User {
    pub id: UserId,
    pub email: String,
    pub display_name: Option<String>,
    pub is_active: bool,
    /// Set for invited users until they replace their temporary password.
    pub must_change_password: bool,

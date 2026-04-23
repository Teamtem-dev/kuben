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


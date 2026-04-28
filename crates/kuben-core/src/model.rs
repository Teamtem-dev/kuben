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
    pub created_at: i64,
}

/// A user together with its password hash (PHC string). Never serialized.
#[derive(Clone, Debug)]
pub struct UserCredentials {
    pub user: User,
    pub password_hash: Option<String>,
}

#[derive(Clone, Debug)]
pub struct Session {
    pub user_id: UserId,
    pub created_at: i64,
    pub expires_at: i64,
    pub last_seen_at: Option<i64>,
    pub revoked_at: Option<i64>,
}

impl Session {
    #[must_use]
    pub fn is_valid_at(&self, now_ms: i64) -> bool {
        self.revoked_at.is_none() && self.expires_at > now_ms
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RoleBinding {
    pub org_id: OrgId,
    pub subject_kind: SubjectKind,
    pub subject_id: String,
    pub role: Role,
    pub scope_kind: ScopeKind,
    pub scope_uid: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum SubjectKind {
    User,
    Team,
    Token,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ScopeKind {
    Org,
    Project,
    Environment,
    App,
}

impl SubjectKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::Team => "team",
            Self::Token => "token",
        }
    }
}

impl ScopeKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Org => "org",
            Self::Project => "project",
            Self::Environment => "environment",
            Self::App => "app",
        }
    }
}

/// Append-only audit record.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AuditEvent {
    /// Monotonic insertion order (pagination cursor).
    pub seq: i64,
    pub id: AuditId,
    pub org_id: Option<OrgId>,
    pub actor_kind: String,
    pub actor_id: Option<String>,

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
    pub action: String,
    pub target_kind: Option<String>,
    pub target_ref: Option<String>,
    pub outcome: String,
    pub ip: Option<String>,
    pub request_id: Option<String>,
    pub data: Option<serde_json::Value>,
    pub created_at: i64,
}

/// A member of an organization with its org-level role.
#[derive(Clone, Debug)]
pub struct Member {
    pub user: User,
    pub role: Role,
}

/// What an API token may do, stored as JSON in `api_tokens.scopes`. The
/// effective role is the weaker of `role` and the owner's own role; the
/// optional project/environment narrows the token to that subtree.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct TokenScope {
    pub role: Role,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub project: Option<uuid::Uuid>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub environment: Option<uuid::Uuid>,
}

/// A personal API token. Only `sha256(secret)` is ever stored.
#[derive(Clone, Debug)]
pub struct ApiToken {
    pub id: TokenId,
    pub org_id: OrgId,
    pub owner: Option<UserId>,
    pub name: String,
    /// Non-secret display prefix, e.g. `kbn_pat_0192f3a1`.
    pub prefix: String,
    pub secret_hash: Vec<u8>,
    pub scope: TokenScope,
    pub expires_at: Option<i64>,
    pub last_used_at: Option<i64>,
    pub revoked_at: Option<i64>,
    pub created_at: i64,
}

impl ApiToken {
    #[must_use]
    pub fn is_usable_at(&self, now_ms: i64) -> bool {
        self.revoked_at.is_none() && self.expires_at.is_none_or(|at| at > now_ms)
    }

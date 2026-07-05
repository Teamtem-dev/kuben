//! Organization members and roles (scenario 4).
//!
//! Rules: nobody grants a role above their own; only owners touch owners;
//! the last owner can be neither demoted nor removed; removing a member
//! revokes their sessions and tokens at once.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ids::{OrgId, UserId},
    model::Member,
    perm::{Perm, Role},
};
use serde::{Deserialize, Serialize};
use tokio::task::spawn_blocking;
use utoipa::ToSchema;

use super::validate;
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    state::ApiState,
};

fn default_role() -> String {
    "developer".into()
}

#[derive(Debug, Serialize, ToSchema)]
pub struct MemberDto {
    pub id: String,
    pub email: String,
    pub display_name: Option<String>,
    pub role: String,
    /// Invited and has not replaced the temporary password yet.
    pub must_change_password: bool,
    pub active: bool,
}

impl From<&Member> for MemberDto {
    fn from(m: &Member) -> Self {
        Self {
            id: m.user.id.to_string(),
            email: m.user.email.clone(),
            display_name: m.user.display_name.clone(),
            role: m.role.to_string(),
            must_change_password: m.user.must_change_password,
            active: m.user.is_active,
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct InviteMember {
    #[schema(example = "carol@example.com")]
    pub email: String,
    pub display_name: Option<String>,
    /// `viewer`, `developer`, `admin` or `owner`.
    #[serde(default = "default_role")]
    pub role: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct InvitedMember {
    pub member: MemberDto,
    /// Temporary password for a newly created account. Shown once; it must be
    /// replaced at the first login. `null` when the user already existed.
    pub temporary_password: Option<String>,
}


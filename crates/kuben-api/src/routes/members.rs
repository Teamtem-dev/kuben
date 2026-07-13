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

#[derive(Debug, Deserialize, ToSchema)]
pub struct UpdateMember {
    pub role: String,
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

async fn find_member(state: &ApiState, org: OrgId, id: &str) -> ApiResult<Member> {
    let not_found = || ApiError(Error::NotFound(format!("member `{id}`")));
    let user: UserId = id.parse().map_err(|_| not_found())?;
    state
        .store
        .list_members(org)
        .await?
        .into_iter()
        .find(|m| m.user.id == user)
        .ok_or_else(not_found)
}

/// Caller's own org role, required for every change.
fn caller_role(authz: &Authz, org: OrgId) -> Result<Role, ApiError> {
    authz.org_role(org).ok_or(ApiError(Error::Forbidden))
}

/// Members of the caller's organization.
#[utoipa::path(
    get,
    path = "/members", operation_id = "listMembers",
    tag = "members",
    responses((status = 200, body = Vec<MemberDto>), (status = 403, body = crate::error::Problem))
)]
pub async fn list(State(state): State<ApiState>, authz: Authz) -> ApiResult<Json<Vec<MemberDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &ScopeChain::org(org))?;
    let members = state.store.list_members(org).await?;
    Ok(Json(members.iter().map(MemberDto::from).collect()))
}

/// Invite a member. New accounts get a one-time temporary password.
#[utoipa::path(
    post,
    path = "/members", operation_id = "inviteMember",
    tag = "members",
    request_body = InviteMember,
    responses(
        (status = 201, body = InvitedMember),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn invite(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<InviteMember>,
) -> ApiResult<(StatusCode, Json<InvitedMember>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::UserAdmin, &ScopeChain::org(org))?;
    let role: Role = body.role.parse()?;
    if role.rank() > caller_role(&authz, org)?.rank() {
        return Err(Error::Forbidden.into());
    }
    let email = body.email.trim().to_ascii_lowercase();
    validate::email(&email)?;

    let (user, temporary_password) = if let Some(existing) = state.store.find_user_by_email(&email).await? {
        if state
            .store
            .list_members(org)
            .await?
            .iter()
            .any(|m| m.user.id == existing.user.id)
        {

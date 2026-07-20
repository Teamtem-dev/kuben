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
            return Err(Error::Conflict(format!("`{email}` is already a member")).into());
        }
        (existing.user, None)
    } else {
        let bytes: [u8; 18] = rand::random();
        let password = URL_SAFE_NO_PAD.encode(bytes);
        let hasher = state.hasher.clone();
        let to_hash = password.clone();
        let hash = spawn_blocking(move || hasher.hash(&to_hash))
            .await
            .map_err(Error::internal)?
            .map_err(Error::internal)?;
        let display_name = body
            .display_name
            .as_deref()
            .map(str::trim)
            .filter(|n| !n.is_empty());
        let user = state
            .store
            .create_invited_user(&email, display_name, Some(&hash))
            .await?;
        (user, Some(password))
    };
    state.store.add_membership(org, user.id).await?;
    state.store.bind_org_role(org, user.id, role).await?;
    let member = Member { user, role };
    Ok((
        StatusCode::CREATED,
        Json(InvitedMember {
            member: MemberDto::from(&member),
            temporary_password,
        }),
    ))
}

/// Change a member's role.
#[utoipa::path(
    patch,
    path = "/members/{member}", operation_id = "updateMember",
    tag = "members",
    params(("member" = String, Path, description = "User id")),
    request_body = UpdateMember,
    responses(
        (status = 200, body = MemberDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn update(
    State(state): State<ApiState>,
    authz: Authz,
    Path(member): Path<String>,
    Json(body): Json<UpdateMember>,
) -> ApiResult<Json<MemberDto>> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::UserAdmin, &ScopeChain::org(org))?;
    let role: Role = body.role.parse()?;
    let target = find_member(&state, org, &member).await?;
    if target.user.id == authz.current.user.id {
        return Err(Error::Conflict("you cannot change your own role".into()).into());
    }
    let caller = caller_role(&authz, org)?;
    let touches_owner = target.role == Role::Owner || role == Role::Owner;
    if (touches_owner && caller != Role::Owner) || role.rank() > caller.rank() {
        return Err(Error::Forbidden.into());
    }
    if target.role == Role::Owner && role != Role::Owner && state.store.count_owners(org).await? <= 1 {
        return Err(Error::Conflict("the last owner cannot be demoted".into()).into());
    }
    state.store.set_org_role(org, target.user.id, role).await?;
    Ok(Json(MemberDto::from(&Member {
        user: target.user,
        role,
    })))
}

/// Remove a member: bindings, sessions and tokens are revoked immediately.
#[utoipa::path(
    delete,
    path = "/members/{member}", operation_id = "removeMember",
    tag = "members",
    params(("member" = String, Path, description = "User id")),
    responses(
        (status = 204, description = "Removed"),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn remove(
    State(state): State<ApiState>,
    authz: Authz,
    Path(member): Path<String>,
) -> ApiResult<StatusCode> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::UserAdmin, &ScopeChain::org(org))?;
    let target = find_member(&state, org, &member).await?;
    if target.user.id == authz.current.user.id {
        return Err(Error::Conflict("you cannot remove yourself".into()).into());
    }
    if target.role == Role::Owner {
        if caller_role(&authz, org)? != Role::Owner {
            return Err(Error::Forbidden.into());
        }
        if state.store.count_owners(org).await? <= 1 {
            return Err(Error::Conflict("the last owner cannot be removed".into()).into());
        }
    }
    state.store.remove_member(org, target.user.id).await?;
    state.store.revoke_all_sessions(target.user.id).await?;
    state.store.revoke_user_tokens(org, target.user.id).await?;
    state.session_cache.invalidate_all();
    Ok(StatusCode::NO_CONTENT)
}

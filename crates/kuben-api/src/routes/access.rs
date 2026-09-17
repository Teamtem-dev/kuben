//! Roles on one project or environment (M4.1). A scoped role adds to what a
//! member holds on the organization; it never makes anyone a member.
//!
//! Only people manage roles (no API tokens), nobody grants or removes a role
//! stronger than their own on that node, and nobody changes their own.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{
    Error,
    authz::ScopeChain,
    ids::{OrgId, UserId},
    model::{Member, ScopeKind},
    perm::{Perm, Role},
    traits::effective_role,
};
use serde::Deserialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::{members::MemberDto, scope};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct PutScopedRole {
    /// `viewer`, `developer`, `admin` or `owner`.
    #[schema(example = "developer")]
    pub role: String,
}

/// A project or environment, with what authorizes changes there.
struct Node {
    org: OrgId,
    kind: ScopeKind,
    id: Uuid,
    chain: ScopeChain,
}

async fn project_node(state: &ApiState, authz: &Authz, project: &str) -> ApiResult<Node> {
    let p = scope::project(state, authz, project).await?;
    Ok(Node {
        org: p.org,
        kind: ScopeKind::Project,
        id: *p.id().as_uuid(),
        chain: p.chain(),
    })
}

async fn environment_node(state: &ApiState, authz: &Authz, project: &str, env: &str) -> ApiResult<Node> {
    let e = scope::environment(state, authz, project, env).await?;
    Ok(Node {
        org: e.project.org,
        kind: ScopeKind::Environment,
        id: *e.id().as_uuid(),
        chain: e.chain(),
    })
}

fn user_id(member: &str) -> Result<UserId, Error> {
    member
        .parse()
        .map_err(|_| Error::NotFound(format!("member `{member}`")))
}

/// Whether a caller holding `caller` may grant `granted` over someone who
/// holds `current` there now.
fn may_assign(caller: Option<Role>, current: Option<Role>, granted: Option<Role>) -> bool {
    let Some(caller) = caller else {
        return false;
    };
    let within = |r: Option<Role>| r.is_none_or(|r| r.rank() <= caller.rank());
    within(current) && within(granted)
}

async fn list(state: &ApiState, authz: &Authz, node: Node) -> ApiResult<Json<Vec<MemberDto>>> {
    let _proof = authz.require(state, Perm::OrgRead, &node.chain)?;
    let members = state.store.scoped_members(node.org, node.kind, node.id).await?;
    Ok(Json(members.iter().map(MemberDto::from).collect()))
}

async fn current_role(state: &ApiState, node: &Node, user: UserId) -> ApiResult<Option<Member>> {
    Ok(state
        .store
        .scoped_members(node.org, node.kind, node.id)
        .await?
        .into_iter()
        .find(|m| m.user.id == user))
}

async fn put(
    state: &ApiState,
    authz: &Authz,
    node: Node,
    member: &str,
    body: PutScopedRole,
) -> ApiResult<Json<MemberDto>> {
    authz.forbid_token()?;
    let _proof = authz.require(state, Perm::UserAdmin, &node.chain)?;
    let user = user_id(member)?;
    if user == authz.current.user.id {
        return Err(Error::Conflict("you cannot change your own role".into()).into());
    }
    let role: Role = body.role.parse()?;
    let current = current_role(state, &node, user).await?.map(|m| m.role);
    if !may_assign(effective_role(&authz.subject, &node.chain), current, Some(role)) {
        return Err(Error::Forbidden.into());
    }
    if !state
        .store
        .bind_scoped_role(node.org, user, node.kind, node.id, role)
        .await?
    {
        return Err(Error::NotFound(format!("member `{member}`")).into());
    }
    let bound = current_role(state, &node, user)
        .await?
        .ok_or_else(|| Error::Internal("the bound role is missing".into()))?;
    Ok(Json(MemberDto::from(&bound)))
}

async fn remove(state: &ApiState, authz: &Authz, node: Node, member: &str) -> ApiResult<StatusCode> {
    authz.forbid_token()?;
    let _proof = authz.require(state, Perm::UserAdmin, &node.chain)?;
    let user = user_id(member)?;
    if user == authz.current.user.id {
        return Err(Error::Conflict("you cannot remove your own role".into()).into());
    }
    let Some(current) = current_role(state, &node, user).await? else {
        return Err(Error::NotFound(format!("a role of member `{member}` here")).into());
    };
    if !may_assign(
        effective_role(&authz.subject, &node.chain),
        Some(current.role),
        None,
    ) {
        return Err(Error::Forbidden.into());
    }
    state
        .store
        .unbind_scoped_role(node.org, user, node.kind, node.id)
        .await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Members with a role on this project.
#[utoipa::path(
    get,
    path = "/projects/{project}/members", operation_id = "listProjectMembers",
    tag = "members",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 200, body = Vec<MemberDto>), (status = 403, body = crate::error::Problem), (status = 404, body = crate::error::Problem))
)]
pub async fn list_project(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<Vec<MemberDto>>> {
    let node = project_node(&state, &authz, &project).await?;
    list(&state, &authz, node).await
}

/// Give an organization member a role on this project, or change it.
#[utoipa::path(
    put,
    path = "/projects/{project}/members/{member}", operation_id = "putProjectMember",
    tag = "members",
    params(
        ("project" = String, Path, description = "Project name"),
        ("member" = String, Path, description = "User id"),
    ),
    request_body = PutScopedRole,
    responses(
        (status = 200, body = MemberDto),
        (status = 403, body = crate::error::Problem, description = "Above your own role, or an API token"),
        (status = 404, body = crate::error::Problem, description = "Not a member of the organization"),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put_project(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, member)): Path<(String, String)>,
    Json(body): Json<PutScopedRole>,
) -> ApiResult<Json<MemberDto>> {
    let node = project_node(&state, &authz, &project).await?;
    put(&state, &authz, node, &member, body).await
}

/// Remove a member's role on this project; their organization role stays.
#[utoipa::path(
    delete,
    path = "/projects/{project}/members/{member}", operation_id = "removeProjectMember",
    tag = "members",
    params(
        ("project" = String, Path, description = "Project name"),
        ("member" = String, Path, description = "User id"),
    ),
    responses(
        (status = 204, description = "Removed"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn remove_project(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, member)): Path<(String, String)>,
) -> ApiResult<StatusCode> {
    let node = project_node(&state, &authz, &project).await?;
    remove(&state, &authz, node, &member).await
}

/// Members with a role on this environment.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/members", operation_id = "listEnvironmentMembers",
    tag = "members",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<MemberDto>), (status = 403, body = crate::error::Problem), (status = 404, body = crate::error::Problem))
)]
pub async fn list_environment(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<MemberDto>>> {
    let node = environment_node(&state, &authz, &project, &environment).await?;
    list(&state, &authz, node).await
}

/// Give an organization member a role on this environment, or change it.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/members/{member}", operation_id = "putEnvironmentMember",
    tag = "members",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("member" = String, Path, description = "User id"),
    ),
    request_body = PutScopedRole,
    responses(
        (status = 200, body = MemberDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put_environment(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, member)): Path<(String, String, String)>,
    Json(body): Json<PutScopedRole>,
) -> ApiResult<Json<MemberDto>> {
    let node = environment_node(&state, &authz, &project, &environment).await?;
    put(&state, &authz, node, &member, body).await
}

/// Remove a member's role on this environment.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}/members/{member}", operation_id = "removeEnvironmentMember",
    tag = "members",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("member" = String, Path, description = "User id"),
    ),
    responses(
        (status = 204, description = "Removed"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn remove_environment(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, member)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let node = environment_node(&state, &authz, &project, &environment).await?;
    remove(&state, &authz, node, &member).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nobody_grants_or_removes_above_their_own_role() {
        use Role::{Admin, Developer, Owner, Viewer};
        assert!(may_assign(Some(Admin), None, Some(Developer)));
        assert!(may_assign(Some(Admin), Some(Developer), Some(Admin)));
        assert!(!may_assign(Some(Admin), None, Some(Owner)), "escalation");
        assert!(
            !may_assign(Some(Admin), Some(Owner), Some(Viewer)),
            "demoting a stronger member"
        );
        assert!(
            !may_assign(Some(Admin), Some(Owner), None),
            "removing a stronger member"
        );
        assert!(may_assign(Some(Owner), Some(Owner), None));
        assert!(!may_assign(None, None, Some(Viewer)));
    }

    #[test]
    fn member_ids_are_user_ids() {
        assert!(user_id("0192f3a1-0000-7000-8000-00000000000a").is_ok());
        assert!(matches!(user_id("bob"), Err(Error::NotFound(_))));
        assert!(serde_json::from_str::<PutScopedRole>(r#"{"role":"admin","scope":"org"}"#).is_err());
    }
}

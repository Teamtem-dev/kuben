//! Projects on the SQL model (ADR-032). SQL holds them; the materializer
//! writes their `Project` resource, whose status the projection adds.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{Error, authz::ScopeChain, ids::OrgId, perm::Perm};
use kuben_platform::projection::ProjectView;
use kuben_store::repo::{PROJECT_APPLY, PROJECT_DELETE, Project, Subject};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{request, scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Serialize, ToSchema)]
pub struct ProjectDto {
    pub name: String,
    /// The project's id.
    pub uid: Option<String>,
    pub display_name: String,
    pub description: Option<String>,
    pub org: Option<String>,
    pub environments: u32,
    pub ready: bool,
    pub deleting: bool,
    pub created_at: Option<String>,
}

impl ProjectDto {
    #[must_use]
    pub fn of(org: OrgId, project: &Project, view: Option<&ProjectView>) -> Self {
        Self {
            name: project.slug.clone(),
            uid: Some(project.id.to_string()),
            display_name: project.name.clone(),
            description: project.description.clone(),
            org: Some(org.to_string()),
            environments: project.environments,
            ready: view.is_some_and(|v| v.ready) && !project.deleting,
            deleting: project.deleting,
            created_at: Some(request::timestamp(project.created_at)),
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateProject {
    /// DNS label, e.g. `shop` (at most 40 characters).
    #[schema(example = "shop")]
    pub name: String,
    #[schema(example = "Online Shop")]
    pub display_name: String,
    pub description: Option<String>,
}

/// List projects visible to the caller.
#[utoipa::path(get, path = "/projects", operation_id = "listProjects", tag = "projects", responses((status = 200, body = Vec<ProjectDto>)))]
pub async fn list(State(state): State<ApiState>, authz: Authz) -> ApiResult<Json<Vec<ProjectDto>>> {
    let mut items = Vec::new();
    for org in authz.org_ids() {
        let mut tenant = state.store.tenant(org).await?;
        for project in tenant.projects().await? {
            let view = state
                .projections
                .project(&project.slug)
                .filter(|v| v.org.as_deref() == Some(org.to_string().as_str()));
            items.push(ProjectDto::of(org, &project, view.as_deref()));
        }
    }
    Ok(Json(items))
}

/// One project.
#[utoipa::path(
    get,
    path = "/projects/{project}", operation_id = "getProject",
    tag = "projects",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 200, body = ProjectDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<ProjectDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectRead, &p.chain())?;
    Ok(Json(ProjectDto::of(p.org, &p.project, p.view.as_deref())))
}

/// Create a project; its `Project` resource follows.
#[utoipa::path(post, path = "/projects", operation_id = "createProject", tag = "projects", request_body = CreateProject, responses(
    (status = 201, body = ProjectDto),
    (status = 403, body = crate::error::Problem),
    (status = 409, body = crate::error::Problem),
    (status = 422, body = crate::error::Problem),
))]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateProject>,
) -> ApiResult<(StatusCode, Json<ProjectDto>)> {
    validate::dns_label("name", &body.name, 40)?;
    let display_name = body.display_name.trim();
    if display_name.is_empty() || display_name.len() > 100 {
        return Err(Error::Validation("display_name must be 1–100 characters".into()).into());
    }
    let org = authz.org_ids().into_iter().next().ok_or(Error::Forbidden)?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &ScopeChain::org(org))?;
    // Project resources are cluster-wide: a name another organization holds
    // there is taken.
    let taken = || Error::Conflict(format!("project `{}` already exists", body.name));
    if state
        .projections
        .project(&body.name)
        .is_some_and(|v| v.org.as_deref() != Some(org.to_string().as_str()))
    {
        return Err(taken().into());
    }
    let description = body
        .description
        .as_deref()
        .map(str::trim)
        .filter(|d| !d.is_empty());
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(org).await?;
    let id = tenant
        .create_project_described(&body.name, display_name, description)
        .await
        .map_err(|e| request::duplicate(e, &format!("project `{}`", body.name)))?;
    tenant
        .request(
            PROJECT_APPLY,
            Subject::project(id),
            &actor,
            request::audit(&authz, PROJECT_APPLY, "project", body.name.clone()),
        )
        .await?;
    let project = tenant.project(&body.name).await?.ok_or_else(taken)?;
    tenant.commit().await?;
    Ok((StatusCode::CREATED, Json(ProjectDto::of(org, &project, None))))
}

/// Delete an empty project. Projects with environments are refused (`409`):
/// deleting environments is an explicit, per-environment decision.
#[utoipa::path(
    delete,
    path = "/projects/{project}", operation_id = "deleteProject",
    tag = "projects",
    params(("project" = String, Path, description = "Project name")),
    responses(
        (status = 204, description = "Deleted"),
        (status = 404, body = crate::error::Problem),
        (status = 409, description = "The project still has environments", body = crate::error::Problem),
    )
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<StatusCode> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let remaining = tenant.live_environments(p.id()).await?;
    if remaining > 0 {
        return Err(Error::Conflict(format!(
            "project `{project}` still has {remaining} environment(s); delete them first"
        ))
        .into());
    }
    if !tenant.mark_project_deleting(p.id()).await? {
        return Err(Error::Conflict(format!("project `{project}` is being deleted")).into());
    }
    let (_, actor) = request::actor(&authz);
    tenant
        .request(
            PROJECT_DELETE,
            Subject::project(p.id()),
            &actor,
            request::audit(&authz, PROJECT_DELETE, "project", project.clone()),
        )
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

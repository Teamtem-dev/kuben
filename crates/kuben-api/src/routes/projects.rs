//! Projects: read from the in-memory projection; write through the
//! Kubernetes API (CRD is the source of truth).

use std::collections::BTreeMap;

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kube::{
    Api,
    api::{DeleteParams, ObjectMeta, PostParams},
};
use kuben_core::{Error, authz::ScopeChain, perm::Perm};
use kuben_crd::{Project, ProjectSpec, labels};
use kuben_platform::projection::ProjectView;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

#[derive(Debug, Serialize, ToSchema)]
pub struct ProjectDto {
    pub name: String,
    pub uid: Option<String>,
    pub display_name: String,
    pub description: Option<String>,
    pub org: Option<String>,
    pub environments: u32,
    pub ready: bool,
    pub deleting: bool,
    pub created_at: Option<String>,
}

impl From<&ProjectView> for ProjectDto {
    fn from(p: &ProjectView) -> Self {
        Self {
            name: p.name.clone(),
            uid: p.uid.clone(),
            display_name: p.display_name.clone(),
            description: p.description.clone(),
            org: p.org.clone(),
            environments: p.environments,
            ready: p.ready,
            deleting: p.deleting,
            created_at: p.created_at.clone(),
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
    let orgs: Vec<String> = authz.org_ids().iter().map(ToString::to_string).collect();
    let items = state
        .projections
        .projects()
        .iter()
        .filter(|p| p.org.as_ref().is_some_and(|o| orgs.contains(o)))
        .map(|p| ProjectDto::from(&**p))
        .collect();
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
    let p = scope::project(&state, &authz, &project)?;
    let _proof = authz.require(&state, Perm::ProjectRead, &p.chain())?;
    Ok(Json(ProjectDto::from(&*p.view)))
}

/// Create a project (writes a `Project` CR).
#[utoipa::path(post, path = "/projects", operation_id = "createProject", tag = "projects", request_body = CreateProject, responses(
    (status = 201, body = ProjectDto),
    (status = 403, body = crate::error::Problem),
    (status = 409, body = crate::error::Problem),
    (status = 422, body = crate::error::Problem),
    (status = 503, description = "No cluster configured", body = crate::error::Problem),
))]
pub async fn create(

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
    let client = scope::cluster(&state)?;

    let project = Project {
        metadata: ObjectMeta {
            name: Some(body.name.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ORG.to_owned(), org.to_string()),
            ])),
            ..ObjectMeta::default()
        },
        spec: ProjectSpec {
            display_name: display_name.to_owned(),
            description: body.description.clone().filter(|d| !d.trim().is_empty()),
            previews: kuben_crd::PreviewPolicy::default(),
        },
        status: None,
    };
    let created = Api::<Project>::all(client)
        .create(&PostParams::default(), &project)
        .await
        .map_err(|e| scope::kube_error(e, &body.name))?;
    Ok((
        StatusCode::CREATED,
        Json(ProjectDto::from(&ProjectView::from(&created))),
    ))
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
    let p = scope::project(&state, &authz, &project)?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    let remaining = state
        .projections
        .environments()
        .iter()
        .filter(|e| e.project == p.view.name)
        .count();
    if remaining > 0 {
        return Err(Error::Conflict(format!(
            "project `{project}` still has {remaining} environment(s); delete them first"
        ))
        .into());
    }
    let client = scope::cluster(&state)?;
    Api::<Project>::all(client)
        .delete(&p.view.name, &DeleteParams::default())
        .await
        .map_err(|e| scope::kube_error(e, &project))?;
    Ok(StatusCode::NO_CONTENT)
}

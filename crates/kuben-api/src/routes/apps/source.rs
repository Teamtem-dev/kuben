//! An app's Git source (M3): which repository and branch it builds from,
//! how, and where the image goes. Binding or changing the source asks for a
//! sync, which reads the branch head from GitHub and queues a build of it.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{
    Error,
    ids::OperationId,
    perm::Perm,
    source::{BranchName, BuildRecipe, BuildStrategy, RepoName, RepoPath},
};
use kuben_store::repo::{Bound, NewAudit, NewBinding, SourceBinding};
use serde::{Deserialize, Serialize};
use serde_json::json;
use utoipa::ToSchema;

use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    oci,
    routes::{git::github, scope},
    state::ApiState,
};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum StrategyDto {
    /// The Dockerfile if the repository has one, Railpack otherwise.
    #[default]
    Auto,
    Dockerfile,
    Railpack,
}

impl From<StrategyDto> for BuildStrategy {
    fn from(s: StrategyDto) -> Self {
        match s {
            StrategyDto::Auto => Self::Auto,
            StrategyDto::Dockerfile => Self::Dockerfile,
            StrategyDto::Railpack => Self::Railpack,
        }
    }
}

impl From<BuildStrategy> for StrategyDto {
    fn from(s: BuildStrategy) -> Self {
        match s {
            BuildStrategy::Auto => Self::Auto,
            BuildStrategy::Dockerfile => Self::Dockerfile,
            BuildStrategy::Railpack => Self::Railpack,
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PutSource {
    /// A GitHub App installation linked to this organization.
    pub installation_id: u64,
    #[schema(example = "acme/shop")]
    pub repository: String,
    #[schema(example = "main")]
    pub branch: String,
    #[serde(default)]
    pub strategy: StrategyDto,
    /// Build context inside the repository; the root when empty.
    #[serde(default)]
    #[schema(example = "apps/web")]
    pub context: String,
    /// Dockerfile relative to the context; `Dockerfile` when unset.
    pub dockerfile: Option<String>,
    /// Where builds push, without tag or digest.
    #[schema(example = "registry.example.com/acme/shop")]
    pub image_repository: String,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct SourceDto {
    pub installation_id: u64,
    pub repository: String,
    pub branch: String,
    pub strategy: StrategyDto,
    pub context: String,
    pub dockerfile: Option<String>,
    pub image_repository: String,
    /// The last head read from GitHub.
    pub head: Option<String>,
    /// The sync this change or request queued, if any.
    pub sync_operation: Option<String>,
}

impl SourceDto {
    fn new(b: &SourceBinding, sync: Option<OperationId>) -> Self {
        Self {
            installation_id: b.installation_id,
            repository: b.repository.to_string(),
            branch: b.branch.to_string(),
            strategy: b.recipe.strategy.into(),
            context: b.recipe.context.as_str().to_owned(),
            dockerfile: b.recipe.dockerfile.as_ref().map(|d| d.as_str().to_owned()),
            image_repository: b.image_repository.clone(),
            head: b.head_sha.as_ref().map(ToString::to_string),
            sync_operation: sync.map(|o| o.to_string()),
        }
    }
}

fn invalid(e: impl std::fmt::Display) -> ApiError {
    ApiError(Error::Validation(e.to_string()))
}

/// `registry/path` without tag or digest, normalized like Docker does.
fn image_repository(value: &str) -> Result<String, ApiError> {
    let last = value.rsplit('/').next().unwrap_or_default();
    if value.contains('@') || last.contains(':') {
        return Err(invalid(format!(
            "`{value}` must name a repository without tag or digest; builds tag and pin it"
        )));
    }
    Ok(oci::parse(value).map_err(invalid)?.repository())
}

fn new_binding(body: &PutSource) -> Result<NewBinding, ApiError> {
    Ok(NewBinding {
        installation_id: body.installation_id,
        repository: body.repository.parse::<RepoName>().map_err(invalid)?,
        branch: body.branch.parse::<BranchName>().map_err(invalid)?,
        recipe: BuildRecipe {
            strategy: body.strategy.into(),
            context: body.context.parse::<RepoPath>().map_err(invalid)?,
            dockerfile: body
                .dockerfile
                .as_deref()
                .map(str::parse::<RepoPath>)
                .transpose()
                .map_err(invalid)?
                .filter(|p| !p.is_root()),
        },
        image_repository: image_repository(&body.image_repository)?,
    })
}

fn audit(authz: &Authz, action: &str, reference: String, data: serde_json::Value) -> NewAudit {
    NewAudit {
        actor_kind: authz.current.via.to_owned(),
        actor_id: Some(authz.current.user.id.to_string()),
        action: action.into(),
        target_kind: Some("app".into()),
        target_ref: Some(reference),
        outcome: "accepted".into(),
        data: Some(data),
        ..NewAudit::default()
    }
}

/// The app's Git source.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/source", operation_id = "getAppSource",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses(
        (status = 200, body = SourceDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem, description = "No such app, or it has no Git source"),
    )
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<SourceDto>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &t.chain())?;
    let mut tenant = state.store.tenant(t.org).await?;
    let binding = tenant
        .binding_of_target(t.target)
        .await?
        .ok_or_else(|| ApiError(Error::NotFound(format!("a Git source of app `{app}`"))))?;
    Ok(Json(SourceDto::new(&binding, None)))
}

/// Build the app from a Git repository and branch, or change how.
///
/// A change stops earlier builds from deploying themselves and builds the
/// branch head again.
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/apps/{app}/source", operation_id = "putAppSource",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = PutSource,
    responses(
        (status = 200, body = SourceDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<PutSource>,
) -> ApiResult<Json<SourceDto>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &t.chain())?;
    github(&state)?;
    let new = new_binding(&body)?;
    let mut tenant = state.store.tenant(t.org).await?;
    let bound = tenant.bind_source(t.project, t.target, &new).await?;
    let id = match bound {
        Bound::InstallationMissing => {
            return Err(invalid(format!(
                "installation {} is not linked to this organization",
                new.installation_id
            )));
        }
        Bound::NotFound => return Err(ApiError(Error::NotFound(format!("app `{app}`")))),
        Bound::Created(id) | Bound::Changed(id) | Bound::Unchanged(id) => id,
    };
    let binding = tenant
        .binding(id)
        .await?
        .ok_or_else(|| ApiError(Error::Internal("the bound source is missing".into())))?;
    let reference = format!("{project}/{environment}/{app}");
    let data = json!({ "repository": new.repository, "branch": new.branch, "changed": !matches!(bound, Bound::Unchanged(_)) });
    let requested_by = format!("user:{}", authz.current.user.id);
    let sync = tenant
        .request_sync(
            &binding,
            &requested_by,
            audit(&authz, "syncSource", reference, data),
        )
        .await?;
    tenant.commit().await?;
    Ok(Json(SourceDto::new(&binding, Some(sync))))
}

/// Read the branch head from GitHub now and build it if it is new.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/source/sync", operation_id = "syncAppSource",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses(
        (status = 202, body = SourceDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn sync(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<(StatusCode, Json<SourceDto>)> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &t.chain())?;
    let mut tenant = state.store.tenant(t.org).await?;
    let binding = tenant
        .binding_of_target(t.target)
        .await?
        .ok_or_else(|| ApiError(Error::NotFound(format!("a Git source of app `{app}`"))))?;
    let reference = format!("{project}/{environment}/{app}");
    let requested_by = format!("user:{}", authz.current.user.id);
    let data = json!({ "repository": binding.repository, "branch": binding.branch });
    let operation = tenant
        .request_sync(
            &binding,
            &requested_by,
            audit(&authz, "syncSource", reference, data),
        )
        .await?;
    tenant.commit().await?;
    Ok((
        StatusCode::ACCEPTED,
        Json(SourceDto::new(&binding, Some(operation))),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(image: &str) -> PutSource {
        PutSource {
            installation_id: 7,
            repository: "Acme/Shop".into(),
            branch: "main".into(),
            strategy: StrategyDto::Dockerfile,
            context: "./apps/web/".into(),
            dockerfile: Some(String::new()),
            image_repository: image.into(),
        }
    }

    #[test]
    fn bindings_are_validated_and_normalized() {
        let b = new_binding(&body("registry.example.com:5000/acme/shop")).expect("binding");
        assert_eq!(b.repository.as_str(), "acme/shop");
        assert_eq!(b.recipe.context.as_str(), "apps/web");
        assert_eq!(
            b.recipe.dockerfile, None,
            "an empty Dockerfile path is the default"
        );
        assert_eq!(b.recipe.strategy, BuildStrategy::Dockerfile);
        assert_eq!(b.image_repository, "registry.example.com:5000/acme/shop");
        assert_eq!(
            new_binding(&body("acme/shop")).expect("hub").image_repository,
            "docker.io/acme/shop"
        );
    }

    #[test]
    fn tags_digests_and_escapes_are_refused() {
        for image in [
            "ghcr.io/acme/shop:v1",
            "ghcr.io/acme/shop@sha256:00",
            "Upper/Case",
        ] {
            assert!(new_binding(&body(image)).is_err(), "{image}");
        }
        let mut escape = body("ghcr.io/acme/shop");
        escape.context = "../secrets".into();
        assert!(new_binding(&escape).is_err());
        let mut branch = body("ghcr.io/acme/shop");
        branch.branch = "a..b".into();
        assert!(new_binding(&branch).is_err());
    }
}

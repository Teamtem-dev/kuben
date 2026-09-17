//! An app's image update policy (M5.4).

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{
    Error,
    image_policy::{MAX_INTERVAL_SECS, MIN_INTERVAL_SECS, Pattern},
    perm::Perm,
};
use kuben_store::repo::{ImagePolicy, NewImagePolicy};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::{
    authz::Authz,
    error::ApiResult,
    oci,
    routes::{request, scope},
    state::ApiState,
};

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ImagePolicyDto {
    pub repository: String,
    /// `semver:<range>`, `tag:<glob>` or a tag.
    pub pattern: String,
    pub enabled: bool,
    pub interval_secs: i32,
    pub next_check_at: String,
    pub last_checked_at: Option<String>,
    pub last_tag: Option<String>,
    pub last_digest: Option<String>,
    pub last_error: Option<String>,
    /// Failed checks in a row.
    pub failures: i32,
    /// The run the policy started last.
    pub last_run: Option<uuid::Uuid>,
}

impl From<ImagePolicy> for ImagePolicyDto {
    fn from(p: ImagePolicy) -> Self {
        Self {
            repository: p.repository,
            pattern: p.pattern,
            enabled: p.enabled,
            interval_secs: p.interval_secs,
            next_check_at: request::timestamp(p.next_check_at),
            last_checked_at: p.last_checked_at.map(request::timestamp),
            last_tag: p.last_tag,
            last_digest: p.last_digest,
            last_error: p.last_error,
            failures: p.failures,
            last_run: p.last_run_id,
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PutImagePolicy {
    /// The repository to watch; the app's current image repository when
    /// unset.
    #[serde(default)]
    pub repository: Option<String>,
    #[schema(example = "semver:^1.2")]
    pub pattern: String,
    #[serde(default = "enabled")]
    pub enabled: bool,
    #[serde(default = "interval")]
    pub interval_secs: u32,
}

const fn enabled() -> bool {
    true
}

const fn interval() -> u32 {
    300
}

/// An app's image update policy.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/image-policy", operation_id = "getImagePolicy",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = ImagePolicyDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<ImagePolicyDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let policy = tenant
        .image_policy(a.app.target)
        .await?
        .ok_or_else(|| Error::NotFound(format!("an image policy of `{app}`")))?;
    Ok(Json(ImagePolicyDto::from(policy)))
}

/// Follow a tag pattern of the app's image repository: a new digest is
/// deployed (with approval where the environment asks for it).
#[utoipa::path(
    put,
    path = "/projects/{project}/environments/{environment}/apps/{app}/image-policy", operation_id = "putImagePolicy",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = PutImagePolicy,
    responses(
        (status = 200, body = ImagePolicyDto),
        (status = 409, body = crate::error::Problem, description = "The app builds from Git"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<PutImagePolicy>,
) -> ApiResult<Json<ImagePolicyDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let pattern = Pattern::parse(&body.pattern).map_err(|e| Error::Validation(e.to_string()))?;
    if !(MIN_INTERVAL_SECS..=MAX_INTERVAL_SECS).contains(&body.interval_secs) {
        return Err(Error::Validation(format!(
            "intervalSecs must be {MIN_INTERVAL_SECS} to {MAX_INTERVAL_SECS}"
        ))
        .into());
    }
    let given = body
        .repository
        .clone()
        .or_else(|| a.app.image.clone())
        .ok_or_else(|| Error::Validation("the app has no image yet: name the repository".into()))?;
    let reference = oci::parse(&given).map_err(|e| Error::Validation(e.to_string()))?;
    let repository = reference.repository();
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if tenant.binding_of_target(a.app.target).await?.is_some() {
        return Err(Error::Conflict(format!(
            "app `{app}` builds from Git: its builds decide what it runs"
        ))
        .into());
    }
    let (_, actor) = request::actor(&authz);
    tenant
        .set_image_policy(&NewImagePolicy {
            project: a.env.project.id(),
            target: a.app.target,
            repository: &repository,
            pattern: &pattern.text(),
            enabled: body.enabled,
            interval_secs: body.interval_secs,
            by: &actor,
        })
        .await?;
    let mut audit = request::audit(
        &authz,
        "image-policy.updated",
        "app",
        format!("{project}/{environment}/{app}"),
    );
    audit.data = Some(serde_json::json!({ "repository": repository, "pattern": pattern.text() }));
    tenant.append_audit(audit).await?;
    let saved = tenant
        .image_policy(a.app.target)
        .await?
        .ok_or_else(|| Error::Internal("the policy is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(ImagePolicyDto::from(saved)))
}

/// Stop following the image repository.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}/apps/{app}/image-policy", operation_id = "deleteImagePolicy",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 204, description = "Removed"), (status = 404, body = crate::error::Problem))
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if !tenant.delete_image_policy(a.app.target).await? {
        return Err(Error::NotFound(format!("an image policy of `{app}`")).into());
    }
    tenant
        .append_audit(request::audit(
            &authz,
            "image-policy.deleted",
            "app",
            format!("{project}/{environment}/{app}"),
        ))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

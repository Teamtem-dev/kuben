//! An app's builds (M3): what was built from which commit, where it stands,
//! why it failed, and which release and deployment run it produced.

use axum::{
    Json,
    extract::{Path, Query, State},
};
use kuben_core::{Error, ids::BuildAttemptId, perm::Perm};
use kuben_store::repo::BuildAttempt;
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

const DEFAULT_LIMIT: i64 = 20;
const MAX_LIMIT: i64 = 100;

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct BuildDto {
    pub id: String,
    /// 1 for the first attempt; an infrastructure retry is the next one.
    pub attempt: u32,
    pub repository: String,
    pub branch: String,
    pub commit: String,
    /// `auto`, `dockerfile` or `railpack`, as configured.
    pub strategy: String,
    /// `queued`, `blocked`, `preparing`, `running`, `publishing`,
    /// `verifyingOutput`, `cancelRequested`, `cancelling`, `succeeded`,
    /// `failed` or `cancelled`.
    pub phase: String,
    pub blocked_reason: Option<String>,
    /// `BuildError`, `OutOfMemory`, `DiskFull`, `DeadlineExceeded`,
    /// `LostWorker`, `SourceUnavailable`, `OutputRejected`, …
    pub failure: Option<String>,
    pub failure_detail: Option<String>,
    /// `repository@digest`, once the registry confirmed it.
    pub image: Option<String>,
    pub release: Option<String>,
    pub deployment: Option<String>,
    /// `deployed`, or why the build did not deploy (`StaleSource`,
    /// `BuildConfigChanged`, `NotAutomatic`, …).
    pub deploy_decision: Option<String>,
    pub cancel_requested: bool,
    pub created_at: i64,
    pub started_at: Option<i64>,
    pub finished_at: Option<i64>,
}

impl From<&BuildAttempt> for BuildDto {
    fn from(a: &BuildAttempt) -> Self {
        Self {
            id: a.id.to_string(),
            attempt: a.attempt_no,
            repository: a.repository.to_string(),
            branch: a.branch.to_string(),
            commit: a.commit.to_string(),
            strategy: a.recipe.strategy.as_str().to_owned(),
            phase: a.phase.as_str().to_owned(),
            blocked_reason: a.blocked_reason.clone(),
            failure: a.failure.clone(),
            failure_detail: a.failure_detail.clone(),
            image: a
                .digest
                .as_ref()
                .map(|d| format!("{}@{}", a.image_repository, d.as_str())),
            release: a.release.map(|r| r.to_string()),
            deployment: a.run.map(|r| r.to_string()),
            deploy_decision: a.deploy_decision.clone(),
            cancel_requested: a.cancel_requested,
            created_at: a.created_at,
            started_at: a.started_at,
            finished_at: a.finished_at,
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct BuildsQuery {
    /// Newest first; 20 unless given, at most 100.
    pub limit: Option<i64>,
}

pub(crate) fn build_id(value: &str) -> Result<BuildAttemptId, Error> {
    value
        .parse::<uuid::Uuid>()
        .map(BuildAttemptId::from_uuid)
        .map_err(|_| Error::NotFound(format!("build `{value}`")))
}

/// The app's newest builds.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/builds", operation_id = "listBuilds",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        BuildsQuery,
    ),
    responses(
        (status = 200, body = Vec<BuildDto>),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Query(q): Query<BuildsQuery>,
) -> ApiResult<Json<Vec<BuildDto>>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &t.chain())?;
    let limit = q.limit.unwrap_or(DEFAULT_LIMIT).clamp(1, MAX_LIMIT);
    let mut tenant = state.store.tenant(t.org).await?;
    let builds = tenant.builds_of_target(t.target, limit).await?;
    Ok(Json(builds.iter().map(BuildDto::from).collect()))
}

/// One build of the app.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/builds/{build}", operation_id = "getBuild",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("build" = String, Path, description = "Build id"),
    ),
    responses(
        (status = 200, body = BuildDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app, build)): Path<(String, String, String, String)>,
) -> ApiResult<Json<BuildDto>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &t.chain())?;
    let id = build_id(&build)?;
    let mut tenant = state.store.tenant(t.org).await?;
    let attempt = tenant
        .build_of_target(t.target, id)
        .await?
        .ok_or_else(|| Error::NotFound(format!("build `{build}`")))?;
    Ok(Json(BuildDto::from(&attempt)))
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{ApplicationId, OperationId, OrgId, ProjectId, ReleaseId, SourceBindingId, TargetId},
        ops::{BuildPhase, SourceEpoch},
        source::BuildRecipe,
    };
    use uuid::Uuid;

    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    #[test]
    fn builds_show_their_image_and_outcome() {
        let attempt = BuildAttempt {
            id: BuildAttemptId::from_uuid(Uuid::from_u128(1)),
            org: OrgId::from_uuid(Uuid::from_u128(2)),
            project: ProjectId::from_uuid(Uuid::from_u128(3)),
            application: ApplicationId::from_uuid(Uuid::from_u128(4)),
            target: TargetId::from_uuid(Uuid::from_u128(5)),
            binding: SourceBindingId::from_uuid(Uuid::from_u128(6)),
            attempt_no: 1,
            commit: "0123456789abcdef0123456789abcdef01234567".parse().expect("sha"),
            source_epoch: SourceEpoch(1),
            build_config_revision: 0,
            lifecycle_uid: Uuid::from_u128(7),
            repository: "acme/shop".parse().expect("repo"),
            recipe: BuildRecipe::default(),
            image_repository: "ghcr.io/acme/shop".into(),
            installation_id: 9,
            branch: "main".parse().expect("branch"),
            phase: BuildPhase::Succeeded,
            blocked_reason: None,
            failure: None,
            failure_detail: None,
            reported_digest: Some(DIGEST.parse().expect("digest")),
            digest: Some(DIGEST.parse().expect("digest")),
            job_name: None,
            release: Some(ReleaseId::from_uuid(Uuid::from_u128(8))),
            run: None,
            deploy_decision: Some("StaleSource".into()),
            operation: OperationId::from_uuid(Uuid::from_u128(10)),
            cancel_requested: false,
            created_at: 1,
            started_at: Some(2),
            finished_at: Some(3),
        };
        let dto = BuildDto::from(&attempt);
        assert_eq!(
            dto.image.as_deref(),
            Some(&*format!("ghcr.io/acme/shop@{DIGEST}"))
        );
        assert_eq!(dto.phase, "succeeded");
        assert_eq!(dto.strategy, "auto");
        assert_eq!(dto.deployment, None);
        assert_eq!(dto.deploy_decision.as_deref(), Some("StaleSource"));
        let json = serde_json::to_value(&dto).expect("json");
        assert!(json.get("deployDecision").is_some(), "camelCase");
    }

    #[test]
    fn build_ids_are_uuids() {
        assert!(build_id("0192f3a1-0000-7000-8000-000000000001").is_ok());
        assert!(matches!(build_id("nope"), Err(Error::NotFound(_))));
    }
}

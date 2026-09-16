//! Deploy acceptance on the SQL model (ADR-032; plan §8.2).
//!
//! `POST …/deployments` accepts a deployment run in one transaction and
//! answers `202` with the run to poll; the materializer carries it out. Only
//! apps that exist in SQL are found here: an app known only as a resource is
//! `404` until it is created through SQL or imported.

use std::{collections::BTreeMap, time::Duration};

use axum::{
    Json,
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode, header},
    response::IntoResponse,
};
use kuben_core::{
    Error,
    artifact::Digest as OciDigest,
    ids::{ConfigRevisionId, DeploymentRunId, ReleaseId},
    ops::Generation,
    perm::Perm,
};
use kuben_store::repo::{
    IdempotencyKey, NewAudit, PortableRelease, RunReason, RunSummary, StartDeployment, Started, Tenant,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::releases::Actors;
use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

/// How long an `Idempotency-Key` receipt is kept; the run itself stays.
const RECEIPT_TTL: Duration = Duration::from_hours(24);
const IDEMPOTENCY_KEY: &str = "idempotency-key";
/// Process name of the single image an image deploy carries.
const WEB: &str = "web";

/// Why a run exists.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum DeployReason {
    #[default]
    Deploy,
    /// An earlier release; the app stays on it until automatic deploys resume.
    Rollback,
    /// A release of another environment, without a build.
    Promotion,
}

impl From<DeployReason> for RunReason {
    fn from(r: DeployReason) -> Self {
        match r {
            DeployReason::Deploy => Self::Deploy,
            DeployReason::Rollback => Self::Rollback,
            DeployReason::Promotion => Self::Promotion,
        }
    }
}

#[derive(Debug, Serialize, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct StartDeploymentRequest {
    /// Image by digest: `registry/repository@sha256:…`. Give either `image`
    /// or `release`.
    #[schema(
        example = "ghcr.io/acme/api@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    )]
    pub image: Option<String>,
    /// An existing release of this app: a rollback or a redeploy.
    pub release: Option<Uuid>,
    /// The app configuration: the app spec without its image. Omitted, the
    /// app's latest configuration is used.
    #[schema(value_type = Option<Object>)]
    pub config: Option<Value>,
    #[serde(default)]
    pub reason: DeployReason,
    /// The target generation the caller last saw. A deploy never silently
    /// replaces a newer one: a stale value is refused with `409`.
    pub expected_generation: u64,
}

/// A deployment run and where it stands.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct DeploymentDto {
    pub run: Uuid,
    pub operation: Uuid,
    /// The target generation this run owns.
    pub generation: u64,
    /// `planned`, `pendingDelivery`, `acceptedByCluster`, `applying`,
    /// `succeeded`, `failed`, `superseded`, …
    pub phase: String,
}

impl From<RunSummary> for DeploymentDto {
    fn from(s: RunSummary) -> Self {
        Self {
            run: *s.run.as_uuid(),
            operation: *s.operation.as_uuid(),
            generation: s.generation.0,
            phase: s.phase.as_str().to_owned(),
        }
    }
}

fn actor(authz: &Authz) -> (String, String) {
    let kind = if authz.current.token.is_some() {
        "token"
    } else {
        "user"
    };
    (kind.to_owned(), format!("{kind}:{}", authz.current.user.id))
}

/// The caller's `Idempotency-Key`, if any: 1–200 visible ASCII characters.
fn idempotency_key(headers: &HeaderMap, actor: &str) -> ApiResult<Option<IdempotencyKey>> {
    let Some(value) = headers.get(IDEMPOTENCY_KEY) else {
        return Ok(None);
    };
    let key = value
        .to_str()
        .ok()
        .map(str::trim)
        .filter(|k| !k.is_empty() && k.len() <= 200 && k.bytes().all(|b| b.is_ascii_graphic()))
        .ok_or_else(|| {
            Error::Validation("Idempotency-Key must be 1 to 200 visible ASCII characters".into())
        })?;
    Ok(Some(IdempotencyKey {
        actor: actor.to_owned(),
        key: key.to_owned(),
        ttl: RECEIPT_TTL,
    }))
}

/// `repository@sha256:…` → the repository and the digest.
fn pinned_image(image: &str) -> ApiResult<(String, OciDigest)> {
    let (repository, digest) = image
        .rsplit_once('@')
        .filter(|(repository, _)| !repository.is_empty())
        .ok_or_else(|| {
            Error::Validation(format!(
                "`{image}` is not pinned by digest: use repository@sha256:…"
            ))
        })?;
    let digest = digest
        .parse()
        .map_err(|e: kuben_core::artifact::InvalidDigest| Error::Validation(e.to_string()))?;
    Ok((repository.to_owned(), digest))
}

/// The release to deploy: a new one from `image` (deduplicated by content),
/// or an existing `release`. Exactly one of the two must be given.
async fn release_for(
    tenant: &mut Tenant,
    t: &scope::TargetScope,
    body: &StartDeploymentRequest,
    actor: &str,
) -> ApiResult<ReleaseId> {
    match (&body.image, body.release) {
        (Some(image), None) => {
            let (repository, digest) = pinned_image(image)?;
            let release = PortableRelease {
                application: t.application,
                artifacts: BTreeMap::from([(WEB.to_owned(), digest)]),
                process_contract: json!({}),
                portable_config: json!({}),
                renderer_schema: 1,
                source: Some(json!({ "image_repository": repository })),
                created_by: actor.to_owned(),
            };
            Ok(tenant.create_release(t.project, &release).await?.0)
        }
        (None, Some(release)) => Ok(ReleaseId::from_uuid(release)),
        _ => Err(Error::Validation("give exactly one of `image` and `release`".into()).into()),
    }
}

/// The configuration to deploy: a new revision from `config`, or the
/// target's latest one.
async fn config_revision_for(
    tenant: &mut Tenant,
    t: &scope::TargetScope,
    config: Option<&Value>,
    actor: &str,
) -> ApiResult<ConfigRevisionId> {
    let revision = match config {
        Some(config) => tenant
            .create_config_revision(t.project, t.target, config, actor)
            .await?
            .map(|(id, _)| id)
            .ok_or_else(|| Error::NotFound("the app".into()))?,
        None => tenant
            .latest_config_revision(t.target)
            .await?
            .ok_or_else(|| Error::Validation("the app has no configuration yet: send `config`".into()))?,
    };
    Ok(revision)
}

/// Accept a deployment of this app.
///
/// The release (from `image`, or an existing `release`), the configuration
/// revision and the run are written in one transaction with the audit record
/// and the message to the executors; then the call answers `202` with the
/// run's `Location`. Replaying the request with the same `Idempotency-Key`
/// returns the first run.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments", operation_id = "startDeployment",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("Idempotency-Key" = Option<String>, Header, description = "Replays of the same request return the first run"),
    ),
    request_body = StartDeploymentRequest,
    responses(
        (status = 202, body = DeploymentDto, description = "Accepted: poll the `Location`"),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "A stale expected generation, or an Idempotency-Key used for another request"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn start(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<StartDeploymentRequest>,
) -> ApiResult<impl IntoResponse> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &t.chain())?;
    let (actor_kind, actor) = actor(&authz);
    let key = idempotency_key(&headers, &actor)?;
    let canonical = serde_json::to_vec(&json!({
        "project": project,
        "environment": environment,
        "app": app,
        "request": body,
    }))
    .map_err(Error::internal)?;
    let input_hash = Sha256::digest(&canonical).to_vec();

    let mut tenant = state.store.tenant(t.org).await?;
    let release = release_for(&mut tenant, &t, &body, &actor).await?;
    let config_revision = config_revision_for(&mut tenant, &t, body.config.as_ref(), &actor).await?;
    let lifecycle_uid = tenant
        .target_state(t.target)
        .await?
        .ok_or_else(|| Error::NotFound(format!("app `{app}`")))?
        .lifecycle_uid;
    let request = StartDeployment {
        project: t.project,
        target: t.target,
        release,
        config_revision,
        render_plan: None,
        expected_generation: Generation(body.expected_generation),
        lifecycle_uid,
        reason: body.reason.into(),
        requested_by: actor.clone(),
        input_hash,
    };
    let audit = NewAudit {
        actor_kind,
        actor_id: Some(authz.current.user.id.to_string()),
        action: "startDeployment".into(),
        target_kind: Some("app".into()),
        target_ref: Some(format!("{project}/{environment}/{app}")),
        outcome: "accepted".into(),
        ..NewAudit::default()
    };
    let summary = match tenant.start_deployment(&request, audit, key.as_ref()).await? {
        Started::Accepted { run, .. } => {
            let summary = tenant
                .run_of_target(t.target, run)
                .await?
                .ok_or_else(|| Error::Internal("the accepted run is missing".into()))?;
            tenant.commit().await?;
            summary
        }
        // Nothing of this request is kept: dropping the transaction rolls
        // back the release and revision written above.
        Started::Replayed(operation) => tenant
            .run_of_operation(operation)
            .await?
            .ok_or_else(|| Error::Internal("the replayed run is missing".into()))?,
        Started::KeyReused(_) => {
            return Err(
                Error::Conflict("this Idempotency-Key was used for a different request".into()).into(),
            );
        }
        Started::Rejected(reject) => return Err(Error::Conflict(reject.to_string()).into()),
        Started::NotFound => {
            return Err(Error::NotFound("that release or configuration of this app".into()).into());
        }
    };
    let location = format!(
        "/api/v1/projects/{project}/environments/{environment}/apps/{app}/deployments/{}",
        summary.run
    );
    Ok((
        StatusCode::ACCEPTED,
        [(header::LOCATION, location)],
        Json(DeploymentDto::from(summary)),
    ))
}

/// One step of a run's timeline.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct PhaseStep {
    pub phase: String,
    /// When the run entered it, Unix milliseconds.
    pub at: i64,
}

/// A deployment run with how it went.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct DeploymentSummary {
    pub run: Uuid,
    /// The target generation (the app's revision) this run owns.
    pub generation: u64,
    /// `deploy`, `rollback` or `promotion`.
    pub reason: String,
    pub phase: String,
    /// `succeeded`, `failed` or `cancelled` once it ended.
    pub outcome: Option<String>,
    /// Email of whoever asked for it.
    pub requested_by: String,
    pub created_at: i64,
    pub image: Option<String>,
    /// Every phase it entered, oldest first.
    pub timeline: Vec<PhaseStep>,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct DeploymentsQuery {
    /// How many runs, newest first (1–50, default 10).
    pub limit: Option<i64>,
}

/// The app's newest deployment runs, each with its timeline.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments", operation_id = "listDeployments",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        DeploymentsQuery,
    ),
    responses(
        (status = 200, body = Vec<DeploymentSummary>),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Query(q): Query<DeploymentsQuery>,
) -> ApiResult<Json<Vec<DeploymentSummary>>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &t.chain())?;
    let mut tenant = state.store.tenant(t.org).await?;
    let runs = tenant.runs(t.target, q.limit.unwrap_or(10).clamp(1, 50)).await?;
    let ids: Vec<DeploymentRunId> = runs.iter().map(|r| r.run).collect();
    let mut phases = tenant.run_phases(t.target, &ids).await?;
    drop(tenant);
    let actors = Actors::load(&state).await?;
    Ok(Json(
        runs.into_iter()
            .map(|r| DeploymentSummary {
                run: *r.run.as_uuid(),
                generation: r.generation.0,
                reason: r.reason,
                phase: r.phase.as_str().to_owned(),
                outcome: r.outcome,
                requested_by: actors.name(&r.requested_by),
                created_at: r.created_at,
                image: r.image,
                timeline: phases
                    .remove(&r.run)
                    .unwrap_or_default()
                    .into_iter()
                    .map(|(phase, at)| PhaseStep { phase, at })
                    .collect(),
            })
            .collect(),
    ))
}

/// One deployment run of this app.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}", operation_id = "getDeployment",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        ("run" = Uuid, Path, description = "Deployment run id"),
    ),
    responses(
        (status = 200, body = DeploymentDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
    )
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app, run)): Path<(String, String, String, Uuid)>,
) -> ApiResult<Json<DeploymentDto>> {
    let t = scope::sql_target(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &t.chain())?;
    let mut tenant = state.store.tenant(t.org).await?;
    let summary = tenant
        .run_of_target(t.target, DeploymentRunId::from_uuid(run))
        .await?
        .ok_or_else(|| Error::NotFound(format!("deployment `{run}`")))?;
    Ok(Json(summary.into()))
}

#[cfg(test)]
mod tests {
    use super::*;

    const DIGEST: &str = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    #[test]
    fn images_must_be_pinned_by_digest() {
        let (repository, digest) = pinned_image(&format!("ghcr.io/acme/api@{DIGEST}")).expect("pinned");
        assert_eq!(repository, "ghcr.io/acme/api");
        assert_eq!(digest.as_str(), DIGEST);
        for bad in [
            "ghcr.io/acme/api:1.2",
            "ghcr.io/acme/api@latest",
            &format!("@{DIGEST}"),
        ] {
            assert!(pinned_image(bad).is_err(), "{bad} is not pinned");
        }
    }

    #[test]
    fn idempotency_keys_are_bounded() {
        let mut headers = HeaderMap::new();
        assert!(idempotency_key(&headers, "user:a").expect("none").is_none());
        headers.insert(IDEMPOTENCY_KEY, "deploy-42".parse().expect("header"));
        assert_eq!(
            idempotency_key(&headers, "user:a")
                .expect("key")
                .expect("some")
                .key,
            "deploy-42"
        );
        headers.insert(IDEMPOTENCY_KEY, "x".repeat(201).parse().expect("header"));
        assert!(idempotency_key(&headers, "user:a").is_err());
    }
}

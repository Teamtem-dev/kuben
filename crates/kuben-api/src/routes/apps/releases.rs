//! Release history and rollback (scenario 5), from the app's deployment
//! runs: each run is a revision, numbered by the target generation it owns.

use std::collections::HashMap;

use axum::{
    Json,
    extract::{Path, State},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::AppSpec;
use kuben_store::repo::{RunReason, RunRecord};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{AppDto, Artifact, Change, deploy, desired_spec, spec::validate_spec, spec_of};
use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

const RELEASE_PAGE: i64 = 50;
/// How far back a rollback may reach.
const ROLLBACK_REACH: i64 = 1000;

#[derive(Debug, Serialize, ToSchema)]
pub struct ReleaseDto {
    pub revision: i64,
    pub image: Option<String>,
    /// `create`, `deploy`, `config`, `rollback`, `promote` or `restart`.
    pub reason: String,
    pub note: Option<String>,
    /// Email of whoever made the change.
    pub actor: Option<String>,
    pub created_at: i64,
    /// The newest revision (what should be running).
    pub current: bool,
}

/// The reason shown for `run`, told apart from the run before it: the first
/// run created the app; a deploy of the same release changed the
/// configuration only.
fn reason(run: &RunRecord, previous: Option<&RunRecord>) -> &'static str {
    match run.reason.as_str() {
        "rollback" => "rollback",
        "promotion" => "promote",
        "restart" => "restart",
        _ => match previous {
            None => "create",
            Some(p) if p.release == run.release => "config",
            Some(_) => "deploy",
        },
    }
}

/// Release history, newest first (50 revisions).
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/releases", operation_id = "listReleases",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = Vec<ReleaseDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn releases(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<Vec<ReleaseDto>>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    // One more than shown: the oldest shown run needs its predecessor.
    let runs = tenant.runs(a.app.target, RELEASE_PAGE + 1).await?;
    drop(tenant);
    let emails: HashMap<String, String> = state
        .store
        .list_users()
        .await?
        .into_iter()
        .map(|u| (u.id.to_string(), u.email))
        .collect();
    let shown = usize::try_from(RELEASE_PAGE).unwrap_or(usize::MAX);
    Ok(Json(
        runs.iter()
            .enumerate()
            .take(shown)
            .map(|(i, run)| {
                let id = run
                    .requested_by
                    .split_once(':')
                    .map_or(run.requested_by.as_str(), |(_, id)| id);
                ReleaseDto {
                    revision: i64::try_from(run.generation.0).unwrap_or(i64::MAX),
                    image: run.image.clone(),
                    reason: reason(run, runs.get(i + 1)).to_owned(),
                    note: None,
                    actor: Some(emails.get(id).cloned().unwrap_or_else(|| id.to_owned())),
                    created_at: run.created_at,
                    current: i == 0,
                }
            })
            .collect(),
    ))
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct Rollback {
    /// Revision to restore.
    pub revision: i64,
}

/// What a rollback restores: image, processes and env of the revision;
/// domains and volumes stay as they are now (data layout never moves back).
#[must_use]
pub fn rollback_spec(current: &AppSpec, revision: AppSpec) -> AppSpec {
    AppSpec {
        source: revision.source,
        runtime: revision.runtime,
        env: revision.env,
        domains: current.domains.clone(),
        volumes: current.volumes.clone(),
    }
}

/// Roll back to an earlier revision: a new run of its release and
/// configuration. The app stays on it until automatic deploys resume.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/rollback", operation_id = "rollbackApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = Rollback,
    responses(
        (status = 200, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn rollback(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<Rollback>,
) -> ApiResult<Json<AppDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let not_found = || Error::NotFound(format!("revision {}", body.revision));
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let run = tenant
        .runs(a.app.target, ROLLBACK_REACH)
        .await?
        .into_iter()
        .find(|r| i64::try_from(r.generation.0).ok() == Some(body.revision))
        .ok_or_else(not_found)?;
    let config = tenant
        .config_revision(a.app.target, run.config_revision)
        .await?
        .ok_or_else(not_found)?;
    let restored = spec_of(Some(config), run.image.as_deref())
        .ok_or_else(|| Error::Validation(format!("revision {} cannot be restored", body.revision)))?;
    let current = desired_spec(&a.app)
        .ok_or_else(|| Error::Conflict(format!("app `{app}` has no configuration yet")))?;
    let spec = rollback_spec(&current, restored);
    validate_spec(&spec)?;
    let change = Change {
        project: a.env.project.id(),
        application: a.app.application,
        target: a.app.target,
        spec: &spec,
        artifact: Artifact::Release(run.release),
        expected: a.app.desired_generation,
        reason: RunReason::Rollback,
        reference: format!("{project}/{environment}/{app}"),
    };
    deploy(&mut tenant, &authz, change).await?;
    let record = tenant
        .app(a.env.id(), a.slug())
        .await?
        .ok_or_else(|| Error::NotFound(format!("app `{app}`")))?;
    tenant.commit().await?;
    Ok(Json(AppDto::of(
        a.env.project.slug(),
        a.env.short_name(),
        &record,
        a.view.as_deref(),
    )))
}

#[cfg(test)]
mod tests {
    use kuben_core::{
        ids::{ConfigRevisionId, DeploymentRunId, ReleaseId},
        ops::{Generation, RunPhase},
    };
    use kuben_crd::Source;

    use super::{super::sample_spec, *};
    use crate::routes::apps::spec::to_domains;

    #[test]
    fn rollback_keeps_domains_and_volumes() {
        let mut current = sample_spec();
        current.domains = to_domains(&["api.acme.com".to_owned()]);
        let mut old = sample_spec();
        old.source = Source::from_image("nginx:1.25");
        let restored = rollback_spec(&current, old);
        assert_eq!(restored.source.image.as_deref(), Some("nginx:1.25"));
        assert_eq!(restored.domains[0].host, "api.acme.com");
    }

    #[test]
    fn reasons_follow_the_runs() {
        let run = |generation: u64, reason: &str, release: ReleaseId| RunRecord {
            run: DeploymentRunId::new(),
            generation: Generation(generation),
            reason: reason.into(),
            phase: RunPhase::Succeeded,
            requested_by: "user:x".into(),
            created_at: 0,
            release,
            config_revision: ConfigRevisionId::new(),
            image: None,
        };
        let (first, second) = (ReleaseId::new(), ReleaseId::new());
        let created = run(1, "deploy", first);
        let scaled = run(2, "deploy", first);
        let upgraded = run(3, "deploy", second);
        assert_eq!(reason(&created, None), "create");
        assert_eq!(reason(&scaled, Some(&created)), "config");
        assert_eq!(reason(&upgraded, Some(&scaled)), "deploy");
        assert_eq!(reason(&run(4, "rollback", first), Some(&upgraded)), "rollback");
        assert_eq!(reason(&run(1, "promotion", first), None), "promote");
        assert_eq!(
            reason(&run(2, "restart", first), Some(&run(1, "deploy", first))),
            "restart"
        );
    }
}

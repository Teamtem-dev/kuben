//! Operational controls (M4.9; S05): owners, change freezes, paused
//! delivery, alert silences and emergency rollbacks. Each has one effect:
//!
//! * an owner names who answers for a project or an application;
//! * a freeze refuses new releases to an environment (secret rotations and
//!   restarts still pass, and so does an emergency rollback);
//! * a pause holds an app's delivery: runs are accepted but not written, and
//!   on resume only the newest is;
//! * a silence keeps alerts quiet, without hiding what happened;
//! * an emergency rollback is a person's break-glass: it returns an app to an
//!   earlier release past approvals, a freeze, the scan gate and a pause,
//!   with a reason, audited.

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use k8s_openapi::jiff::Timestamp;
use kuben_core::{
    Error,
    ids::{DeploymentRunId, ReleaseId},
    perm::Perm,
    time::now_ms,
};
use kuben_store::repo::{Freeze, NewWindow, Owner, RunReason, Silence, StartDeployment, Started};
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::{
    apps::deployments::DeploymentDto,
    request,
    scope::{self, EnvScope},
};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Longest freeze.
const MAX_FREEZE_MS: i64 = 30 * 86_400_000;
/// Longest silence.
const MAX_SILENCE_MS: i64 = 7 * 86_400_000;

#[derive(Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct OwnerDto {
    /// A team or a person.
    #[schema(example = "payments-team")]
    pub owner: String,
    /// How to reach them: an email, a chat channel, an on-call rotation.
    pub contact: Option<String>,
    pub runbook_url: Option<String>,
    #[serde(default, skip_deserializing)]
    pub updated_by: Option<String>,
    #[serde(default, skip_deserializing)]
    pub updated_at: Option<String>,
}

impl From<Owner> for OwnerDto {
    fn from(o: Owner) -> Self {
        Self {
            owner: o.owner,
            contact: o.contact,
            runbook_url: o.runbook_url,
            updated_by: Some(o.updated_by),
            updated_at: Some(request::timestamp(o.updated_at)),
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct CreateWindow {
    pub reason: String,
    /// RFC 3339; now when omitted (freezes only).
    pub starts_at: Option<String>,
    /// RFC 3339.
    pub ends_at: String,
    /// Silences only: one app of the environment.
    pub app: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct WindowDto {
    pub id: Uuid,
    pub reason: String,
    /// Silences only: the app, when not the whole environment.
    pub app: Option<Uuid>,
    pub created_by: String,
    pub starts_at: String,
    pub ends_at: String,
    pub lifted_at: Option<String>,
    /// In force now.
    pub active: bool,
}

impl WindowDto {
    fn of_freeze(f: Freeze, now: i64) -> Self {
        Self {
            active: f.lifted_at.is_none() && f.starts_at <= now && now < f.ends_at,
            id: f.id,
            reason: f.reason,
            app: None,
            created_by: f.created_by,
            starts_at: request::timestamp(f.starts_at),
            ends_at: request::timestamp(f.ends_at),
            lifted_at: f.lifted_at.map(request::timestamp),
        }
    }

    fn of_silence(s: Silence, now: i64) -> Self {
        Self {
            active: s.lifted_at.is_none() && now < s.ends_at,
            id: s.id,
            reason: s.reason,
            app: s.target_id,
            created_by: s.created_by,
            starts_at: request::timestamp(s.created_at),
            ends_at: request::timestamp(s.ends_at),
            lifted_at: s.lifted_at.map(request::timestamp),
        }
    }
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct HistoryQuery {
    /// Include ended and lifted ones.
    #[serde(default)]
    pub all: bool,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Reason {
    pub reason: String,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct EmergencyRollback {
    /// Why this cannot wait; kept on the run and in the audit log.
    pub reason: String,
    /// The release to return to; the newest earlier release that ran
    /// successfully when omitted.
    pub release: Option<Uuid>,
}

fn text(name: &str, value: &str, max: usize) -> Result<String, Error> {
    let value = value.trim();
    if value.is_empty() || value.chars().count() > max || value.chars().any(char::is_control) {
        return Err(Error::Validation(format!(
            "{name} must be 1 to {max} printable characters"
        )));
    }
    Ok(value.to_owned())
}

fn millis(name: &str, value: &str) -> Result<i64, Error> {
    value
        .parse::<Timestamp>()
        .map(Timestamp::as_millisecond)
        .map_err(|_| Error::Validation(format!("{name} is not an RFC 3339 time")))
}

/// The window `body` asks for, from `now`, at most `max` long.
fn window(body: &CreateWindow, now: i64, max: i64) -> Result<(i64, i64), Error> {
    let starts = body
        .starts_at
        .as_deref()
        .map(|s| millis("startsAt", s))
        .transpose()?
        .unwrap_or(now);
    let ends = millis("endsAt", &body.ends_at)?;
    if ends <= starts.max(now) {
        return Err(Error::Validation(
            "endsAt must be in the future and after startsAt".into(),
        ));
    }
    if ends - starts.max(now) > max || starts - now > max {
        return Err(Error::Validation(format!(
            "a window lasts at most {} days",
            max / 86_400_000
        )));
    }
    Ok((starts, ends))
}

fn check_owner(body: &OwnerDto) -> Result<(String, Option<String>, Option<String>), Error> {
    let owner = text("owner", &body.owner, 256)?;
    let contact = body
        .contact
        .as_deref()
        .map(|c| text("contact", c, 512))
        .transpose()?;
    let runbook = body.runbook_url.as_deref().map(str::trim).map(str::to_owned);
    if let Some(url) = &runbook
        && !((url.starts_with("https://") || url.starts_with("http://")) && url.len() <= 2048)
    {
        return Err(Error::Validation("runbookUrl must be an http(s) URL".into()));
    }
    Ok((owner, contact, runbook))
}

/// Who answers for a project.
#[utoipa::path(
    get, path = "/projects/{project}/owner", operation_id = "getProjectOwner", tag = "projects",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 200, body = Option<OwnerDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn get_project_owner(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<Option<OwnerDto>>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    Ok(Json(tenant.owner(p.id(), None).await?.map(OwnerDto::from)))
}

/// Name who answers for a project.
#[utoipa::path(
    put, path = "/projects/{project}/owner", operation_id = "putProjectOwner", tag = "projects",
    params(("project" = String, Path, description = "Project name")),
    request_body = OwnerDto,
    responses((status = 200, body = OwnerDto), (status = 403, body = crate::error::Problem), (status = 422, body = crate::error::Problem))
)]
pub async fn put_project_owner(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Json(body): Json<OwnerDto>,
) -> ApiResult<Json<OwnerDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    let (owner, contact, runbook) = check_owner(&body)?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(p.org).await?;
    tenant
        .set_owner(
            p.id(),
            None,
            Some((&owner, contact.as_deref(), runbook.as_deref())),
            &actor,
        )
        .await?;
    let saved = tenant
        .owner(p.id(), None)
        .await?
        .ok_or_else(|| Error::Internal("the owner is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(saved.into()))
}

/// Who answers for an application (in every environment).
#[utoipa::path(
    get, path = "/projects/{project}/applications/{app}/owner", operation_id = "getApplicationOwner", tag = "apps",
    params(("project" = String, Path, description = "Project name"), ("app" = String, Path, description = "App name")),
    responses((status = 200, body = Option<OwnerDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn get_app_owner(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, app)): Path<(String, String)>,
) -> ApiResult<Json<Option<OwnerDto>>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::AppRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let application = tenant
        .application(p.id(), &app)
        .await?
        .ok_or_else(|| Error::NotFound(format!("app `{app}`")))?;
    Ok(Json(
        tenant.owner(p.id(), Some(application)).await?.map(OwnerDto::from),
    ))
}

/// Name who answers for an application.
#[utoipa::path(
    put, path = "/projects/{project}/applications/{app}/owner", operation_id = "putApplicationOwner", tag = "apps",
    params(("project" = String, Path, description = "Project name"), ("app" = String, Path, description = "App name")),
    request_body = OwnerDto,
    responses((status = 200, body = OwnerDto), (status = 403, body = crate::error::Problem), (status = 404, body = crate::error::Problem))
)]
pub async fn put_app_owner(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, app)): Path<(String, String)>,
    Json(body): Json<OwnerDto>,
) -> ApiResult<Json<OwnerDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &p.chain())?;
    let (owner, contact, runbook) = check_owner(&body)?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(p.org).await?;
    let application = tenant
        .application(p.id(), &app)
        .await?
        .ok_or_else(|| Error::NotFound(format!("app `{app}`")))?;
    tenant
        .set_owner(
            p.id(),
            Some(application),
            Some((&owner, contact.as_deref(), runbook.as_deref())),
            &actor,
        )
        .await?;
    let saved = tenant
        .owner(p.id(), Some(application))
        .await?
        .ok_or_else(|| Error::Internal("the owner is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(saved.into()))
}

/// The environment's change freezes in force or to come.
#[utoipa::path(
    get, path = "/projects/{project}/environments/{environment}/freezes", operation_id = "listFreezes", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        HistoryQuery,
    ),
    responses((status = 200, body = Vec<WindowDto>))
)]
pub async fn list_freezes(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Query(q): Query<HistoryQuery>,
) -> ApiResult<Json<Vec<WindowDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let now = now_ms();
    let found = tenant.freezes(e.id(), q.all).await?;
    Ok(Json(
        found.into_iter().map(|f| WindowDto::of_freeze(f, now)).collect(),
    ))
}

/// Freeze an environment: new releases are refused until it ends.
#[utoipa::path(
    post, path = "/projects/{project}/environments/{environment}/freezes", operation_id = "createFreeze", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    request_body = CreateWindow,
    responses((status = 201, body = WindowDto), (status = 403, body = crate::error::Problem), (status = 422, body = crate::error::Problem))
)]
pub async fn create_freeze(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<CreateWindow>,
) -> ApiResult<(StatusCode, Json<WindowDto>)> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvWrite, &e.chain())?;
    if body.app.is_some() {
        return Err(Error::Validation("a freeze holds the whole environment".into()).into());
    }
    let now = now_ms();
    let (starts_at, ends_at) = window(&body, now, MAX_FREEZE_MS)?;
    let new = new_window(&authz, &e, &body.reason, None, starts_at, ends_at)?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let id = tenant.create_freeze(&new).await?;
    audit(&mut tenant, &authz, &e, "environment.frozen", &new.reason).await?;
    let made = tenant
        .freezes(e.id(), true)
        .await?
        .into_iter()
        .find(|f| f.id == id)
        .ok_or_else(|| Error::Internal("the new freeze is missing".into()))?;
    tenant.commit().await?;
    Ok((StatusCode::CREATED, Json(WindowDto::of_freeze(made, now))))
}

fn new_window(
    authz: &Authz,
    e: &EnvScope,
    reason: &str,
    target: Option<kuben_core::ids::TargetId>,
    starts_at: i64,
    ends_at: i64,
) -> Result<NewWindow, Error> {
    let (_, actor) = request::actor(authz);
    Ok(NewWindow {
        project: e.project.id(),
        environment: e.id(),
        target,
        reason: text("reason", reason, 1024)?,
        created_by: actor,
        starts_at,
        ends_at,
    })
}

async fn audit(
    tenant: &mut kuben_store::repo::Tenant,
    authz: &Authz,
    e: &EnvScope,
    action: &str,
    reason: &str,
) -> ApiResult<()> {
    let mut record = request::audit(
        authz,
        action,
        "environment",
        format!("{}/{}", e.project.slug(), e.short_name()),
    );
    record.data = Some(serde_json::json!({ "reason": reason }));
    tenant.append_audit(record).await?;
    Ok(())
}

/// Lift a freeze early.
#[utoipa::path(
    delete, path = "/projects/{project}/environments/{environment}/freezes/{id}", operation_id = "liftFreeze", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("id" = Uuid, Path, description = "Freeze id"),
    ),
    responses((status = 204, description = "Lifted"), (status = 404, body = crate::error::Problem))
)]
pub async fn lift_freeze(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, id)): Path<(String, String, Uuid)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvWrite, &e.chain())?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(e.project.org).await?;
    if !tenant.lift_freeze(e.id(), id, &actor).await? {
        return Err(Error::NotFound(format!("freeze `{id}` in force")).into());
    }
    audit(
        &mut tenant,
        &authz,
        &e,
        "environment.freeze_lifted",
        &id.to_string(),
    )
    .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// The environment's alert silences in force.
#[utoipa::path(
    get, path = "/projects/{project}/environments/{environment}/silences", operation_id = "listSilences", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        HistoryQuery,
    ),
    responses((status = 200, body = Vec<WindowDto>))
)]
pub async fn list_silences(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Query(q): Query<HistoryQuery>,
) -> ApiResult<Json<Vec<WindowDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let now = now_ms();
    let found = tenant.silences(e.id(), q.all).await?;
    Ok(Json(
        found.into_iter().map(|s| WindowDto::of_silence(s, now)).collect(),
    ))
}

/// Silence the alerts of an environment, or of one of its apps.
#[utoipa::path(
    post, path = "/projects/{project}/environments/{environment}/silences", operation_id = "createSilence", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    request_body = CreateWindow,
    responses((status = 201, body = WindowDto), (status = 403, body = crate::error::Problem), (status = 422, body = crate::error::Problem))
)]
pub async fn create_silence(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<CreateWindow>,
) -> ApiResult<(StatusCode, Json<WindowDto>)> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &e.chain())?;
    if body.starts_at.is_some() {
        return Err(Error::Validation("a silence starts now".into()).into());
    }
    let now = now_ms();
    let (_, ends_at) = window(&body, now, MAX_SILENCE_MS)?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let target = match body.app.as_deref() {
        None => None,
        Some(app) => Some(
            tenant
                .app(e.id(), app)
                .await?
                .ok_or_else(|| Error::NotFound(format!("app `{app}`")))?
                .target,
        ),
    };
    let new = new_window(&authz, &e, &body.reason, target, now, ends_at)?;
    let id = tenant.create_silence(&new).await?;
    audit(&mut tenant, &authz, &e, "alerts.silenced", &new.reason).await?;
    let made = tenant
        .silences(e.id(), true)
        .await?
        .into_iter()
        .find(|s| s.id == id)
        .ok_or_else(|| Error::Internal("the new silence is missing".into()))?;
    tenant.commit().await?;
    Ok((StatusCode::CREATED, Json(WindowDto::of_silence(made, now))))
}

/// Lift a silence early.
#[utoipa::path(
    delete, path = "/projects/{project}/environments/{environment}/silences/{id}", operation_id = "liftSilence", tag = "environments",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("id" = Uuid, Path, description = "Silence id"),
    ),
    responses((status = 204, description = "Lifted"), (status = 404, body = crate::error::Problem))
)]
pub async fn lift_silence(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, id)): Path<(String, String, Uuid)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &e.chain())?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(e.project.org).await?;
    if !tenant.lift_silence(e.id(), id, &actor).await? {
        return Err(Error::NotFound(format!("silence `{id}` in force")).into());
    }
    audit(&mut tenant, &authz, &e, "alerts.silence_lifted", &id.to_string()).await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Hold an app's delivery: new runs are accepted and wait.
#[utoipa::path(
    post, path = "/projects/{project}/environments/{environment}/apps/{app}/pause", operation_id = "pauseApp", tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = Reason,
    responses((status = 204, description = "Paused"), (status = 409, body = crate::error::Problem, description = "Paused already"))
)]
pub async fn pause(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<Reason>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let reason = text("reason", &body.reason, 1024)?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if !tenant.pause_target(a.app.target, &actor, &reason).await? {
        return Err(Error::Conflict(format!("app `{app}` is paused already or being deleted")).into());
    }
    audit(&mut tenant, &authz, &a.env, "app.paused", &reason).await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Resume an app's delivery: its newest waiting run is written.
#[utoipa::path(
    post, path = "/projects/{project}/environments/{environment}/apps/{app}/resume", operation_id = "resumeApp", tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 204, description = "Resumed"), (status = 409, body = crate::error::Problem, description = "Not paused"))
)]
pub async fn resume(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if !tenant.resume_target(a.app.target).await? {
        return Err(Error::Conflict(format!("app `{app}` is not paused")).into());
    }
    audit(&mut tenant, &authz, &a.env, "app.resumed", &app).await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Return an app to an earlier release now, past approvals, a freeze, the
/// scan gate and a pause. People with approval rights only; the reason is
/// kept.
#[utoipa::path(
    post, path = "/projects/{project}/environments/{environment}/apps/{app}/emergency-rollback", operation_id = "emergencyRollback", tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = EmergencyRollback,
    responses(
        (status = 202, body = DeploymentDto),
        (status = 403, body = crate::error::Problem),
        (status = 404, body = crate::error::Problem, description = "No earlier release ran successfully"),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn emergency_rollback(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<EmergencyRollback>,
) -> ApiResult<(StatusCode, Json<DeploymentDto>)> {
    authz.forbid_token()?;
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _deploy = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    let _break_glass = authz.require(&state, Perm::ReleaseApprove, &a.chain())?;
    let why = text("reason", &body.reason, 1024)?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let (release, config_revision) = tenant
        .rollback_point(a.app.target, body.release.map(ReleaseId::from_uuid))
        .await?
        .ok_or_else(|| Error::NotFound(format!("an earlier release of `{app}` that ran successfully")))?;
    let (_, actor) = request::actor(&authz);
    let expected = a.app.desired_generation;
    let run = StartDeployment {
        project: a.env.project.id(),
        target: a.app.target,
        release,
        config_revision,
        render_plan: None,
        expected_generation: expected,
        lifecycle_uid: a.app.lifecycle_uid,
        reason: RunReason::Emergency,
        requested_by: actor,
        input_hash: Sha256::digest(format!("emergency/{release}/{config_revision}/{}", expected.0)).to_vec(),
    };
    let mut record = request::audit(
        &authz,
        "deployment.emergency_rollback",
        "app",
        format!("{project}/{environment}/{app}"),
    );
    record.data = Some(serde_json::json!({ "reason": why, "release": release }));
    let run: DeploymentRunId = match tenant.start_emergency_rollback(&run, &why, record).await? {
        Started::Accepted { run, .. } => run,
        Started::Rejected(reject) => return Err(Error::Conflict(reject.to_string()).into()),
        Started::SecretRevoked => return Err(super::apps::secret_revoked().into()),
        other => return Err(Error::Conflict(format!("the rollback was not accepted: {other:?}")).into()),
    };
    let summary = tenant
        .run_of_target(a.app.target, run)
        .await?
        .ok_or_else(|| Error::Internal("the accepted run is missing".into()))?;
    tenant.commit().await?;
    tracing::warn!(app = %format!("{project}/{environment}/{app}"), %release, reason = %why, "emergency rollback");
    Ok((StatusCode::ACCEPTED, Json(DeploymentDto::from(summary))))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn body(starts: Option<&str>, ends: &str) -> CreateWindow {
        CreateWindow {
            reason: "release freeze".into(),
            starts_at: starts.map(str::to_owned),
            ends_at: ends.into(),
            app: None,
        }
    }

    #[test]
    fn windows_are_bounded() {
        let now = millis("now", "2026-09-17T12:00:00Z").expect("time");
        assert_eq!(
            window(&body(None, "2026-09-18T12:00:00Z"), now, MAX_FREEZE_MS).ok(),
            Some((now, now + 86_400_000))
        );
        let later = window(
            &body(Some("2026-09-20T00:00:00Z"), "2026-09-21T00:00:00Z"),
            now,
            MAX_FREEZE_MS,
        );
        assert!(later.is_ok(), "a freeze to come");
        for bad in [
            body(None, "2026-09-17T11:00:00Z"),
            body(Some("2026-09-19T00:00:00Z"), "2026-09-18T00:00:00Z"),
            body(None, "2026-12-01T00:00:00Z"),
            body(None, "tomorrow"),
        ] {
            assert!(window(&bad, now, MAX_FREEZE_MS).is_err(), "{bad:?}");
        }
        assert!(window(&body(None, "2026-09-26T00:00:00Z"), now, MAX_SILENCE_MS).is_err());
    }

    #[test]
    fn owners_and_reasons_are_checked() {
        let owner = |owner: &str, runbook: Option<&str>| OwnerDto {
            owner: owner.into(),
            contact: Some("#payments".into()),
            runbook_url: runbook.map(str::to_owned),
            updated_by: None,
            updated_at: None,
        };
        assert!(check_owner(&owner("payments", Some("https://wiki/pay"))).is_ok());
        assert!(check_owner(&owner(" ", None)).is_err());
        assert!(check_owner(&owner("payments", Some("javascript:alert(1)"))).is_err());
        assert_eq!(text("reason", "  outage  ", 10).ok().as_deref(), Some("outage"));
        assert!(text("reason", "a\nb", 10).is_err());
    }
}

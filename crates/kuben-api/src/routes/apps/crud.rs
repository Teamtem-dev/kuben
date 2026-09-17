//! Apps on the SQL model: create, read, update and delete, and rolling
//! restart, which acts on the materialized workloads directly (an app its
//! cluster's agent delivers restarts through a run instead).

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use kube::{
    Api,
    api::{Patch, PatchParams},
};
use kuben_core::{Error, perm::Perm};
use kuben_crd::App;
use kuben_platform::controller::resources::RESTARTED_AT;
use kuben_store::repo::{Delivery, RunReason, StartDeployment, Subject, TARGET_DELETE};
use serde::Deserialize;
use serde_json::json;
use sha2::{Digest as _, Sha256};
use utoipa::IntoParams;

use super::{
    AppDetail, AppDto, Artifact, Change, PodDto, create_app, deploy, desired_spec, resolve,
    spec::{
        CreateApp, UpdateApp, apply_update, ensure_domains_free, from_crd_env, spec_from_create,
        validate_spec,
    },
    spec_json, started,
};
use crate::{
    authz::Authz,
    error::ApiResult,
    routes::{request, scope, validate},
    state::ApiState,
};

/// List the apps of an environment.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps", operation_id = "listApps",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<AppDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<AppDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let items = tenant
        .apps(e.id())
        .await?
        .iter()
        .map(|a| {
            let view = state.projections.app(&a.namespace, &a.slug);
            AppDto::of(e.project.slug(), e.short_name(), a, view.as_deref()).with_exposure(&state.projections)
        })
        .collect();
    Ok(Json(items))
}

/// Deploy a new app from a container image, or from a Git repository. A tag
/// is resolved to a digest at its registry; a Git app deploys with its first
/// build.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps", operation_id = "createApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    request_body = CreateApp,
    responses(
        (status = 201, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, description = "The image's registry cannot be reached", body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
    Json(body): Json<CreateApp>,
) -> ApiResult<(StatusCode, Json<AppDto>)> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &e.chain())?;
    validate::dns_label("name", &body.name, 40)?;
    let spec = spec_from_create(&body)?;
    let dto = match &body.git {
        Some(git) => super::source::create_git_app(&state, &authz, &e, &body.name, spec, git).await?,
        None => create_app(&state, &authz, &e, &body.name, spec).await?,
    };
    Ok((StatusCode::CREATED, Json(dto)))
}

/// App detail with pods. Plain env values are included only for callers
/// holding `secret-read`.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}", operation_id = "getApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = AppDetail), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<AppDetail>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let with_values = authz.require(&state, Perm::SecretRead, &a.chain()).is_ok();
    let mut dto = AppDto::of(
        a.env.project.slug(),
        a.env.short_name(),
        &a.app,
        a.view.as_deref(),
    )
    .with_exposure(&state.projections);
    if let Some(spec) = desired_spec(&a.app) {
        dto.env = spec.env.iter().map(|e| from_crd_env(e, with_values)).collect();
    }
    let pods = state
        .projections
        .pods_of_app(a.namespace(), a.slug())
        .iter()
        .map(|p| PodDto::from(&**p))
        .collect();
    Ok(Json(AppDetail { app: dto, pods }))
}

/// Update an app (image changes require `app-deploy`). Every change is a new
/// deployment run; a new tag is resolved to a digest at its registry.
#[utoipa::path(
    patch,
    path = "/projects/{project}/environments/{environment}/apps/{app}", operation_id = "updateApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = UpdateApp,
    responses(
        (status = 200, body = AppDto),
        (status = 403, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem),
        (status = 503, description = "The image's registry cannot be reached", body = crate::error::Problem),
    )
)]
pub async fn update(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<UpdateApp>,
) -> ApiResult<Json<AppDto>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let perm = if body.image.is_some() {
        Perm::AppDeploy
    } else {
        Perm::AppWrite
    };
    let _proof = authz.require(&state, perm, &a.chain())?;
    if a.deleting() {
        return Err(Error::Conflict(format!("app `{app}` is being deleted")).into());
    }
    let mut spec = desired_spec(&a.app)
        .ok_or_else(|| Error::Conflict(format!("app `{app}` has no configuration yet")))?;
    let before = spec_json(&spec);
    let image = body.image.as_deref().map(str::trim).map(str::to_owned);
    apply_update(&mut spec, body)?;
    validate_spec(&spec)?;
    ensure_domains_free(&state, a.env.project.org, a.namespace(), a.slug(), &spec).await?;
    if spec_json(&spec) == before {
        return Ok(Json(AppDto::of(
            a.env.project.slug(),
            a.env.short_name(),
            &a.app,
            a.view.as_deref(),
        )));
    }
    let artifact = match image.filter(|i| Some(i.as_str()) != a.app.image.as_deref()) {
        Some(image) => Artifact::Resolved(resolve(&state, &a.env, &image).await?),
        None => Artifact::Release(
            a.app
                .release
                .ok_or_else(|| Error::Conflict(format!("app `{app}` has no release yet")))?,
        ),
    };
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let change = Change {
        project: a.env.project.id(),
        application: a.app.application,
        target: a.app.target,
        spec: &spec,
        artifact,
        expected: a.app.desired_generation,
        reason: RunReason::Deploy,
        reference: format!("{project}/{environment}/{app}"),
        chain: a.chain(),
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

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct DeleteAppQuery {
    /// Also delete the app's persistent volumes. Irreversible.
    pub delete_volumes: Option<bool>,
}

/// Delete an app and everything it owns. Volumes are kept unless
/// `delete_volumes=true`.
#[utoipa::path(
    delete,
    path = "/projects/{project}/environments/{environment}/apps/{app}", operation_id = "deleteApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
        DeleteAppQuery,
    ),
    responses(
        (status = 204, description = "Deleted"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem),
    )
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Query(q): Query<DeleteAppQuery>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &a.chain())?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if !tenant.mark_target_deleting(a.app.target).await? {
        return Err(Error::Conflict(format!("app `{app}` is being deleted")).into());
    }
    let (_, actor) = request::actor(&authz);
    let subject = Subject::target(
        a.env.project.id(),
        a.env.id(),
        a.app.target,
        q.delete_volumes == Some(true),
    );
    tenant
        .request(
            TARGET_DELETE,
            subject,
            &actor,
            request::audit(
                &authz,
                TARGET_DELETE,
                "app",
                format!("{project}/{environment}/{app}"),
            ),
        )
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Rolling restart of every process (no spec change).
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/restart", operation_id = "restartApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 202, description = "Restart scheduled"), (status = 404, body = crate::error::Problem))
)]
pub async fn restart(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    if a.app.delivery == Delivery::Agent {
        // No App object to annotate: a run of the same release and
        // configuration stamps every pod template instead (M1.9).
        let run = rerun(&a, &authz, RunReason::Restart, &app)?;
        let audit = request::audit(
            &authz,
            "deployment.accepted",
            "app",
            format!("{project}/{environment}/{app}"),
        );
        let mut tenant = state.store.tenant(a.env.project.org).await?;
        super::approvals::ensure_may_deploy(&mut tenant, &authz, a.app.target, &a.chain()).await?;
        started(tenant.start_deployment(&run, audit, None).await?)?;
        tenant.commit().await?;
        return Ok(StatusCode::ACCEPTED);
    }
    let now = k8s_openapi::jiff::Timestamp::now()
        .strftime("%Y-%m-%dT%H:%M:%SZ")
        .to_string();
    // An annotation, not the spec: the materializer's drift check ignores it.
    let patch = json!({ "metadata": { "annotations": { RESTARTED_AT: now } } });
    Api::<App>::namespaced(scope::cluster(&state)?, a.namespace())
        .patch(a.slug(), &PatchParams::default(), &Patch::Merge(&patch))
        .await
        .map_err(|e| scope::kube_error(e, &app))?;
    Ok(StatusCode::ACCEPTED)
}

/// Hand an app over from the App controller to its cluster's agent: its
/// next run goes through the agent, which adopts the app's workloads in
/// place (no new pods), and every later run follows. Only toward the agent;
/// the cluster needs a linked agent that carries applications.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/handover", operation_id = "handOverApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses(
        (status = 202, description = "Handover scheduled"),
        (status = 404, body = crate::error::Problem),
        (status = 409, description = "Delivered by the agent already, being deleted, or no agent to take it", body = crate::error::Problem),
    )
)]
pub async fn hand_over(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<StatusCode> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppDeploy, &a.chain())?;
    if a.app.delivery == Delivery::Agent {
        return Err(
            Error::Conflict(format!("app `{app}` is delivered by its cluster's agent already")).into(),
        );
    }
    if a.deleting() {
        return Err(Error::Conflict(format!("app `{app}` is being deleted")).into());
    }
    let run = rerun(&a, &authz, RunReason::Handover, &app)?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    super::approvals::ensure_may_deploy(&mut tenant, &authz, a.app.target, &a.chain()).await?;
    if !tenant.hand_over_to_agent(a.app.target).await? {
        return Err(Error::Conflict(format!(
            "the cluster of app `{app}` has no linked agent that carries applications"
        ))
        .into());
    }
    let audit = request::audit(
        &authz,
        "deployment.accepted",
        "app",
        format!("{project}/{environment}/{app}"),
    );
    started(tenant.start_deployment(&run, audit, None).await?)?;
    tenant.commit().await?;
    Ok(StatusCode::ACCEPTED)
}

/// A run of `a`'s current release and configuration for `reason`: a restart
/// or a handover changes neither.
fn rerun(a: &scope::AppScope, authz: &Authz, reason: RunReason, app: &str) -> ApiResult<StartDeployment> {
    let (Some(release), Some(config_revision)) = (a.app.release, a.app.config_revision) else {
        return Err(Error::Conflict(format!("app `{app}` has no release yet")).into());
    };
    let (_, actor) = request::actor(authz);
    let expected = a.app.desired_generation;
    Ok(StartDeployment {
        project: a.env.project.id(),
        target: a.app.target,
        release,
        config_revision,
        render_plan: None,
        expected_generation: expected,
        lifecycle_uid: a.app.lifecycle_uid,
        reason,
        requested_by: actor,
        input_hash: Sha256::digest(format!(
            "{}/{release}/{config_revision}/{}",
            reason.as_str(),
            expected.0
        ))
        .to_vec(),
    })
}

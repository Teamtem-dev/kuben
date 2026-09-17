//! Public status pages (M5.3).
//!
//! A project can publish a page anyone may read without signing in. The page
//! shows:
//! - each app of the chosen environments, by its display name, with its
//!   state (`operational`, `degraded` or `majorOutage`);
//! - the incidents of those apps, open ones and those resolved in the last
//!   week, with their severity and times.
//!
//! It shows nothing else: no namespaces, pods, addresses, images, incident
//! texts or error codes. Answers are cached for a few seconds.

use std::{collections::HashMap, sync::Arc, time::Duration};

use axum::{
    Json,
    extract::{Path, State},
    http::{StatusCode, header},
    response::{IntoResponse, Response},
};
use kuben_core::{
    Error,
    perm::Perm,
    status::{Observed, ServiceStatus, component_status, page_status},
    time::now_ms,
};
use kuben_store::repo::{Delivery, StatusPage};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{request, scope, validate};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// How long a public answer is reused.
pub const CACHE_TTL: Duration = Duration::from_secs(15);
const WEEK_MS: i64 = 7 * 24 * 3_600_000;

/// The public status cache of a server.
pub type StatusCache = moka::future::Cache<String, Arc<PublicStatus>>;

#[must_use]
pub fn status_cache() -> StatusCache {
    moka::future::Cache::builder()
        .time_to_live(CACHE_TTL)
        .max_capacity(1_000)
        .build()
}

#[derive(Clone, Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PublicComponent {
    pub name: String,
    #[schema(value_type = String)]
    pub status: ServiceStatus,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PublicIncidentDto {
    /// The affected component.
    pub component: String,
    /// `critical`, `warning` or `info`.
    pub severity: String,
    pub started_at: String,
    pub resolved_at: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct PublicStatus {
    pub title: String,
    /// `operational`, `degraded` or `majorOutage`.
    #[schema(value_type = String)]
    pub status: ServiceStatus,
    pub components: Vec<PublicComponent>,
    pub incidents: Vec<PublicIncidentDto>,
    pub updated_at: String,
}

async fn build(state: &ApiState, page: &StatusPage) -> ApiResult<PublicStatus> {
    let mut tenant = state.store.tenant(page.org).await?;
    let mut apps = Vec::new();
    for env in &page.environments {
        apps.extend(tenant.apps(*env).await?);
    }
    apps.sort_by(|a, b| a.name.cmp(&b.name));
    let now = now_ms();
    let targets: Vec<uuid::Uuid> = apps.iter().map(|a| *a.target.as_uuid()).collect();
    let incidents = tenant.public_incidents(&targets, now - WEEK_MS).await?;
    drop(tenant);
    let names: HashMap<uuid::Uuid, &str> = apps
        .iter()
        .map(|a| (*a.target.as_uuid(), a.name.as_str()))
        .collect();
    let components: Vec<PublicComponent> = apps
        .iter()
        .map(|a| {
            let pods = state.projections.pods_of_app(&a.namespace, &a.slug);
            let ready = match a.delivery {
                Delivery::Agent => a.runtime.as_ref().map(kuben_store::repo::RuntimeStatus::ready),
                Delivery::Controller => state.projections.app(&a.namespace, &a.slug).map(|v| v.ready),
            };
            let open = |severity: &str| {
                incidents.iter().any(|i| {
                    i.resolved_at.is_none()
                        && i.severity == severity
                        && i.target_id == Some(*a.target.as_uuid())
                })
            };
            let observed = Observed {
                ready,
                pods: u32::try_from(pods.len()).unwrap_or(u32::MAX),
                ready_pods: u32::try_from(pods.iter().filter(|p| p.ready).count()).unwrap_or(u32::MAX),
                critical_incident: open("critical"),
                warning_incident: open("warning"),
            };
            PublicComponent {
                name: a.name.clone(),
                status: component_status(observed),
            }
        })
        .collect();
    let statuses: Vec<ServiceStatus> = components.iter().map(|c| c.status).collect();
    Ok(PublicStatus {
        title: page.title.clone(),
        status: page_status(&statuses),
        incidents: incidents
            .iter()
            .filter_map(|i| {
                let component = names.get(&i.target_id?)?;
                Some(PublicIncidentDto {
                    component: (*component).to_owned(),
                    severity: i.severity.clone(),
                    started_at: request::timestamp(i.opened_at),
                    resolved_at: i.resolved_at.map(request::timestamp),
                })
            })
            .collect(),
        components,
        updated_at: request::timestamp(now),
    })
}

/// A project's public status page. No sign-in; nothing internal.
#[utoipa::path(
    get, path = "/public/status/{slug}", operation_id = "getPublicStatus", tag = "status",
    params(("slug" = String, Path, description = "The page's public name")),
    responses((status = 200, body = PublicStatus), (status = 404, body = crate::error::Problem))
)]
pub async fn public(State(state): State<ApiState>, Path(slug): Path<String>) -> ApiResult<Response> {
    let cached = if let Some(found) = state.status_cache.get(&slug).await {
        found
    } else {
        let page = state
            .store
            .public_status_page(&slug)
            .await?
            .ok_or_else(|| Error::NotFound(format!("status page `{slug}`")))?;
        let built = Arc::new(build(&state, &page).await?);
        state.status_cache.insert(slug, built.clone()).await;
        built
    };
    let cache = format!("public, max-age={}", CACHE_TTL.as_secs());
    Ok(([(header::CACHE_CONTROL, cache)], Json((*cached).clone())).into_response())
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct StatusPageDto {
    pub slug: String,
    pub title: String,
    pub enabled: bool,
    pub environments: Vec<String>,
    /// Where the page is served: `/status/<slug>` on the console's address.
    pub path: String,
    pub updated_by: String,
    pub updated_at: String,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PutStatusPage {
    #[schema(example = "shop")]
    pub slug: String,
    #[schema(example = "Shop")]
    pub title: String,
    #[serde(default = "enabled")]
    pub enabled: bool,
    /// The environments whose apps the page lists.
    pub environments: Vec<String>,
}

const fn enabled() -> bool {
    true
}

fn dto(page: &StatusPage, names: &[String]) -> StatusPageDto {
    StatusPageDto {
        slug: page.slug.clone(),
        title: page.title.clone(),
        enabled: page.enabled,
        environments: names.to_vec(),
        path: format!("/status/{}", page.slug),
        updated_by: page.updated_by.clone(),
        updated_at: request::timestamp(page.updated_at),
    }
}

/// A project's status page settings.
#[utoipa::path(
    get, path = "/projects/{project}/status-page", operation_id = "getStatusPage", tag = "status",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 200, body = StatusPageDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<Json<StatusPageDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectRead, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let page = tenant
        .status_page(p.id())
        .await?
        .ok_or_else(|| Error::NotFound(format!("the status page of `{project}`")))?;
    let environments = tenant.environments(p.id()).await?;
    let names: Vec<String> = page
        .environments
        .iter()
        .filter_map(|id| environments.iter().find(|e| e.id == *id).map(|e| e.slug.clone()))
        .collect();
    Ok(Json(dto(&page, &names)))
}

/// Publish or change a project's status page.
#[utoipa::path(
    put, path = "/projects/{project}/status-page", operation_id = "putStatusPage", tag = "status",
    params(("project" = String, Path, description = "Project name")),
    request_body = PutStatusPage,
    responses(
        (status = 200, body = StatusPageDto),
        (status = 409, body = crate::error::Problem, description = "The slug is taken"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn put(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
    Json(body): Json<PutStatusPage>,
) -> ApiResult<Json<StatusPageDto>> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    validate::dns_label("slug", &body.slug, 63)?;
    let title = body.title.trim();
    if title.is_empty() || title.chars().count() > 100 || title.chars().any(char::is_control) {
        return Err(Error::Validation("title must be 1 to 100 printable characters".into()).into());
    }
    if body.environments.is_empty() || body.environments.len() > 20 {
        return Err(Error::Validation("list 1 to 20 environments".into()).into());
    }
    let mut tenant = state.store.tenant(p.org).await?;
    let known = tenant.environments(p.id()).await?;
    let mut ids = Vec::new();
    for name in &body.environments {
        let env = known
            .iter()
            .find(|e| &e.slug == name && !e.deleting)
            .ok_or_else(|| Error::Validation(format!("no environment `{name}`")))?;
        ids.push(env.id);
    }
    let (_, actor) = request::actor(&authz);
    let page = StatusPage {
        project: p.id(),
        org: p.org,
        slug: body.slug.clone(),
        title: title.to_owned(),
        enabled: body.enabled,
        environments: ids,
        updated_by: actor,
        updated_at: now_ms(),
    };
    tenant
        .set_status_page(&page)
        .await
        .map_err(|e| request::duplicate(e, &format!("status page `{}`", body.slug)))?;
    tenant
        .append_audit(request::audit(&authz, "status-page.updated", "project", project))
        .await?;
    tenant.commit().await?;
    state.status_cache.invalidate(&body.slug).await;
    Ok(Json(dto(&page, &body.environments)))
}

/// Take a project's status page down.
#[utoipa::path(
    delete, path = "/projects/{project}/status-page", operation_id = "deleteStatusPage", tag = "status",
    params(("project" = String, Path, description = "Project name")),
    responses((status = 204, description = "Removed"), (status = 404, body = crate::error::Problem))
)]
pub async fn delete(
    State(state): State<ApiState>,
    authz: Authz,
    Path(project): Path<String>,
) -> ApiResult<StatusCode> {
    let p = scope::project(&state, &authz, &project).await?;
    let _proof = authz.require(&state, Perm::ProjectWrite, &p.chain())?;
    let mut tenant = state.store.tenant(p.org).await?;
    let slug = tenant.status_page(p.id()).await?.map(|page| page.slug);
    if !tenant.delete_status_page(p.id()).await? {
        return Err(Error::NotFound(format!("the status page of `{project}`")).into());
    }
    tenant
        .append_audit(request::audit(&authz, "status-page.deleted", "project", project))
        .await?;
    tenant.commit().await?;
    if let Some(slug) = slug {
        state.status_cache.invalidate(&slug).await;
    }
    Ok(StatusCode::NO_CONTENT)
}

//! Export and detach (M4.11; plan §17 exit path, S08).
//!
//! An export is one JSON document:
//! - the portable release and configuration;
//! - the Kubernetes objects of the app's newest delivered run, as frozen by
//!   the renderer (with its version), as a `v1/List` that `kubectl apply`
//!   takes;
//! - the references the objects depend on (Secrets, pull Secrets, volumes,
//!   hostnames, Gateway and issuer);
//! - the inventory;
//! - a runbook.
//!
//! Secret values are never in it.
//!
//! A detach is a deletion that leaves the app running: its export is frozen
//! with the request, the materializer orphans the objects, and the app
//! leaves Kuben. The record stays until an operator releases it; until then
//! the environment's namespace outlives the environment.

use axum::{
    Json,
    extract::{Path, State},
    http::StatusCode,
};
use kuben_core::{Error, ids::TargetId, perm::Perm};
use kuben_store::repo::{DetachedApp, ExportMaterial, NewDetach, Subject, TARGET_DELETE};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    authz::Authz,
    error::ApiResult,
    routes::{request, scope},
    state::ApiState,
};

/// The export document's format.
pub const FORMAT: &str = "kuben.dev/export/v1";

/// Where an exported app lives.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ExportSubject<'a> {
    pub project: &'a str,
    pub environment: &'a str,
    pub app: &'a str,
    pub namespace: &'a str,
    pub delivery: &'a str,
    pub exported_at: String,
}

fn items(resources: &Value, namespace: &str) -> Vec<Value> {
    let mut items = resources.as_array().cloned().unwrap_or_default();
    for item in &mut items {
        if let Some(meta) = item.get_mut("metadata").and_then(Value::as_object_mut) {
            meta.entry("namespace")
                .or_insert_with(|| Value::String(namespace.to_owned()));
        }
    }
    items
}

fn names_of<'a>(items: &'a [Value], kind: &'a str) -> impl Iterator<Item = &'a str> + 'a {
    items
        .iter()
        .filter(move |i| i["kind"] == kind)
        .filter_map(|i| i["metadata"]["name"].as_str())
}

fn runbook(
    s: &ExportSubject<'_>,
    gateway: &str,
    hosts: &[&str],
    volumes: &[&str],
    issuer: Option<&str>,
) -> Vec<String> {
    let mut steps = vec![
        format!(
            "Secret values are not in this export. The Secrets listed under references.secrets stay in \
             namespace {}; once detached they lose Kuben's managed-by label and are never garbage-collected. \
             Rotate them yourself from now on, registry pull Secrets included.",
            s.namespace
        ),
        "To run the app elsewhere, copy those Secrets first, then apply `manifests` \
         (for example `jq .manifests export.json | kubectl apply -f -`)."
            .to_owned(),
        "The app's database and any other external service are not managed by Kuben: their lifecycle, \
         credentials and backups stay as they are."
            .to_owned(),
    ];
    if !hosts.is_empty() {
        steps.push(format!(
            "Routing: the HTTPRoute serves {} through Gateway {gateway}. While Kuben runs, its Gateway keeps \
             serving these hostnames. Before you uninstall Kuben, attach the route to a Gateway you own{}.",
            hosts.join(", "),
            issuer.map_or_else(String::new, |i| format!(
                " whose HTTPS listeners carry `cert-manager.io/cluster-issuer: {i}`, so certificates keep renewing"
            )),
        ));
    }
    if !volumes.is_empty() {
        steps.push(format!(
            "Volumes {} are kept: Kuben never deletes them after a detach. Back them up before you move the app.",
            volumes.join(", ")
        ));
    }
    steps.push(format!(
        "A new app named `{}` in this environment would take these objects over: pick another name, or \
         remove the objects first.",
        s.app
    ));
    steps.push(
        "Release the detached app once you own it for good: afterwards, deleting the environment deletes \
         its namespace with everything in it."
            .to_owned(),
    );
    steps
}

/// The export document of `s` from its newest delivered run `m`.
#[must_use]
pub fn document(s: &ExportSubject<'_>, m: &ExportMaterial) -> Value {
    let items = items(&m.resources, s.namespace);
    let gateway = m.capabilities["gateway"].as_str().unwrap_or("(none)");
    let issuer = m.capabilities["clusterIssuer"].as_str();
    let hosts: Vec<&str> = items
        .iter()
        .filter(|i| i["kind"] == "HTTPRoute")
        .flat_map(|i| i["spec"]["hostnames"].as_array().into_iter().flatten())
        .filter_map(Value::as_str)
        .collect();
    let volumes: Vec<&str> = names_of(&items, "PersistentVolumeClaim").collect();
    let secrets: Vec<Value> = m
        .secrets
        .iter()
        .map(|r| {
            json!({ "name": r.name, "revision": r.revision, "object": r.object(),
                    "kind": r.kind, "registry": r.registry })
        })
        .collect();
    let mut inventory: Vec<Value> = items
        .iter()
        .map(|i| {
            json!({ "apiVersion": i["apiVersion"], "kind": i["kind"],
                    "name": i["metadata"]["name"], "namespace": s.namespace })
        })
        .collect();
    inventory.extend(m.secrets.iter().map(
        |r| json!({ "apiVersion": "v1", "kind": "Secret", "name": r.object(), "namespace": s.namespace }),
    ));
    json!({
        "format": FORMAT,
        "kuben": env!("CARGO_PKG_VERSION"),
        "exportedAt": s.exported_at,
        "app": {
            "project": s.project, "environment": s.environment, "app": s.app,
            "namespace": s.namespace, "delivery": s.delivery,
        },
        "run": { "id": m.run, "generation": m.generation, "phase": m.phase, "reason": m.reason },
        "release": {
            "id": m.release, "artifacts": m.artifacts, "processContract": m.process_contract,
            "portableConfig": m.portable_config, "source": m.source,
        },
        "config": { "revision": m.config_revision, "spec": m.config },
        "renderer": { "version": m.renderer_version, "capabilities": m.capabilities },
        "manifests": { "apiVersion": "v1", "kind": "List", "items": items },
        "references": {
            "secrets": secrets,
            "volumes": volumes,
            "hostnames": hosts,
            "gateway": m.capabilities["gateway"],
            "clusterIssuer": issuer,
        },
        "inventory": inventory,
        "runbook": runbook(s, gateway, &hosts, &volumes, issuer),
    })
}

async fn export_of(
    state: &ApiState,
    a: &scope::AppScope,
    project: &str,
    environment: &str,
) -> ApiResult<Value> {
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let material = tenant
        .export_material(a.app.target)
        .await?
        .ok_or_else(|| Error::Conflict(format!("app `{}` has not been delivered yet", a.slug())))?;
    let subject = ExportSubject {
        project,
        environment,
        app: a.slug(),
        namespace: a.namespace(),
        delivery: a.app.delivery.as_str(),
        exported_at: request::timestamp(kuben_core::time::now_ms()),
    };
    Ok(document(&subject, &material))
}

/// Export an app: portable release and configuration, standard manifests,
/// references, inventory and runbook. No secret values.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/export", operation_id = "exportApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses(
        (status = 200, body = Object, description = "The export document (`kuben.dev/export/v1`)"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Not delivered yet"),
    )
)]
pub async fn export(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<Value>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    Ok(Json(export_of(&state, &a, &project, &environment).await?))
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct DetachRequest {
    /// The app's name again.
    pub confirm: String,
    /// Why; kept with the record and audited.
    pub reason: String,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct DetachedAppDto {
    pub id: Uuid,
    pub app: String,
    pub namespace: String,
    pub reason: String,
    pub requested_by: String,
    pub requested_at: String,
    /// Set once the objects are orphaned.
    pub completed_at: Option<String>,
    pub released_at: Option<String>,
    pub released_by: Option<String>,
    /// The export frozen with the request (only when one app is read).
    #[serde(skip_serializing_if = "Option::is_none")]
    #[schema(value_type = Option<Object>)]
    pub export: Option<Value>,
}

impl From<DetachedApp> for DetachedAppDto {
    fn from(d: DetachedApp) -> Self {
        Self {
            id: d.target_id,
            app: d.app,
            namespace: d.namespace,
            reason: d.reason,
            requested_by: d.requested_by,
            requested_at: request::timestamp(d.requested_at),
            completed_at: d.completed_at.map(request::timestamp),
            released_at: d.released_at.map(request::timestamp),
            released_by: d.released_by,
            export: None,
        }
    }
}

/// Detach an app: Kuben lets go of it and leaves its objects running.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/detach", operation_id = "detachApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = DetachRequest,
    responses(
        (status = 202, body = DetachedAppDto, description = "Detach scheduled; the answer holds the export"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Being deleted, a run in flight, or never delivered"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn detach(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<DetachRequest>,
) -> ApiResult<(StatusCode, Json<DetachedAppDto>)> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &a.chain())?;
    let reason = body.reason.trim();
    if body.confirm != app {
        return Err(Error::Validation("confirm must repeat the app's name".into()).into());
    }
    if reason.is_empty() || reason.chars().count() > 1024 {
        return Err(Error::Validation("reason must be 1 to 1024 characters".into()).into());
    }
    let export = export_of(&state, &a, &project, &environment).await?;
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    if tenant.run_in_flight(a.app.target).await? {
        return Err(Error::Conflict(format!(
            "a run of app `{app}` has not finished: wait for it or cancel it first"
        ))
        .into());
    }
    if !tenant.mark_target_deleting(a.app.target).await? {
        return Err(Error::Conflict(format!("app `{app}` is being deleted")).into());
    }
    let (_, actor) = request::actor(&authz);
    tenant
        .record_detach(&NewDetach {
            project: a.env.project.id(),
            environment: a.env.id(),
            target: a.app.target,
            app: a.slug(),
            namespace: a.namespace(),
            reason,
            requested_by: &actor,
            export: &export,
        })
        .await?;
    let mut audit = request::audit(
        &authz,
        "app.detached",
        "app",
        format!("{project}/{environment}/{app}"),
    );
    audit.data = Some(json!({ "reason": reason }));
    let subject = Subject::detach(a.env.project.id(), a.env.id(), a.app.target);
    tenant.request(TARGET_DELETE, subject, &actor, audit).await?;
    let record = tenant
        .detached_app(a.app.target)
        .await?
        .ok_or_else(|| Error::Internal("the detach record is missing".into()))?;
    tenant.commit().await?;
    Ok((
        StatusCode::ACCEPTED,
        Json(DetachedAppDto {
            export: Some(export),
            ..DetachedAppDto::from(record)
        }),
    ))
}

/// The apps detached from an environment.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/detached", operation_id = "listDetachedApps",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
    ),
    responses((status = 200, body = Vec<DetachedAppDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn list_detached(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment)): Path<(String, String)>,
) -> ApiResult<Json<Vec<DetachedAppDto>>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppRead, &e.chain())?;
    let mut tenant = state.store.tenant(e.project.org).await?;
    let found = tenant.detached_apps(e.id()).await?;
    Ok(Json(found.into_iter().map(DetachedAppDto::from).collect()))
}

/// One detached app, with its export.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/detached/{id}", operation_id = "getDetachedApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("id" = Uuid, Path, description = "The detached app's id"),
    ),
    responses((status = 200, body = DetachedAppDto), (status = 404, body = crate::error::Problem))
)]
pub async fn get_detached(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, id)): Path<(String, String, Uuid)>,
) -> ApiResult<Json<DetachedAppDto>> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::AppRead, &e.chain())?;
    let target = TargetId::from_uuid(id);
    let mut tenant = state.store.tenant(e.project.org).await?;
    let record = tenant
        .detached_app(target)
        .await?
        .filter(|d| d.environment_id == *e.id().as_uuid())
        .ok_or_else(|| Error::NotFound(format!("detached app `{id}`")))?;
    let export = tenant.detached_export(target).await?;
    Ok(Json(DetachedAppDto {
        export,
        ..DetachedAppDto::from(record)
    }))
}

/// Release a detached app: someone owns it now, and deleting the
/// environment no longer keeps its namespace.
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/detached/{id}/release", operation_id = "releaseDetachedApp",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("id" = Uuid, Path, description = "The detached app's id"),
    ),
    responses(
        (status = 204, description = "Released"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Not complete yet, or released already"),
    )
)]
pub async fn release_detached(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, id)): Path<(String, String, Uuid)>,
) -> ApiResult<StatusCode> {
    let e = scope::environment(&state, &authz, &project, &environment).await?;
    let _proof = authz.require(&state, Perm::EnvWrite, &e.chain())?;
    let target = TargetId::from_uuid(id);
    let mut tenant = state.store.tenant(e.project.org).await?;
    let record = tenant
        .detached_app(target)
        .await?
        .filter(|d| d.environment_id == *e.id().as_uuid())
        .ok_or_else(|| Error::NotFound(format!("detached app `{id}`")))?;
    if record.completed_at.is_none() {
        return Err(Error::Conflict(format!("the detach of `{}` has not finished", record.app)).into());
    }
    let (_, actor) = request::actor(&authz);
    if !tenant.release_detached(target, &actor).await? {
        return Err(Error::Conflict(format!("`{}` was released already", record.app)).into());
    }
    tenant
        .append_audit(request::audit(
            &authz,
            "app.detached.released",
            "app",
            format!("{project}/{environment}/{}", record.app),
        ))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

#[cfg(test)]
mod tests {
    use kuben_core::ids::DeploymentRunId;
    use kuben_store::repo::SecretReference;

    use super::*;

    fn material() -> ExportMaterial {
        ExportMaterial {
            run: DeploymentRunId::from_uuid(Uuid::nil()),
            generation: 3,
            phase: "succeeded".into(),
            reason: "deploy".into(),
            release: Uuid::nil(),
            artifacts: json!({ "web": "ghcr.io/acme/web@sha256:aa" }),
            process_contract: json!({}),
            portable_config: json!({ "env": { "MODE": "prod" } }),
            source: None,
            config_revision: 2,
            config: json!({ "replicas": 2 }),
            renderer_version: "kuben-renderer/2".into(),
            capabilities: json!({ "gateway": "kuben-system/kuben", "clusterIssuer": "letsencrypt" }),
            resources: json!([
                { "apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": { "name": "web-data" } },
                { "apiVersion": "apps/v1", "kind": "Deployment", "metadata": { "name": "web-web" } },
                { "apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
                  "metadata": { "name": "web", "namespace": "other" },
                  "spec": { "hostnames": ["web.example.com"] } },
            ]),
            secrets: vec![SecretReference {
                name: "db".into(),
                revision: 4,
                kind: "opaque".into(),
                registry: None,
            }],
        }
    }

    fn subject() -> ExportSubject<'static> {
        ExportSubject {
            project: "shop",
            environment: "prod",
            app: "web",
            namespace: "shop-prod",
            delivery: "controller",
            exported_at: "2026-09-17T00:00:00Z".into(),
        }
    }

    #[test]
    fn an_export_holds_manifests_references_and_a_runbook() {
        let doc = document(&subject(), &material());
        assert_eq!(doc["format"], FORMAT);
        assert_eq!(doc["manifests"]["kind"], "List");
        let items = doc["manifests"]["items"].as_array().expect("items");
        assert_eq!(items[0]["metadata"]["namespace"], "shop-prod", "namespaced");
        assert_eq!(items[2]["metadata"]["namespace"], "other", "kept as rendered");
        assert_eq!(doc["references"]["volumes"], json!(["web-data"]));
        assert_eq!(doc["references"]["hostnames"], json!(["web.example.com"]));
        assert_eq!(doc["references"]["secrets"][0]["object"], "db.r4");
        let inventory = doc["inventory"].as_array().expect("inventory");
        assert_eq!(inventory.len(), 4);
        assert!(
            inventory
                .iter()
                .any(|i| i["kind"] == "Secret" && i["name"] == "db.r4")
        );
        let runbook = doc["runbook"].to_string();
        assert!(runbook.contains("web.example.com"));
        assert!(runbook.contains("cert-manager.io/cluster-issuer: letsencrypt"));
        assert!(runbook.contains("web-data"));
        assert_eq!(doc["renderer"]["version"], "kuben-renderer/2");
    }

    #[test]
    fn an_app_without_routes_or_volumes_gets_a_shorter_runbook() {
        let mut m = material();
        m.resources = json!([{ "apiVersion": "apps/v1", "kind": "Deployment", "metadata": { "name": "w" } }]);
        m.secrets.clear();
        let doc = document(&subject(), &m);
        let steps = doc["runbook"].as_array().expect("steps");
        assert_eq!(steps.len(), 5);
        assert!(!doc["runbook"].to_string().contains("Gateway kuben-system"));
    }
}

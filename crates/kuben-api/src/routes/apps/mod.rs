//! Apps of an environment, one module per concern:
//!
//! * [`crud`] — CRUD on the `App` CRD, rolling restart and logs;
//! * [`releases`] — release history and rollback (scenario 5);
//! * [`jobs`] — scheduled runs, "run now" (scenario 7);
//! * [`domains`] — DNS checks of an app's hostnames (scenario 9);
//! * [`promote`] — promotion between environments (scenario 10);
//! * [`spec`] — request bodies, validation and spec construction. Cross-field
//!   rules are the controller's own (`resources::validate`), so the API
//!   rejects exactly what the controller could never build.
//!
//! This module holds the DTOs and helpers they share. Volumes (scenario 6)
//! are part of the app spec.

pub mod crud;
pub mod domains;
pub mod jobs;
pub mod promote;
pub mod releases;
pub mod spec;

use std::collections::BTreeMap;

use kube::{
    Api,
    api::{ObjectMeta, PostParams},
};
use kuben_core::{Error, ids::OrgId};
use kuben_crd::{App, AppSpec, Protocol, labels};
use kuben_platform::projection::{AppView, PodPhase, PodView};
use kuben_store::repo::NewRelease;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use spec::ensure_domains_free;
pub(crate) use spec::validate_spec;

use super::scope::{self, AppScope, EnvScope};
use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Process created for single-process web apps.
const WEB: &str = "web";
/// Process created for scheduled apps.
const JOB: &str = "job";

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum ProtocolDto {
    /// Routed through the gateway on the app's hostnames.
    #[default]
    Http,
    /// Cluster-internal TCP (databases, caches); never public.
    Tcp,
}

impl From<ProtocolDto> for Protocol {
    fn from(p: ProtocolDto) -> Self {
        match p {
            ProtocolDto::Http => Self::Http,
            ProtocolDto::Tcp => Self::Tcp,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct ProcessDto {
    pub name: String,
    pub command: Vec<String>,
    pub port: Option<u16>,
    pub size: String,
    pub min_replicas: u32,
    pub max_replicas: u32,
    /// Cron expression of scheduled processes.
    pub schedule: Option<String>,
    /// `http` or `tcp`.
    pub protocol: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
pub struct SecretRef {
    pub name: String,
    pub key: String,
}

/// Environment variable: a plain `value` or a `secret` reference.
#[derive(Clone, Debug, Serialize, Deserialize, ToSchema)]
pub struct EnvVarDto {
    #[schema(example = "LOG_LEVEL")]
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub value: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub secret: Option<SecretRef>,
}

fn default_volume_size() -> String {
    "1Gi".into()
}

/// Persistent volume (kept when the app is deleted, unless requested).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
pub struct VolumeDto {
    #[schema(example = "data")]
    pub name: String,
    #[schema(example = "/data")]
    pub mount_path: String,
    /// Capacity such as `5Gi`. Can grow, never shrink.
    #[serde(default = "default_volume_size")]
    #[schema(example = "5Gi")]
    pub size: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct AppDto {
    pub name: String,
    pub project: String,
    /// Environment short name.
    pub environment: String,
    pub namespace: String,
    pub image: Option<String>,
    pub git_repo: Option<String>,
    pub url: Option<String>,
    pub ready: bool,
    /// `Available`, `Progressing`, `RolloutFailed`, `Scheduled`, `AwaitingBuild`, ...
    pub reason: Option<String>,
    pub message: Option<String>,
    pub processes: Vec<ProcessDto>,
    /// Values are only included in the app detail, for callers allowed to read secrets.
    pub env: Vec<EnvVarDto>,
    pub domains: Vec<String>,
    pub volumes: Vec<VolumeDto>,
    pub created_at: Option<String>,
}

impl AppDto {
    pub(crate) fn from_view(v: &AppView, project: &str, environment: &str) -> Self {
        Self {
            name: v.name.clone(),
            project: project.to_owned(),
            environment: environment.to_owned(),
            namespace: v.namespace.clone(),
            image: v.image.clone(),
            git_repo: v.git_repo.clone(),
            url: v.url.clone(),
            ready: v.ready,
            reason: v.reason.clone(),
            message: v.message.clone(),
            processes: v
                .processes
                .iter()
                .map(|p| ProcessDto {
                    name: p.name.clone(),
                    command: p.command.clone(),
                    port: p.port,
                    size: p.size.clone(),
                    min_replicas: p.min_replicas,
                    max_replicas: p.max_replicas,
                    schedule: p.schedule.clone(),
                    protocol: p.protocol.clone(),
                })
                .collect(),
            env: v
                .env
                .iter()
                .map(|e| EnvVarDto {
                    name: e.name.clone(),
                    value: None,
                    secret: e
                        .secret
                        .as_deref()
                        .and_then(|s| s.split_once('/'))
                        .map(|(n, k)| SecretRef {
                            name: n.to_owned(),
                            key: k.to_owned(),
                        }),
                })
                .collect(),
            domains: v.domains.clone(),
            volumes: v
                .volumes
                .iter()
                .map(|vol| VolumeDto {
                    name: vol.name.clone(),
                    mount_path: vol.mount_path.clone(),
                    size: vol.size.clone(),
                })
                .collect(),
            created_at: v.created_at.clone(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct PodDto {
    pub name: String,
    pub process: Option<String>,
    /// `pending`, `running`, `succeeded`, `failed` or `unknown`.
    pub phase: String,
    pub ready: bool,
    pub restarts: i32,
    pub reason: Option<String>,
    pub node: Option<String>,
    pub started_at: Option<String>,
}

impl From<&PodView> for PodDto {
    fn from(p: &PodView) -> Self {
        let phase = match p.phase {
            PodPhase::Pending => "pending",
            PodPhase::Running => "running",
            PodPhase::Succeeded => "succeeded",
            PodPhase::Failed => "failed",
            PodPhase::Unknown => "unknown",
        };
        Self {
            name: p.name.clone(),
            process: p.process.clone(),
            phase: phase.into(),
            ready: p.ready,
            restarts: p.restarts,
            reason: p.reason.clone(),
            node: p.node.clone(),
            started_at: p.started_at.clone(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct AppDetail {
    pub app: AppDto,
    pub pods: Vec<PodDto>,
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

fn spec_json(spec: &AppSpec) -> serde_json::Value {
    serde_json::to_value(spec).unwrap_or_default()
}

/// Record a revision. A failure is logged, never returned: the Kubernetes
/// write already happened and must not be reported as failed.
#[allow(clippy::too_many_arguments)]
async fn record_release(
    state: &ApiState,
    authz: &Authz,
    org: OrgId,
    namespace: &str,
    app: &str,
    spec: &AppSpec,
    reason: &str,
    note: Option<String>,
) {
    let release = NewRelease {
        org_id: Some(org),
        namespace: namespace.to_owned(),
        app: app.to_owned(),
        image: spec.source.image.clone(),
        spec: spec_json(spec),
        reason: reason.to_owned(),
        actor_id: Some(authz.current.user.id.to_string()),
        note,
    };
    if let Err(e) = state.store.record_release(release).await {
        tracing::error!(error = %e, %namespace, %app, "failed to record release");
    }
}

fn app_dto(a: &AppScope, view: &AppView) -> AppDto {
    AppDto::from_view(view, &a.env.project.view.name, a.env.short_name())
}

fn app_api(state: &ApiState, env: &EnvScope) -> ApiResult<Api<App>> {
    Ok(Api::namespaced(scope::cluster(state)?, &env.view.namespace))
}

fn new_app_object(e: &EnvScope, name: &str, spec: AppSpec) -> App {
    App {
        metadata: ObjectMeta {
            name: Some(name.to_owned()),
            namespace: Some(e.view.namespace.clone()),
            labels: Some(BTreeMap::from([
                (labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned()),
                (labels::ORG.to_owned(), e.project.org.to_string()),
                (labels::PROJECT.to_owned(), e.project.view.name.clone()),
                (labels::ENVIRONMENT.to_owned(), e.view.name.clone()),
            ])),
            ..ObjectMeta::default()
        },
        spec,
        status: None,
    }
}

/// Create an App (shared by `createApp` and `deployTemplate`).
pub(crate) async fn create_app(
    state: &ApiState,
    authz: &Authz,
    e: &EnvScope,
    name: &str,
    spec: AppSpec,
    reason: &str,
    note: Option<String>,
) -> ApiResult<AppDto> {
    validate_spec(&spec)?;
    if e.view.deleting {
        return Err(Error::Conflict(format!("environment `{}` is being deleted", e.short_name())).into());
    }
    ensure_domains_free(state, &e.view.namespace, name, &spec)?;
    let created = app_api(state, e)?
        .create(&PostParams::default(), &new_app_object(e, name, spec))
        .await
        .map_err(|err| scope::kube_error(err, name))?;
    record_release(
        state,
        authz,
        e.project.org,
        &e.view.namespace,
        name,
        &created.spec,
        reason,
        note,
    )
    .await;
    Ok(AppDto::from_view(
        &AppView::from(&created),
        &e.project.view.name,
        e.short_name(),
    ))
}

/// A one-process web app, the starting point of most unit tests here.
#[cfg(test)]
fn sample_spec() -> AppSpec {
    serde_json::from_value(serde_json::json!({
        "source": { "image": "nginx:1.27" },
        "runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 1, "max": 1 } } } }
    }))
    .expect("spec")
}

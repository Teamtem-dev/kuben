//! Apps of an environment on the SQL model (ADR-032), one module per concern:
//!
//! * [`crud`] — create, read, update and delete, rolling restart;
//! * [`logs`] — log lines, once or followed live, and Kubernetes events;
//! * [`releases`] — release history and rollback (scenario 5);
//! * [`jobs`] — scheduled runs, "run now" (scenario 7);
//! * [`domains`] — DNS checks of an app's hostnames (scenario 9);
//! * [`doctor`] — why an app is or is not reachable, check by check;
//! * [`promote`] — promotion between environments (scenario 10);
//! * [`deployments`] — deploy acceptance by digest or release;
//! * [`source`] — the Git repository and branch an app builds from (M3);
//! * [`builds`] — the app's builds, their outcome, release and run (M3);
//! * [`spec`] — request bodies, validation and spec construction. Cross-field
//!   rules are the controller's own (`resources::validate`), so the API
//!   rejects exactly what the controller could never build.
//!
//! An app is a target: one application on its environment's placement. Its
//! desired state is its newest configuration revision (the App spec without
//! its image) and the release of its newest deployment run. Every change is
//! a new run, which the materializer writes and the controllers carry out;
//! the projection adds live status (for an app its cluster's agent delivers,
//! the agent's last report, from SQL). An image given as a tag is resolved to a
//! digest at its registry first (option A).

pub mod admission;
pub mod approvals;
pub mod builds;
pub mod crud;
pub mod deployments;
pub mod doctor;
pub mod domains;
pub mod jobs;
pub mod logs;
pub mod promote;
pub mod releases;
pub mod scans;
pub mod source;
pub mod spec;

use std::collections::BTreeMap;

use kuben_core::{
    Error,
    ids::{ApplicationId, ProjectId, ReleaseId, TargetId},
    ops::Generation,
};
use kuben_crd::{App, AppSpec, Protocol, Runtime, Source};
use kuben_platform::projection::{AppView, ExposureView, PodPhase, PodView, Projections};
use kuben_store::repo::{
    AppRecord, Delivery, PortableRelease, RunReason, RuntimeStatus, StartDeployment, Started, Tenant,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest as _, Sha256};
use utoipa::ToSchema;

use spec::ensure_domains_free;
pub(crate) use spec::validate_spec;

use super::{request, scope::EnvScope};
use crate::{
    authz::Authz,
    error::{ApiError, ApiResult},
    oci::{ResolveError, Resolved},
    state::ApiState,
};

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

#[derive(Debug, Serialize, Deserialize, ToSchema)]
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

#[derive(Debug, Serialize, Deserialize, ToSchema)]
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
    /// Whether the app can be reached through the gateway, apart from
    /// whether it runs (null while it has no route).
    pub exposure: Option<ExposureDto>,
}

/// How an app is reached through the gateway.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct ExposureDto {
    /// The gateway accepted the route and resolved its references; null
    /// until a gateway controller answered.
    pub routed: Option<bool>,
    pub message: Option<String>,
    pub hosts: Vec<HostDto>,
}

/// One hostname of an app.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct HostDto {
    pub host: String,
    /// `auto` (certificate from the cluster issuer), `secret` (the app's own
    /// certificate) or `none` (plain HTTP).
    pub tls: String,
    /// For `auto` hosts with a certificate of their own: whether it is issued.
    pub certificate_ready: Option<bool>,
    pub certificate_message: Option<String>,
}

impl From<ExposureView> for ExposureDto {
    fn from(v: ExposureView) -> Self {
        Self {
            routed: v.accepted,
            message: v.message,
            hosts: v
                .hosts
                .into_iter()
                .map(|h| HostDto {
                    host: h.host,
                    tls: h.tls.to_owned(),
                    certificate_ready: h.certificate_ready,
                    certificate_message: h.certificate_message,
                })
                .collect(),
        }
    }
}

impl AppDto {
    /// Add what the cluster says about reaching the app.
    #[must_use]
    pub fn with_exposure(mut self, projections: &Projections) -> Self {
        self.exposure = projections
            .exposure(&self.namespace, &self.name)
            .map(ExposureDto::from);
        self
    }

    fn from_view(v: &AppView, project: &str, environment: &str) -> Self {
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
            exposure: None,
        }
    }

    /// The app `record`: its desired state from SQL, its live status from
    /// `view` (none until the materializer wrote it), or for an app its
    /// cluster's agent delivers, from the agent's last report (M1.9).
    #[must_use]
    pub fn of(project: &str, environment: &str, record: &AppRecord, view: Option<&AppView>) -> Self {
        let mut desired = App::new(&record.slug, desired_spec(record).unwrap_or_else(empty_spec));
        desired.metadata.namespace = Some(record.namespace.clone());
        let mut dto = Self::from_view(&AppView::from(&desired), project, environment);
        dto.image.clone_from(&record.image);
        if record.delivery == Delivery::Agent {
            // No App object: the agent reports what it carried out.
            let runtime = record.runtime.as_ref();
            dto.url = runtime.and_then(|r| r.url.clone());
            dto.ready = runtime.is_some_and(RuntimeStatus::ready) && !record.deleting;
            dto.reason = runtime.and_then(runtime_reason);
            dto.message = runtime.and_then(|r| r.message.clone());
        } else {
            dto.url = view.and_then(|v| v.url.clone());
            dto.ready = view.is_some_and(|v| v.ready) && !record.deleting;
            dto.reason = view.and_then(|v| v.reason.clone());
            dto.message = view.and_then(|v| v.message.clone());
        }
        dto.created_at = Some(request::timestamp(record.created_at));
        dto
    }
}

/// Why an agent-delivered app is where it is: the agent's reason, else the
/// phase it reported while not ready.
fn runtime_reason(runtime: &RuntimeStatus) -> Option<String> {
    runtime.reason.clone().or_else(|| {
        (!runtime.ready()).then(|| {
            match runtime.phase.as_str() {
                "accepted" => "Accepted",
                "applying" => "Applying",
                "failed" => "Failed",
                "rejected" => "Rejected",
                _ => "Unknown",
            }
            .to_owned()
        })
    })
}

#[derive(Debug, Serialize, Deserialize, ToSchema)]
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

#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct AppDetail {
    pub app: AppDto,
    pub pods: Vec<PodDto>,
}

// ---------------------------------------------------------------------------
// Desired state
// ---------------------------------------------------------------------------

fn empty_spec() -> AppSpec {
    AppSpec {
        source: Source::default(),
        runtime: Runtime {
            processes: BTreeMap::new(),
            health_check: None,
            fs_group: None,
        },
        env: Vec::new(),
        domains: Vec::new(),
        volumes: Vec::new(),
        image_pull_secrets: Vec::new(),
    }
}

fn spec_json(spec: &AppSpec) -> Value {
    serde_json::to_value(spec).unwrap_or_default()
}

/// The app's desired spec: its newest configuration with the image of its
/// newest release, as it was given.
#[must_use]
pub(crate) fn desired_spec(record: &AppRecord) -> Option<AppSpec> {
    spec_of(record.config.clone(), record.image.as_deref())
}

/// An App spec from a configuration revision and the image it runs.
#[must_use]
pub(crate) fn spec_of(config: Option<Value>, image: Option<&str>) -> Option<AppSpec> {
    let mut config = config?;
    if let (Some(object), Some(image)) = (config.as_object_mut(), image) {
        object.insert("source".into(), json!({ "image": image }));
    }
    serde_json::from_value(config).ok()
}

/// The configuration revision of `spec`: the App spec without its image, the
/// contract the materializer renders from.
pub(crate) fn config_of(spec: &AppSpec) -> Result<Value, Error> {
    let mut config = serde_json::to_value(spec).map_err(Error::internal)?;
    if let Some(object) = config.as_object_mut() {
        object.remove("source");
    }
    Ok(config)
}

/// Resolve `image` to a digest at its registry (option A), pulling with
/// `e`'s login for that registry when it has one.
pub(crate) async fn resolve(state: &ApiState, e: &EnvScope, image: &str) -> ApiResult<Resolved> {
    let login = match crate::oci::parse(image) {
        // A digest needs no registry.
        Ok(reference) if matches!(reference.reference, crate::oci::Reference::Tag(_)) => {
            let mut tenant = state.store.tenant(e.project.org).await?;
            crate::routes::secrets::registry_login(state, &mut tenant, e, &reference.registry).await?
        }
        _ => None,
    };
    state
        .images
        .resolve_as(image, login.as_ref())
        .await
        .map_err(|e| match e {
            ResolveError::Unreachable { .. } => ApiError(Error::Unavailable(e.to_string())),
            _ => ApiError(Error::Validation(e.to_string())),
        })
}

/// What a deployment runs: a newly resolved image, or an existing release.
#[derive(Debug)]
pub(crate) enum Artifact {
    Resolved(Resolved),
    Release(ReleaseId),
}

/// One change of an app, to record and deploy.
#[derive(Debug)]
pub(crate) struct Change<'a> {
    pub project: ProjectId,
    pub application: ApplicationId,
    pub target: TargetId,
    pub spec: &'a AppSpec,
    pub artifact: Artifact,
    /// The target generation the caller saw.
    pub expected: Generation,
    pub reason: RunReason,
    /// `project/environment/app`, for the audit record.
    pub reference: String,
    /// Where the change lands, for the environment's deploy role.
    pub chain: kuben_core::authz::ScopeChain,
    /// The environment and its quota, for admission.
    pub environment: (kuben_core::ids::EnvironmentId, Option<&'a Value>),
}

/// Record `change` as the app's newest configuration and start a deployment
/// run of it, in `tenant`'s transaction.
pub(crate) async fn deploy(
    state: &ApiState,
    tenant: &mut Tenant,
    authz: &Authz,
    change: Change<'_>,
) -> ApiResult<()> {
    approvals::ensure_may_deploy(tenant, authz, change.target, &change.chain).await?;
    let (environment, quota) = change.environment;
    let placement = admission::Placement {
        environment,
        quota,
        target: change.target,
    };
    for warning in admission::admit(state, tenant, placement, change.spec).await? {
        tracing::info!(target = %change.reference, %warning, "admitted with a warning");
    }
    let (_, actor) = request::actor(authz);
    let config = config_of(change.spec)?;
    let missing = || Error::NotFound("the app".into());
    let (revision, _) = tenant
        .create_config_revision(change.project, change.target, &config, &actor)
        .await?
        .ok_or_else(missing)?;
    let release = match change.artifact {
        Artifact::Release(id) => id,
        Artifact::Resolved(image) => {
            let release = PortableRelease {
                application: change.application,
                artifacts: BTreeMap::from([(WEB.to_owned(), image.digest.clone())]),
                process_contract: json!({}),
                portable_config: json!({}),
                renderer_schema: 1,
                source: Some(json!({ "image_repository": image.repository, "image": image.given })),
                created_by: actor.clone(),
            };
            tenant.create_release(change.project, &release).await?.0
        }
    };
    if change.reason.carries_new_code() {
        for warning in admission::scan_gate(tenant, change.target, release).await? {
            tracing::info!(target = %change.reference, %warning, "deployed with a vulnerability warning");
        }
    }
    let lifecycle_uid = tenant
        .target_state(change.target)
        .await?
        .ok_or_else(missing)?
        .lifecycle_uid;
    let input_hash = Sha256::digest(format!("{release}/{revision}/{}", change.expected.0)).to_vec();
    let request = StartDeployment {
        project: change.project,
        target: change.target,
        release,
        config_revision: revision,
        render_plan: None,
        expected_generation: change.expected,
        lifecycle_uid,
        reason: change.reason,
        requested_by: actor,
        input_hash,
    };
    let audit = request::audit(authz, "deployment.accepted", "app", change.reference);
    started(tenant.start_deployment(&request, audit, None).await?)
}

/// The answer to a run the API started without an idempotency key.
pub(crate) fn started(started: Started) -> ApiResult<()> {
    match started {
        Started::Accepted { .. } | Started::Replayed(_) => Ok(()),
        Started::Rejected(reject) => Err(Error::Conflict(reject.to_string()).into()),
        Started::NotFound => Err(Error::NotFound("that release of this app".into()).into()),
        Started::SecretRevoked => Err(secret_revoked().into()),
        Started::VulnerabilityBlocked => Err(vulnerability_blocked().into()),
        Started::KeyReused(_) => {
            Err(Error::Internal("a deployment without a key was a replay".into()).into())
        }
    }
}

/// A run refused because a secret it references has a revoked current
/// revision.
pub(crate) fn secret_revoked() -> Error {
    Error::Conflict(
        "a secret this app references has its current revision revoked: set a new value first".into(),
    )
}

/// A run refused by the environment's vulnerability gate.
pub(crate) fn vulnerability_blocked() -> Error {
    Error::Conflict("the environment's vulnerability gate refuses this release".into())
}

/// Create the app `name` from `spec` in environment `e` (shared by
/// `createApp` and `deployTemplate`). An app of that name in another
/// environment of the project is the same application.
pub(crate) async fn create_app(
    state: &ApiState,
    authz: &Authz,
    e: &EnvScope,
    name: &str,
    spec: AppSpec,
) -> ApiResult<AppDto> {
    validate_spec(&spec)?;
    if e.deleting() {
        return Err(Error::Conflict(format!("environment `{}` is being deleted", e.short_name())).into());
    }
    ensure_domains_free(state, e.project.org, &e.namespace(), name, &spec).await?;
    let image = spec
        .source
        .image
        .as_deref()
        .ok_or_else(|| Error::Validation("an app needs an image".into()))?;
    let resolved = resolve(state, e, image).await?;
    let placement = e
        .env
        .placement
        .ok_or_else(|| Error::Conflict(format!("environment `{}` has no placement", e.short_name())))?;
    let project = e.project.id();
    let what = format!("app `{name}`");
    let mut tenant = state.store.tenant(e.project.org).await?;
    let application = match tenant.application(project, name).await? {
        Some(id) => id,
        None => tenant
            .create_application(project, name, name)
            .await
            .map_err(|er| request::duplicate(er, &what))?,
    };
    let target = tenant
        .create_target(project, application, placement)
        .await
        .map_err(|er| request::duplicate(er, &what))?;
    let change = Change {
        project,
        application,
        target,
        spec: &spec,
        artifact: Artifact::Resolved(resolved),
        expected: Generation(0),
        reason: RunReason::Deploy,
        reference: format!("{}/{}/{name}", e.project.slug(), e.short_name()),
        chain: e.chain(),
        environment: (e.id(), e.env.quota.as_ref()),
    };
    deploy(state, &mut tenant, authz, change).await?;
    let record = tenant
        .app(e.id(), name)
        .await?
        .ok_or_else(|| Error::Internal("the new app is missing".into()))?;
    tenant.commit().await?;
    Ok(AppDto::of(e.project.slug(), e.short_name(), &record, None))
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

#[cfg(test)]
mod tests {
    use kuben_core::ids::{ApplicationId, TargetId};
    use uuid::Uuid;

    use super::*;

    fn record() -> AppRecord {
        AppRecord {
            target: TargetId::new(),
            application: ApplicationId::new(),
            slug: "web".into(),
            name: "web".into(),
            namespace: "kb-shop-prod".into(),
            legacy_uid: None,
            deleting: false,
            lifecycle_uid: Uuid::now_v7(),
            desired_generation: Generation(1),
            created_at: 0,
            config_revision: None,
            config: Some(config_of(&sample_spec()).expect("config")),
            release: None,
            image: Some("nginx:1.27".into()),
            delivery: Delivery::Controller,
            runtime: None,
        }
    }

    #[test]
    fn the_configuration_is_the_spec_without_its_image() {
        let spec = sample_spec();
        let config = config_of(&spec).expect("config");
        assert!(config.get("source").is_none());
        let record = AppRecord {
            config: Some(config),
            ..record()
        };
        let back = desired_spec(&record).expect("spec");
        assert_eq!(spec_json(&back), spec_json(&spec), "the round trip is lossless");
        let dto = AppDto::of("shop", "prod", &record, None);
        assert_eq!(dto.image.as_deref(), Some("nginx:1.27"));
        assert_eq!(dto.processes[0].port, Some(80));
        assert!(!dto.ready, "not materialized yet");
    }

    #[test]
    fn an_agent_delivered_app_shows_its_agents_report() {
        let runtime = RuntimeStatus {
            generation: Generation(1),
            phase: "applying".into(),
            reason: None,
            message: None,
            observed_at: 0,
            url: Some("https://shop.example.com".into()),
        };
        let applying = AppRecord {
            delivery: Delivery::Agent,
            runtime: Some(runtime.clone()),
            ..record()
        };
        let dto = AppDto::of("shop", "prod", &applying, None);
        assert_eq!(
            (dto.ready, dto.reason.as_deref(), dto.url.as_deref()),
            (false, Some("Applying"), Some("https://shop.example.com"))
        );

        let ready = AppRecord {
            runtime: Some(RuntimeStatus {
                phase: "ready".into(),
                ..runtime.clone()
            }),
            ..applying.clone()
        };
        let dto = AppDto::of("shop", "prod", &ready, None);
        assert_eq!((dto.ready, dto.reason), (true, None));
        let deleting = AppRecord {
            deleting: true,
            ..ready
        };
        assert!(!AppDto::of("shop", "prod", &deleting, None).ready);

        let failed = AppRecord {
            runtime: Some(RuntimeStatus {
                phase: "failed".into(),
                reason: Some("ProgressDeadlineExceeded".into()),
                message: Some("web-web did not roll out".into()),
                ..runtime
            }),
            ..applying
        };
        let dto = AppDto::of("shop", "prod", &failed, None);
        assert_eq!(
            (dto.ready, dto.reason.as_deref(), dto.message.as_deref()),
            (
                false,
                Some("ProgressDeadlineExceeded"),
                Some("web-web did not roll out")
            )
        );
        let unreported = AppRecord {
            delivery: Delivery::Agent,
            ..record()
        };
        let dto = AppDto::of("shop", "prod", &unreported, None);
        assert_eq!(
            (dto.ready, dto.url),
            (false, None),
            "the agent has not reported yet"
        );
    }
}

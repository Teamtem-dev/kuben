//! Pure builders: Kuben resources → Kubernetes objects. No I/O here, so every
//! rule is unit-tested; the reconcilers only server-side-apply the output.

use std::collections::{BTreeMap, BTreeSet};

use k8s_openapi::{
    api::{
        apps::v1::{Deployment, DeploymentSpec, DeploymentStrategy, RollingUpdateDeployment},
        autoscaling::v2::{
            CrossVersionObjectReference, HorizontalPodAutoscaler, HorizontalPodAutoscalerSpec, MetricSpec,
            MetricTarget, ResourceMetricSource,
        },
        batch::v1::{CronJob, CronJobSpec, Job, JobSpec, JobTemplateSpec},
        core::v1::{
            Capabilities, Container, ContainerPort, EnvVar, EnvVarSource, HTTPGetAction, LimitRange,
            LimitRangeItem, LimitRangeSpec, Namespace, PersistentVolumeClaim, PersistentVolumeClaimSpec,
            PersistentVolumeClaimVolumeSource, PodSecurityContext, PodSpec, PodTemplateSpec, Probe,
            ResourceQuota, ResourceQuotaSpec, ResourceRequirements, SeccompProfile, SecretKeySelector,
            SecurityContext, Service, ServicePort, ServiceSpec, TCPSocketAction, Volume as PodVolume,
            VolumeMount, VolumeResourceRequirements,
        },
        networking::v1::{NetworkPolicy, NetworkPolicyIngressRule, NetworkPolicyPeer, NetworkPolicySpec},
    },
    apimachinery::pkg::{
        api::resource::Quantity,
        apis::meta::v1::{LabelSelector, LabelSelectorRequirement, ObjectMeta, OwnerReference},
        util::intstr::IntOrString,
    },
};
use kube::ResourceExt;
use kuben_crd::{App, Environment, EnvironmentType, KubenConfigSpec, Process, SizePreset, labels};
use serde_json::json;

/// Finalizer that implements the environment deletion policy.
pub const ENV_FINALIZER: &str = "kuben.dev/environment";
pub const QUOTA_NAME: &str = "kuben-quota";
pub const LIMITS_NAME: &str = "kuben-defaults";
pub const NETPOL_NAME: &str = "kuben-isolation";
/// Pod-template annotation bumped by "restart" to roll pods without a spec change.
pub const RESTARTED_AT: &str = "kuben.dev/restarted-at";
/// Label carrying the environment type on namespaces.
pub const ENV_TYPE: &str = "kuben.dev/environment-type";
/// Annotation on PVCs: retained when the App is deleted (no ownerReference).
pub const RETAIN: &str = "kuben.dev/retain";

/// Port every app Service listens on; the gateway routes here.
pub const SERVICE_PORT: i32 = 80;

/// Why an App cannot be turned into workloads (surfaced as a condition).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum BuildError {
    #[error("app has no processes")]
    NoProcesses,
    #[error("only one process may expose a port, found: {0}")]
    MultiplePorts(String),
    #[error("process `{process}` uses unknown size `{size}`")]
    UnknownSize { process: String, size: String },
    #[error("no image yet: git sources are deployed once a build has produced an image")]
    AwaitingBuild,
    #[error("set exactly one of source.image or source.git")]
    InvalidSource,
    #[error("apps with volumes run a single process with at most one replica (ReadWriteOnce)")]
    VolumeNeedsSingleReplica,
    #[error("scheduled process `{0}` cannot expose a port")]
    ScheduledWithPort(String),
    #[error("process `{process}` has an invalid schedule `{schedule}`")]
    InvalidSchedule { process: String, schedule: String },
}

impl BuildError {
    /// CamelCase reason for Kubernetes conditions.
    #[must_use]
    pub fn reason(&self) -> &'static str {
        match self {
            Self::NoProcesses => "NoProcesses",
            Self::MultiplePorts(_) => "MultiplePorts",
            Self::UnknownSize { .. } => "UnknownSize",
            Self::AwaitingBuild => "AwaitingBuild",
            Self::InvalidSource => "InvalidSource",
            Self::VolumeNeedsSingleReplica => "VolumeNeedsSingleReplica",
            Self::ScheduledWithPort(_) => "ScheduledWithPort",
            Self::InvalidSchedule { .. } => "InvalidSchedule",
        }
    }
}

/// Gateway API `Gateway` that app routes attach to.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GatewayRef {
    pub namespace: String,
    pub name: String,
}

/// Platform settings resolved from the `KubenConfig` singleton (or defaults).
#[derive(Clone, Debug)]
pub struct Platform {
    pub sizes: Vec<SizePreset>,
    pub base_domain: Option<String>,
    pub gateway: Option<GatewayRef>,
    /// Routes are served over TLS (a ClusterIssuer is configured).
    pub tls: bool,
    pub cluster_issuer: Option<String>,
    /// Secret with a `*.<base_domain>` certificate (scenario 9).
    pub wildcard_tls_secret: Option<String>,
}

impl Default for Platform {
    fn default() -> Self {
        Self::from_spec(None)
    }
}

impl Platform {
    #[must_use]
    pub fn from_spec(spec: Option<&KubenConfigSpec>) -> Self {
        let defaults;
        let spec = if let Some(s) = spec {
            s
        } else {
            // `{}` deserializes to the CRD defaults (size presets etc.).
            defaults = serde_json::from_value::<KubenConfigSpec>(json!({})).ok();
            match &defaults {
                Some(d) => d,
                None => {
                    return Self {
                        sizes: Vec::new(),
                        base_domain: None,
                        gateway: None,
                        tls: false,
                        cluster_issuer: None,
                        wildcard_tls_secret: None,
                    };
                }
            }
        };
        let gateway = spec.gateway.as_deref().and_then(|g| {
            let (ns, name) = g.split_once('/')?;
            (!ns.is_empty() && !name.is_empty()).then(|| GatewayRef {
                namespace: ns.into(),
                name: name.into(),
            })
        });
        Self {
            sizes: spec.sizes.clone(),
            base_domain: spec.base_domain.clone().filter(|d| !d.is_empty()),
            gateway,
            tls: spec.cluster_issuer.is_some(),
            cluster_issuer: spec.cluster_issuer.clone(),
            wildcard_tls_secret: spec.wildcard_tls_secret.clone().filter(|s| !s.is_empty()),
        }
    }

    fn size(&self, name: &str) -> Option<&SizePreset> {
        self.sizes.iter().find(|s| s.name == name)
    }
}

fn q(v: &str) -> Quantity {
    Quantity(v.to_owned())
}

fn managed_labels() -> BTreeMap<String, String> {
    BTreeMap::from([(labels::MANAGED_BY.to_owned(), labels::MANAGER.to_owned())])
}

// ---------------------------------------------------------------------------
// Environment → Namespace + guard rails
// ---------------------------------------------------------------------------

#[must_use]
pub fn namespace_name(environment: &str) -> String {
    format!("kb-{environment}")
}

const fn env_type_str(t: EnvironmentType) -> &'static str {
    match t {
        EnvironmentType::Standard => "standard",
        EnvironmentType::Production => "production",
        EnvironmentType::Preview => "preview",
    }
}

/// Namespace with Pod Security Admission: `baseline` is enforced (arbitrary
/// images still run), `restricted` violations are warned and audited.
#[must_use]
pub fn namespace(env: &Environment) -> Namespace {
    let name = env.name_any();
    let mut l = managed_labels();
    l.insert(labels::PROJECT.into(), env.spec.project.clone());
    l.insert(labels::ENVIRONMENT.into(), name.clone());
    l.insert(ENV_TYPE.into(), env_type_str(env.spec.type_).into());
    if let Some(org) = env.labels().get(labels::ORG) {
        l.insert(labels::ORG.into(), org.clone());
    }
    for (k, v) in [
        ("pod-security.kubernetes.io/enforce", "baseline"),
        ("pod-security.kubernetes.io/enforce-version", "latest"),
        ("pod-security.kubernetes.io/warn", "restricted"),
        ("pod-security.kubernetes.io/audit", "restricted"),
    ] {
        l.insert(k.into(), v.into());
    }
    Namespace {
        metadata: ObjectMeta {
            name: Some(namespace_name(&name)),
            labels: Some(l),
            ..ObjectMeta::default()
        },
        ..Namespace::default()
    }
}

/// Quota: tenants can never create LoadBalancer/NodePort services (cost and
/// exposure), plus the optional CPU/memory/pod caps from the spec.
#[must_use]
pub fn resource_quota(env: &Environment) -> ResourceQuota {
    let mut hard = BTreeMap::from([
        ("services.loadbalancers".to_owned(), q("0")),
        ("services.nodeports".to_owned(), q("0")),
    ]);
    if let Some(quota) = &env.spec.quota {
        if let Some(cpu) = &quota.cpu {
            hard.insert("requests.cpu".into(), q(cpu));
        }
        if let Some(mem) = &quota.memory {
            hard.insert("requests.memory".into(), q(mem));
        }
        if let Some(pods) = quota.pods {
            hard.insert("pods".into(), Quantity(pods.to_string()));
        }
    }
    ResourceQuota {
        metadata: ObjectMeta {
            name: Some(QUOTA_NAME.into()),
            namespace: Some(namespace_name(&env.name_any())),
            labels: Some(managed_labels()),
            ..ObjectMeta::default()
        },
        spec: Some(ResourceQuotaSpec {
            hard: Some(hard),
            ..ResourceQuotaSpec::default()
        }),
        ..ResourceQuota::default()
    }
}

/// Defaults for containers that declare no resources (keeps quota admission
/// working and bounds noisy neighbours).
#[must_use]
pub fn limit_range(env: &Environment) -> LimitRange {
    LimitRange {
        metadata: ObjectMeta {
            name: Some(LIMITS_NAME.into()),
            namespace: Some(namespace_name(&env.name_any())),
            labels: Some(managed_labels()),
            ..ObjectMeta::default()
        },
        spec: Some(LimitRangeSpec {
            limits: vec![LimitRangeItem {
                type_: "Container".into(),
                default: Some(BTreeMap::from([("memory".to_owned(), q("512Mi"))])),
                default_request: Some(BTreeMap::from([
                    ("cpu".to_owned(), q("50m")),
                    ("memory".to_owned(), q("64Mi")),
                ])),
                ..LimitRangeItem::default()
            }],
        }),
    }
}

/// Tenant isolation: ingress is allowed from the same namespace and from
/// namespaces Kuben does *not* manage (gateway, monitoring, kuben-system), so
/// environments cannot reach each other.
#[must_use]
pub fn network_policy(env: &Environment) -> NetworkPolicy {
    let same_namespace = NetworkPolicyPeer {
        pod_selector: Some(LabelSelector::default()),
        ..NetworkPolicyPeer::default()
    };
    let unmanaged_namespaces = NetworkPolicyPeer {
        namespace_selector: Some(LabelSelector {
            match_expressions: Some(vec![LabelSelectorRequirement {
                key: labels::MANAGED_BY.into(),
                operator: "NotIn".into(),
                values: Some(vec![labels::MANAGER.into()]),
            }]),
            match_labels: None,
        }),
        ..NetworkPolicyPeer::default()
    };
    NetworkPolicy {
        metadata: ObjectMeta {
            name: Some(NETPOL_NAME.into()),
            namespace: Some(namespace_name(&env.name_any())),
            labels: Some(managed_labels()),
            ..ObjectMeta::default()
        },
        spec: Some(NetworkPolicySpec {
            pod_selector: Some(LabelSelector::default()),
            policy_types: Some(vec!["Ingress".into()]),
            ingress: Some(vec![
                NetworkPolicyIngressRule {
                    from: Some(vec![same_namespace]),
                    ports: None,
                },
                NetworkPolicyIngressRule {
                    from: Some(vec![unmanaged_namespaces]),
                    ports: None,
                },
            ]),
            egress: None,
        }),
    }
}

// ---------------------------------------------------------------------------
// App → Deployments, Service, HPAs, HTTPRoute
// ---------------------------------------------------------------------------

/// Labels shared by everything an App owns (propagated org/project/env).
#[must_use]
pub fn app_labels(app: &App, process: Option<&str>) -> BTreeMap<String, String> {
    let mut l = managed_labels();
    for key in [labels::ORG, labels::PROJECT, labels::ENVIRONMENT] {
        if let Some(v) = app.labels().get(key) {
            l.insert(key.into(), v.clone());
        }
    }
    l.insert(labels::APP.into(), app.name_any());
    if let Some(p) = process {
        l.insert(labels::PROCESS.into(), p.into());
    }
    l
}

fn selector(app: &App, process: &str) -> BTreeMap<String, String> {
    BTreeMap::from([
        (labels::APP.to_owned(), app.name_any()),
        (labels::PROCESS.to_owned(), process.to_owned()),
    ])
}

#[must_use]
pub fn workload_name(app: &str, process: &str) -> String {
    format!("{app}-{process}")
}

/// Processes that run continuously (everything without a `schedule`).
fn long_running(app: &App) -> impl Iterator<Item = (&String, &Process)> {
    app.spec
        .runtime
        .processes
        .iter()
        .filter(|(_, p)| p.schedule.is_none())
}

/// The single long-running process that exposes a port (the one routed to).
pub fn web_process(app: &App) -> Result<Option<(&str, &Process)>, BuildError> {
    let exposed: Vec<(&String, &Process)> = long_running(app).filter(|(_, p)| p.port.is_some()).collect();
    match exposed.as_slice() {
        [] => Ok(None),
        [(name, process)] => Ok(Some((name.as_str(), *process))),
        many => Err(BuildError::MultiplePorts(
            many.iter()
                .map(|(n, _)| n.as_str())
                .collect::<Vec<_>>()
                .join(", "),
        )),
    }
}

/// Image to run. Git sources have none until a build produced one.
pub fn image(app: &App) -> Result<&str, BuildError> {
    match (&app.spec.source.image, &app.spec.source.git) {
        (Some(image), None) => Ok(image),
        (None, Some(_)) => Err(BuildError::AwaitingBuild),
        _ => Err(BuildError::InvalidSource),
    }
}

/// Syntax check for a CronJob schedule: five fields or an `@hourly`-style macro.
#[must_use]
pub fn valid_schedule(expr: &str) -> bool {
    let expr = expr.trim();
    if expr.starts_with('@') {
        return matches!(
            expr,
            "@yearly" | "@annually" | "@monthly" | "@weekly" | "@daily" | "@midnight" | "@hourly"
        );
    }
    let fields: Vec<&str> = expr.split_whitespace().collect();
    fields.len() == 5
        && fields.iter().all(|f| {
            f.chars()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, '*' | '/' | ',' | '-' | '?'))
        })
}

/// Cross-field rules the CRD schema cannot express. Every builder calls this
/// first, so an invalid App never produces a half-applied set of objects.
pub fn validate(app: &App) -> Result<(), BuildError> {
    if app.spec.runtime.processes.is_empty() {
        return Err(BuildError::NoProcesses);
    }
    for (name, p) in &app.spec.runtime.processes {
        if let Some(schedule) = &p.schedule {
            if p.port.is_some() {
                return Err(BuildError::ScheduledWithPort(name.clone()));
            }
            if !valid_schedule(schedule) {
                return Err(BuildError::InvalidSchedule {
                    process: name.clone(),
                    schedule: schedule.clone(),
                });
            }
        }
    }
    if !app.spec.volumes.is_empty() {
        let single = app.spec.runtime.processes.len() == 1
            && app.spec.runtime.processes.values().all(|p| p.replicas.max <= 1);
        if !single {
            return Err(BuildError::VolumeNeedsSingleReplica);
        }
    }
    web_process(app)?;
    image(app)?;
    Ok(())
}

fn env_vars(app: &App, port: Option<u16>) -> Vec<EnvVar> {
    let mut vars: Vec<EnvVar> = app
        .spec
        .env
        .iter()
        .map(|e| EnvVar {
            name: e.name.clone(),
            value: if e.from_secret.is_some() || e.from_service.is_some() {
                None
            } else {
                e.value.clone()
            },
            // Service bindings are published as Secrets named after the service.
            value_from: e
                .from_secret
                .as_ref()
                .or(e.from_service.as_ref())
                .map(|r| EnvVarSource {
                    secret_key_ref: Some(SecretKeySelector {
                        name: r.name.clone(),
                        key: r.key.clone(),
                        optional: None,
                    }),
                    ..EnvVarSource::default()
                }),
        })
        .collect();
    // Heroku/buildpack convention: tell the process which port to bind.
    if let Some(port) = port
        && !vars.iter().any(|v| v.name == "PORT")
    {
        vars.push(EnvVar {
            name: "PORT".into(),
            value: Some(port.to_string()),
            value_from: None,
        });
    }
    vars
}

fn resources(preset: &SizePreset) -> ResourceRequirements {
    let mut limits = BTreeMap::from([("memory".to_owned(), q(&preset.memory_limit))]);
    if let Some(cpu) = &preset.cpu_limit {
        limits.insert("cpu".into(), q(cpu));
    }
    ResourceRequirements {
        requests: Some(BTreeMap::from([
            ("cpu".to_owned(), q(&preset.cpu_request)),
            ("memory".to_owned(), q(&preset.memory_request)),
        ])),
        limits: Some(limits),
        ..ResourceRequirements::default()
    }
}

/// `(readiness, startup, liveness)`. Readiness gates traffic; startup gives
/// slow boots up to 5 minutes before the other probes start; liveness only
/// runs with an explicit health path (a TCP liveness check would restart
/// apps that are merely busy).
fn probes(app: &App, port: u16) -> (Probe, Probe, Option<Probe>) {
    let target = |p: Option<u16>| IntOrString::Int(i32::from(p.unwrap_or(port)));
    let health = app.spec.runtime.health_check.as_ref();
    let (http_get, tcp_socket) = match health {
        Some(h) => (
            Some(HTTPGetAction {
                path: Some(h.path.clone()),
                port: target(h.port),
                ..HTTPGetAction::default()
            }),
            None,
        ),
        None => (
            None,
            Some(TCPSocketAction {
                port: target(None),
                host: None,
            }),
        ),
    };
    let base = Probe {
        http_get,
        tcp_socket,
        timeout_seconds: Some(3),
        ..Probe::default()
    };
    let readiness = Probe {
        period_seconds: Some(10),
        failure_threshold: Some(3),
        ..base.clone()
    };
    let startup = Probe {
        period_seconds: Some(5),
        failure_threshold: Some(60),
        ..base.clone()
    };
    let liveness = health.map(|_| Probe {
        period_seconds: Some(20),
        timeout_seconds: Some(5),
        failure_threshold: Some(3),
        ..base
    });
    (readiness, startup, liveness)
}

fn hardened() -> SecurityContext {
    SecurityContext {
        allow_privilege_escalation: Some(false),
        capabilities: Some(Capabilities {
            drop: Some(vec!["NET_RAW".into()]),
            add: None,
        }),
        seccomp_profile: Some(SeccompProfile {
            type_: "RuntimeDefault".into(),
            localhost_profile: None,
        }),
        ..SecurityContext::default()
    }
}

/// One container per process. `serve`: long-running (gets probes).
fn container(
    app: &App,
    name: &str,
    process: &Process,
    image: &str,
    preset: &SizePreset,
    serve: bool,
) -> Container {
    let (readiness_probe, startup_probe, liveness_probe) = match process.port.filter(|_| serve) {
        Some(port) => {
            let (r, s, l) = probes(app, port);
            (Some(r), Some(s), l)
        }
        None => (None, None, None),
    };
    Container {
        name: name.to_owned(),
        image: Some(image.to_owned()),
        command: (!process.command.is_empty()).then(|| process.command.clone()),
        ports: process.port.map(|p| {
            vec![ContainerPort {
                name: Some(if process.protocol.is_http() { "http" } else { "tcp" }.into()),
                container_port: i32::from(p),
                protocol: Some("TCP".into()),
                ..ContainerPort::default()
            }]
        }),
        env: Some(env_vars(app, process.port)),
        resources: Some(resources(preset)),
        readiness_probe,
        startup_probe,
        liveness_probe,
        volume_mounts: (!app.spec.volumes.is_empty()).then(|| {
            app.spec
                .volumes
                .iter()
                .map(|v| VolumeMount {
                    name: v.name.clone(),
                    mount_path: v.mount_path.clone(),
                    ..VolumeMount::default()
                })
                .collect()
        }),
        security_context: Some(hardened()),
        ..Container::default()
    }
}

fn pod_spec(app: &App, container: Container, restart_policy: Option<&str>) -> PodSpec {
    PodSpec {
        containers: vec![container],
        volumes: (!app.spec.volumes.is_empty()).then(|| {
            app.spec
                .volumes
                .iter()
                .map(|v| PodVolume {
                    name: v.name.clone(),
                    persistent_volume_claim: Some(PersistentVolumeClaimVolumeSource {
                        claim_name: pvc_name(&app.name_any(), &v.name),
                        read_only: None,
                    }),
                    ..PodVolume::default()
                })
                .collect()
        }),
        restart_policy: restart_policy.map(str::to_owned),
        automount_service_account_token: Some(false),
        enable_service_links: Some(false),
        security_context: Some(PodSecurityContext {
            seccomp_profile: Some(SeccompProfile {
                type_: "RuntimeDefault".into(),
                localhost_profile: None,
            }),
            fs_group: app.spec.runtime.fs_group,
            fs_group_change_policy: app.spec.runtime.fs_group.map(|_| "OnRootMismatch".to_owned()),
            ..PodSecurityContext::default()
        }),
        termination_grace_period_seconds: Some(30),
        ..PodSpec::default()
    }
}

fn preset<'a>(platform: &'a Platform, pname: &str, process: &Process) -> Result<&'a SizePreset, BuildError> {
    platform
        .size(&process.size)
        .ok_or_else(|| BuildError::UnknownSize {
            process: pname.to_owned(),
            size: process.size.clone(),
        })
}

/// Autoscaling is on when `max > min`; then the HPA owns `replicas`.
const fn autoscaled(p: &Process) -> bool {
    p.replicas.max > p.replicas.min && p.schedule.is_none()
}

/// One Deployment per long-running process.
pub fn deployments(
    app: &App,
    platform: &Platform,
    owner: &OwnerReference,
) -> Result<Vec<Deployment>, BuildError> {
    validate(app)?;
    let image = image(app)?;
    let restarted_at = app.annotations().get(RESTARTED_AT).cloned();
    // ReadWriteOnce volumes cannot be attached to the old and new pod at once.
    let strategy = if app.spec.volumes.is_empty() {
        DeploymentStrategy {
            type_: Some("RollingUpdate".into()),
            rolling_update: Some(RollingUpdateDeployment {
                max_surge: Some(IntOrString::Int(1)),
                max_unavailable: Some(IntOrString::Int(0)),
            }),
        }
    } else {
        DeploymentStrategy {
            type_: Some("Recreate".into()),
            rolling_update: None,
        }
    };
    let mut out = Vec::new();
    for (pname, process) in long_running(app) {
        let container = container(
            app,
            pname,
            process,
            image,
            preset(platform, pname, process)?,
            true,
        );
        let pod_annotations = restarted_at
            .as_ref()
            .map(|at| BTreeMap::from([(RESTARTED_AT.to_owned(), at.clone())]));
        out.push(Deployment {
            metadata: ObjectMeta {
                name: Some(workload_name(&app.name_any(), pname)),
                namespace: app.namespace(),
                labels: Some(app_labels(app, Some(pname))),
                owner_references: Some(vec![owner.clone()]),
                ..ObjectMeta::default()
            },
            spec: Some(DeploymentSpec {
                replicas: (!autoscaled(process))
                    .then(|| i32::try_from(process.replicas.min).unwrap_or(i32::MAX)),
                revision_history_limit: Some(5),
                progress_deadline_seconds: Some(600),
                selector: LabelSelector {
                    match_labels: Some(selector(app, pname)),
                    match_expressions: None,
                },
                strategy: Some(strategy.clone()),
                template: PodTemplateSpec {
                    metadata: Some(ObjectMeta {
                        labels: Some(app_labels(app, Some(pname))),
                        annotations: pod_annotations,
                        ..ObjectMeta::default()
                    }),
                    spec: Some(pod_spec(app, container, None)),
                },
                ..DeploymentSpec::default()
            }),
            ..Deployment::default()
        });
    }
    Ok(out)
}

/// One CronJob per scheduled process (scenario 7). Overlapping runs are
/// skipped (`Forbid`), a run that could not start within 5 minutes is
/// dropped, and finished Jobs are garbage-collected after a day.
pub fn cron_jobs(app: &App, platform: &Platform, owner: &OwnerReference) -> Result<Vec<CronJob>, BuildError> {
    validate(app)?;
    let scheduled: Vec<(&String, &Process)> = app
        .spec
        .runtime
        .processes
        .iter()
        .filter(|(_, p)| p.schedule.is_some())
        .collect();
    if scheduled.is_empty() {
        return Ok(Vec::new());
    }
    let image = image(app)?;
    let mut out = Vec::with_capacity(scheduled.len());
    for (pname, process) in scheduled {
        let container = container(
            app,
            pname,
            process,
            image,
            preset(platform, pname, process)?,
            false,
        );
        out.push(CronJob {
            metadata: ObjectMeta {
                name: Some(workload_name(&app.name_any(), pname)),
                namespace: app.namespace(),
                labels: Some(app_labels(app, Some(pname))),
                owner_references: Some(vec![owner.clone()]),
                ..ObjectMeta::default()
            },
            spec: Some(CronJobSpec {
                schedule: process.schedule.clone().unwrap_or_default(),
                time_zone: process.time_zone.clone(),
                concurrency_policy: Some("Forbid".into()),
                starting_deadline_seconds: Some(300),
                successful_jobs_history_limit: Some(3),
                failed_jobs_history_limit: Some(3),
                job_template: JobTemplateSpec {
                    metadata: Some(ObjectMeta {
                        labels: Some(app_labels(app, Some(pname))),
                        ..ObjectMeta::default()
                    }),
                    spec: Some(JobSpec {
                        backoff_limit: Some(1),
                        active_deadline_seconds: Some(3600),
                        ttl_seconds_after_finished: Some(86_400),
                        template: PodTemplateSpec {
                            metadata: Some(ObjectMeta {
                                labels: Some(app_labels(app, Some(pname))),
                                ..ObjectMeta::default()
                            }),
                            spec: Some(pod_spec(app, container, Some("Never"))),
                        },
                        ..JobSpec::default()
                    }),
                },
                ..CronJobSpec::default()
            }),
            status: None,
        });
    }
    Ok(out)
}

/// A one-off Job from a CronJob's template ("run now"), equivalent to
/// `kubectl create job --from=cronjob/<name>`.
#[must_use]
pub fn job_from_cron(cron: &CronJob, name: &str) -> Option<Job> {
    let template = cron.spec.as_ref()?.job_template.clone();
    let job_labels = template.metadata.as_ref().and_then(|m| m.labels.clone());
    Some(Job {
        metadata: ObjectMeta {
            name: Some(name.to_owned()),
            namespace: cron.metadata.namespace.clone(),
            labels: job_labels,
            annotations: Some(BTreeMap::from([(
                "cronjob.kubernetes.io/instantiate".to_owned(),
                "manual".to_owned(),
            )])),
            owner_references: cron.metadata.uid.clone().map(|uid| {
                vec![OwnerReference {
                    api_version: "batch/v1".into(),
                    kind: "CronJob".into(),
                    name: cron.name_any(),
                    uid,
                    controller: Some(false),
                    block_owner_deletion: None,
                }]
            }),
            ..ObjectMeta::default()
        },
        spec: template.spec,
        status: None,
    })
}

#[must_use]
pub fn pvc_name(app: &str, volume: &str) -> String {
    format!("{app}-{volume}")
}

/// PVCs for the app's volumes (scenario 6). Deliberately **without** an
/// ownerReference: deleting an App never deletes its data; the API removes
/// them only on an explicit `delete_volumes=true`.
#[must_use]
pub fn persistent_volume_claims(app: &App) -> Vec<PersistentVolumeClaim> {
    app.spec
        .volumes
        .iter()
        .map(|v| PersistentVolumeClaim {
            metadata: ObjectMeta {
                name: Some(pvc_name(&app.name_any(), &v.name)),
                namespace: app.namespace(),
                labels: Some(app_labels(app, None)),
                annotations: Some(BTreeMap::from([(RETAIN.to_owned(), "true".to_owned())])),
                ..ObjectMeta::default()
            },
            spec: Some(PersistentVolumeClaimSpec {
                access_modes: Some(vec!["ReadWriteOnce".into()]),
                storage_class_name: v.storage_class.clone(),
                resources: Some(VolumeResourceRequirements {
                    requests: Some(BTreeMap::from([("storage".to_owned(), q(&v.size))])),
                    limits: None,
                }),
                ..PersistentVolumeClaimSpec::default()
            }),
            ..PersistentVolumeClaim::default()
        })
        .collect()
}

/// HPAs for autoscaled processes (target: 80 % CPU of the request).
#[must_use]
pub fn autoscalers(app: &App, owner: &OwnerReference) -> Vec<HorizontalPodAutoscaler> {
    app.spec
        .runtime
        .processes
        .iter()
        .filter(|(_, p)| autoscaled(p))
        .map(|(pname, p)| {
            let name = workload_name(&app.name_any(), pname);
            HorizontalPodAutoscaler {
                metadata: ObjectMeta {
                    name: Some(name.clone()),
                    namespace: app.namespace(),
                    labels: Some(app_labels(app, Some(pname))),
                    owner_references: Some(vec![owner.clone()]),
                    ..ObjectMeta::default()
                },
                spec: Some(HorizontalPodAutoscalerSpec {
                    scale_target_ref: CrossVersionObjectReference {
                        api_version: Some("apps/v1".into()),
                        kind: "Deployment".into(),
                        name,
                    },
                    min_replicas: Some(i32::try_from(p.replicas.min.max(1)).unwrap_or(1)),
                    max_replicas: i32::try_from(p.replicas.max).unwrap_or(i32::MAX),
                    metrics: Some(vec![MetricSpec {
                        type_: "Resource".into(),
                        resource: Some(ResourceMetricSource {
                            name: "cpu".into(),
                            target: MetricTarget {
                                type_: "Utilization".into(),
                                average_utilization: Some(80),
                                ..MetricTarget::default()
                            },
                        }),
                        ..MetricSpec::default()
                    }]),
                    behavior: None,
                }),
                ..HorizontalPodAutoscaler::default()
            }
        })
        .collect()
}

/// ClusterIP Service in front of the web process. HTTP: `:80` → container
/// port (the gateway routes here). TCP: the real port, cluster-internal only.
pub fn service(app: &App, owner: &OwnerReference) -> Result<Option<Service>, BuildError> {
    let Some((pname, process)) = web_process(app)? else {
        return Ok(None);
    };
    let target = process.port.map_or(8080, i32::from);
    let http = process.protocol.is_http();
    Ok(Some(Service {
        metadata: ObjectMeta {
            name: Some(app.name_any()),
            namespace: app.namespace(),
            labels: Some(app_labels(app, None)),
            owner_references: Some(vec![owner.clone()]),
            ..ObjectMeta::default()
        },
        spec: Some(ServiceSpec {
            type_: Some("ClusterIP".into()),
            selector: Some(selector(app, pname)),
            ports: Some(vec![ServicePort {
                name: Some(if http { "http" } else { "tcp" }.into()),
                port: if http { SERVICE_PORT } else { target },
                target_port: Some(IntOrString::Int(target)),
                protocol: Some("TCP".into()),
                app_protocol: http.then(|| "http".to_owned()),
                ..ServicePort::default()
            }]),
            ..ServiceSpec::default()
        }),
        ..Service::default()
    }))
}

/// Hostnames: explicit domains first, then `<app>-<environment>.<base_domain>`.
#[must_use]
pub fn hostnames_for<'a>(
    app: &str,
    environment: Option<&str>,
    domains: impl IntoIterator<Item = &'a str>,
    platform: &Platform,
) -> Vec<String> {
    let mut hosts: Vec<String> = domains.into_iter().map(str::to_ascii_lowercase).collect();
    if let (Some(base), Some(env)) = (&platform.base_domain, environment) {
        let generated = format!("{app}-{env}.{base}");
        if !hosts.contains(&generated) {
            hosts.push(generated);
        }
    }
    hosts
}

#[must_use]
pub fn hostnames(app: &App, platform: &Platform) -> Vec<String> {
    hostnames_for(
        &app.name_any(),
        app.labels().get(labels::ENVIRONMENT).map(String::as_str),
        app.spec.domains.iter().map(|d| d.host.as_str()),
        platform,
    )
}

/// Public URL shown in the UI (first hostname).
#[must_use]
pub fn url(app: &App, platform: &Platform) -> Option<String> {
    let scheme = if platform.tls { "https" } else { "http" };
    hostnames(app, platform)
        .first()
        .map(|h| format!("{scheme}://{h}"))
}

/// Gateway API `HTTPRoute` (as JSON: the Gateway API types are not part of
/// k8s-openapi). `None` without a gateway, an HTTP web process or a host.
/// With TLS the route attaches only to the HTTPS listeners of its hosts
/// (scenario 9); plain HTTP is answered by the platform redirect.
pub fn http_route(
    app: &App,
    platform: &Platform,
    owner: &OwnerReference,
) -> Result<Option<serde_json::Value>, BuildError> {
    let Some(gateway) = &platform.gateway else {
        return Ok(None);
    };
    match web_process(app)? {
        Some((_, p)) if p.protocol.is_http() => {}
        _ => return Ok(None),
    }
    let hosts = hostnames(app, platform);
    if hosts.is_empty() {
        return Ok(None);
    }
    let parent_refs: Vec<serde_json::Value> = if platform.tls {
        let sections: BTreeSet<String> = hosts
            .iter()
            .map(|h| super::gateway::section_for_host(h, platform))
            .collect();
        sections
            .into_iter()
            .map(|s| json!({ "name": gateway.name, "namespace": gateway.namespace, "sectionName": s }))
            .collect()
    } else {
        vec![json!({ "name": gateway.name, "namespace": gateway.namespace })]
    };
    Ok(Some(json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "HTTPRoute",
        "metadata": {
            "name": app.name_any(),
            "namespace": app.namespace(),
            "labels": app_labels(app, None),
            "ownerReferences": [owner],
        },
        "spec": {
            "parentRefs": parent_refs,
            "hostnames": hosts,
            "rules": [{
                "matches": [{ "path": { "type": "PathPrefix", "value": "/" } }],
                "backendRefs": [{ "name": app.name_any(), "port": SERVICE_PORT }],
            }],
        },
    })))
}

#[cfg(test)]
mod tests {
    use kube::Resource;
    use kuben_crd::{AppSpec, EnvironmentSpec, Quota};

    use super::*;

    fn env(quota: Option<Quota>) -> Environment {
        let mut e = Environment::new(
            "shop-prod",
            serde_json::from_value::<EnvironmentSpec>(json!({ "project": "shop", "type": "production" }))
                .expect("spec"),
        );
        e.spec.quota = quota;
        e.metadata.labels = Some(BTreeMap::from([(labels::ORG.to_owned(), "org-1".to_owned())]));
        e
    }

    fn app(spec: serde_json::Value) -> App {
        let mut a = App::new("api", serde_json::from_value::<AppSpec>(spec).expect("app spec"));
        a.metadata.namespace = Some("kb-shop-prod".into());
        a.metadata.uid = Some("uid-1".into());
        a.metadata.labels = Some(BTreeMap::from([
            (labels::PROJECT.to_owned(), "shop".to_owned()),
            (labels::ENVIRONMENT.to_owned(), "shop-prod".to_owned()),
        ]));
        a
    }

    fn web_app() -> App {
        app(json!({
            "source": { "image": "ghcr.io/acme/api:1.2.3" },
            "runtime": { "processes": {
                "web": { "port": 3000, "size": "small", "replicas": { "min": 2, "max": 2 } },
                "worker": { "command": ["bin/worker"], "size": "nano" }
            }, "healthCheck": { "path": "/healthz" } },
            "env": [
                { "name": "LOG_LEVEL", "value": "info" },
                { "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } }
            ],
            "domains": [{ "host": "API.acme.com" }]
        }))
    }

    fn owner(a: &App) -> OwnerReference {
        a.controller_owner_ref(&()).expect("owner ref")
    }

    fn platform() -> Platform {
        let spec = serde_json::from_value::<KubenConfigSpec>(json!({
            "baseDomain": "apps.example.com",
            "gateway": "kuben-system/kuben",
            "clusterIssuer": "letsencrypt"
        }))
        .expect("config");
        Platform::from_spec(Some(&spec))
    }

    #[test]
    fn namespace_has_psa_and_ownership_labels() {
        let ns = namespace(&env(None));
        assert_eq!(ns.metadata.name.as_deref(), Some("kb-shop-prod"));
        let l = ns.metadata.labels.expect("labels");
        assert_eq!(l["pod-security.kubernetes.io/enforce"], "baseline");
        assert_eq!(l["pod-security.kubernetes.io/warn"], "restricted");
        assert_eq!(l[labels::MANAGED_BY], "kuben");
        assert_eq!(l[labels::PROJECT], "shop");
        assert_eq!(l[labels::ORG], "org-1");
        assert_eq!(l[ENV_TYPE], "production");
    }

    #[test]
    fn quota_always_blocks_loadbalancers_and_adds_caps() {
        let hard = |e: &Environment| resource_quota(e).spec.and_then(|s| s.hard).expect("hard");
        let base = hard(&env(None));
        assert_eq!(base["services.loadbalancers"], q("0"));
        assert_eq!(base["services.nodeports"], q("0"));
        assert!(!base.contains_key("requests.cpu"));
        let capped = hard(&env(Some(Quota {
            cpu: Some("4".into()),
            memory: Some("8Gi".into()),
            pods: Some(50),
        })));
        assert_eq!(capped["requests.cpu"], q("4"));
        assert_eq!(capped["requests.memory"], q("8Gi"));
        assert_eq!(capped["pods"], q("50"));
    }

    #[test]
    fn network_policy_isolates_managed_namespaces() {
        let np = network_policy(&env(None));
        let spec = np.spec.expect("spec");
        assert_eq!(spec.policy_types.as_deref(), Some(&["Ingress".to_owned()][..]));
        let rules = spec.ingress.expect("ingress");
        assert_eq!(rules.len(), 2);
        let ns_sel = rules[1].from.as_ref().expect("from")[0]
            .namespace_selector
            .as_ref()
            .expect("ns selector");
        let req = &ns_sel.match_expressions.as_ref().expect("expr")[0];
        assert_eq!(
            (req.key.as_str(), req.operator.as_str()),
            (labels::MANAGED_BY, "NotIn")
        );
    }

    #[test]
    fn deployments_are_hardened_and_resourced() {
        let a = web_app();
        let deps = deployments(&a, &Platform::default(), &owner(&a)).expect("deployments");
        assert_eq!(deps.len(), 2);
        let web = deps
            .iter()
            .find(|d| d.metadata.name.as_deref() == Some("api-web"))
            .expect("web");
        let spec = web.spec.as_ref().expect("spec");
        assert_eq!(spec.replicas, Some(2));
        let pod = spec.template.spec.as_ref().expect("pod");
        assert_eq!(pod.automount_service_account_token, Some(false));
        let c = &pod.containers[0];
        assert_eq!(c.image.as_deref(), Some("ghcr.io/acme/api:1.2.3"));
        let sc = c.security_context.as_ref().expect("sc");
        assert_eq!(sc.allow_privilege_escalation, Some(false));
        let env = c.env.as_ref().expect("env");
        assert!(
            env.iter()
                .any(|v| v.name == "PORT" && v.value.as_deref() == Some("3000"))
        );
        let db = env.iter().find(|v| v.name == "DATABASE_URL").expect("db");
        assert!(db.value.is_none(), "secret values are never inlined");
        assert_eq!(
            db.value_from
                .as_ref()
                .and_then(|s| s.secret_key_ref.as_ref())
                .map(|r| r.key.as_str()),
            Some("url")
        );
        let probe = c
            .readiness_probe
            .as_ref()
            .and_then(|p| p.http_get.as_ref())
            .expect("http probe");
        assert_eq!(probe.path.as_deref(), Some("/healthz"));
        assert_eq!(
            c.startup_probe.as_ref().and_then(|p| p.failure_threshold),
            Some(60),
            "slow boots get 5 minutes"
        );
        assert!(c.liveness_probe.is_some(), "health path → liveness");
        assert_eq!(spec.progress_deadline_seconds, Some(600));
        assert_eq!(
            spec.strategy.as_ref().and_then(|s| s.type_.as_deref()),
            Some("RollingUpdate")
        );
        let req = c
            .resources
            .as_ref()
            .and_then(|r| r.requests.as_ref())
            .expect("requests");
        assert_eq!(req["memory"], q("128Mi"));
        assert_eq!(
            web.metadata.owner_references.as_ref().expect("owner")[0].uid,
            "uid-1"
        );

        let worker = deps
            .iter()
            .find(|d| d.metadata.name.as_deref() == Some("api-worker"))
            .expect("worker");
        let wc = &worker
            .spec
            .as_ref()
            .and_then(|s| s.template.spec.as_ref())
            .expect("pod")
            .containers[0];
        assert!(wc.ports.is_none() && wc.readiness_probe.is_none());
        assert_eq!(wc.command.as_deref(), Some(&["bin/worker".to_owned()][..]));
    }

    #[test]
    fn autoscaled_process_leaves_replicas_to_the_hpa() {
        let a = app(json!({
            "source": { "image": "nginx:1.27" },
            "runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 2, "max": 6 } } } }
        }));
        let o = owner(&a);
        let deps = deployments(&a, &Platform::default(), &o).expect("deployments");
        assert_eq!(deps[0].spec.as_ref().expect("spec").replicas, None);
        let hpas = autoscalers(&a, &o);
        let spec = hpas[0].spec.as_ref().expect("hpa");
        assert_eq!((spec.min_replicas, spec.max_replicas), (Some(2), 6));
    }

    #[test]
    fn restart_annotation_reaches_the_pod_template() {
        let mut a = web_app();
        a.metadata.annotations = Some(BTreeMap::from([(
            RESTARTED_AT.to_owned(),
            "2026-09-11T00:00:00Z".to_owned(),
        )]));
        let deps = deployments(&a, &Platform::default(), &owner(&a)).expect("deployments");
        let ann = deps[0]
            .spec
            .as_ref()
            .and_then(|s| s.template.metadata.as_ref())
            .and_then(|m| m.annotations.as_ref());
        assert_eq!(
            ann.map(|a| a[RESTARTED_AT].as_str()),
            Some("2026-09-11T00:00:00Z")
        );

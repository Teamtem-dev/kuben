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
use serde::{Deserialize, Serialize};
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
/// Annotation on an app's `HTTPRoute`: its domains and how each is secured
/// (M2.3), read by the gateway controller and the exposure view whichever
/// path (App controller or agent) wrote the route.
pub const DOMAINS_ANNOTATION: &str = "kuben.dev/domains";
/// Name suffix of the `ReferenceGrant` that lets the Gateway read an app's
/// own certificate Secrets.
pub const GRANT_SUFFIX: &str = "-tls";

/// How one hostname is served.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TlsMode {
    /// A certificate from the configured ClusterIssuer.
    Auto,
    /// Plain HTTP only (`tls: none`).
    Plain,
    /// The app's own certificate, in this Secret of its namespace.
    Secret(String),
}

impl TlsMode {
    /// `auto` (or empty), `none`, or a Secret name.
    pub fn parse(tls: &str) -> Option<Self> {
        match tls {
            "" | "auto" => Some(Self::Auto),
            "none" => Some(Self::Plain),
            name if is_dns_label(name) => Some(Self::Secret(name.to_owned())),
            _ => None,
        }
    }
}

/// A DNS-1123 subdomain, as Kubernetes object names are.
fn is_dns_label(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 253
        && name.split('.').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
                && !part.starts_with('-')
                && !part.ends_with('-')
        })
}

/// A hostname of an app and its `tls` setting, as the route annotation
/// carries them.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct DomainClaim {
    pub host: String,
    pub tls: String,
}

impl DomainClaim {
    /// The TLS mode; an unreadable setting counts as `auto` (validation
    /// refuses it before anything is built).
    #[must_use]
    pub fn mode(&self) -> TlsMode {
        TlsMode::parse(&self.tls).unwrap_or(TlsMode::Auto)
    }
}

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
    #[error("domain `{host}` has tls `{tls}`: use `auto`, `none` or the name of a Secret")]
    InvalidDomainTls { host: String, tls: String },
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
            Self::InvalidDomainTls { .. } => "InvalidDomainTls",
        }
    }
}

/// Gateway API `Gateway` that app routes attach to.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GatewayRef {
    pub namespace: String,
    pub name: String,
}

impl GatewayRef {
    /// The Gateway Kuben creates when only a GatewayClass is configured.
    #[must_use]
    pub fn owned_default() -> Self {
        Self {
            namespace: "kuben-system".into(),
            name: "kuben".into(),
        }
    }
}

/// Platform settings resolved from the `KubenConfig` singleton (or defaults).
#[derive(Clone, Debug)]
pub struct Platform {
    pub sizes: Vec<SizePreset>,
    pub base_domain: Option<String>,
    pub gateway: Option<GatewayRef>,
    /// Kuben creates and owns `gateway` with this class (M2.2).
    pub gateway_class: Option<String>,
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
                        gateway_class: None,
                        tls: false,
                        cluster_issuer: None,
                        wildcard_tls_secret: None,
                    };
                }
            }
        };
        let gateway_class = spec.gateway_class_name.clone().filter(|c| !c.is_empty());
        let gateway = match spec.gateway.as_deref().filter(|g| !g.is_empty()) {
            Some(g) => g.split_once('/').and_then(|(ns, name)| {
                (!ns.is_empty() && !name.is_empty()).then(|| GatewayRef {
                    namespace: ns.into(),
                    name: name.into(),
                })
            }),
            None => gateway_class.as_ref().map(|_| GatewayRef::owned_default()),
        };
        Self {
            sizes: spec.sizes.clone(),
            base_domain: spec.base_domain.clone().filter(|d| !d.is_empty()),
            gateway,
            gateway_class,
            tls: spec.cluster_issuer.is_some(),
            cluster_issuer: spec.cluster_issuer.clone(),
            wildcard_tls_secret: spec.wildcard_tls_secret.clone().filter(|s| !s.is_empty()),
        }
    }

    /// Narrow the settings to what the cluster can do (ADR-031's capability
    /// gate): TLS needs the configured ClusterIssuer to be Ready. Without
    /// facts, or when the probe failed, the configured intent stands. A
    /// missing capability blocks only TLS; plain HTTP routing still works.
    #[must_use]
    pub fn gated(mut self, facts: Option<&crate::discovery::ClusterFacts>) -> Self {
        if let (Some(facts), Some(issuer)) = (facts, &self.cluster_issuer)
            && !facts.issuer(issuer).usable_or_unknown()
        {
            self.tls = false;
        }
        self
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
    for domain in &app.spec.domains {
        if TlsMode::parse(&domain.tls).is_none() {
            return Err(BuildError::InvalidDomainTls {
                host: domain.host.clone(),
                tls: domain.tls.clone(),
            });
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

/// Hostnames with their TLS setting: explicit domains first, then the
/// generated one (always `auto`).
#[must_use]
pub fn domain_claims(app: &App, platform: &Platform) -> Vec<DomainClaim> {
    let explicit: BTreeMap<String, &str> = app
        .spec
        .domains
        .iter()
        .map(|d| (d.host.to_ascii_lowercase(), d.tls.as_str()))
        .collect();
    hostnames(app, platform)
        .into_iter()
        .map(|host| {
            let tls = explicit.get(&host).copied().unwrap_or("auto");
            DomainClaim {
                tls: if tls.is_empty() {
                    "auto".into()
                } else {
                    tls.to_owned()
                },
                host,
            }
        })
        .collect()
}

/// Whether `claim` is served over HTTPS on `platform`.
#[must_use]
pub fn secured(claim: &DomainClaim, platform: &Platform) -> bool {
    platform.tls && claim.mode() != TlsMode::Plain
}

/// Public URL shown in the UI (first hostname, with the scheme it is
/// served with).
#[must_use]
pub fn url(app: &App, platform: &Platform) -> Option<String> {
    domain_claims(app, platform).first().map(|claim| {
        let scheme = if secured(claim, platform) { "https" } else { "http" };
        format!("{scheme}://{}", claim.host)
    })
}

/// The app's web process is served over HTTP through the gateway.
fn routes_http(app: &App) -> Result<bool, BuildError> {
    Ok(matches!(web_process(app)?, Some((_, p)) if p.protocol.is_http()))
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
    if !routes_http(app)? {
        return Ok(None);
    }
    let claims = domain_claims(app, platform);
    if claims.is_empty() {
        return Ok(None);
    }
    let parent_refs: Vec<serde_json::Value> = if platform.tls {
        let sections: BTreeSet<String> = claims
            .iter()
            .map(|c| super::gateway::section_for(c, platform))
            .collect();
        sections
            .into_iter()
            .map(|s| json!({ "name": gateway.name, "namespace": gateway.namespace, "sectionName": s }))
            .collect()
    } else {
        vec![json!({ "name": gateway.name, "namespace": gateway.namespace })]
    };
    let hosts: Vec<&str> = claims.iter().map(|c| c.host.as_str()).collect();
    let domains = serde_json::to_string(&claims).unwrap_or_default();
    Ok(Some(json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "HTTPRoute",
        "metadata": {
            "name": app.name_any(),
            "namespace": app.namespace(),
            "labels": app_labels(app, None),
            "annotations": { DOMAINS_ANNOTATION: domains },
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

/// A `ReferenceGrant` that lets the Gateway read the Secrets of the app's
/// own certificates (`tls: <secret>`), in the app's namespace. `None` when no
/// domain brings its own certificate or TLS is off.
pub fn reference_grant(
    app: &App,
    platform: &Platform,
    owner: &OwnerReference,
) -> Result<Option<serde_json::Value>, BuildError> {
    let Some(gateway) = &platform.gateway else {
        return Ok(None);
    };
    if !platform.tls || !routes_http(app)? {
        return Ok(None);
    }
    let secrets: BTreeSet<String> = domain_claims(app, platform)
        .iter()
        .filter_map(|c| match c.mode() {
            TlsMode::Secret(name) => Some(name),
            TlsMode::Auto | TlsMode::Plain => None,
        })
        .collect();
    if secrets.is_empty() {
        return Ok(None);
    }
    let to: Vec<serde_json::Value> = secrets
        .into_iter()
        .map(|name| json!({ "group": "", "kind": "Secret", "name": name }))
        .collect();
    Ok(Some(json!({
        "apiVersion": "gateway.networking.k8s.io/v1beta1",
        "kind": "ReferenceGrant",
        "metadata": {
            "name": format!("{}{GRANT_SUFFIX}", app.name_any()),
            "namespace": app.namespace(),
            "labels": app_labels(app, None),
            "ownerReferences": [owner],
        },
        "spec": {
            "from": [{ "group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": gateway.namespace }],
            "to": to,
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
    }

    #[test]
    fn invalid_apps_are_rejected_with_a_reason() {
        let two_ports = app(json!({
            "source": { "image": "x" },
            "runtime": { "processes": { "a": { "port": 1 }, "b": { "port": 2 } } }
        }));
        let o = owner(&two_ports);
        assert!(matches!(
            deployments(&two_ports, &Platform::default(), &o),
            Err(BuildError::MultiplePorts(_))
        ));

        let git = app(json!({
            "source": { "git": { "repo": "https://github.com/acme/api" } },
            "runtime": { "processes": { "web": { "port": 8080 } } }
        }));
        assert_eq!(
            deployments(&git, &Platform::default(), &o)
                .expect_err("a git source without a built image must wait for the build")
                .reason(),
            "AwaitingBuild"
        );

        let bad_size = app(json!({
            "source": { "image": "x" },
            "runtime": { "processes": { "web": { "size": "galactic" } } }
        }));
        assert!(matches!(
            deployments(&bad_size, &Platform::default(), &o),
            Err(BuildError::UnknownSize { .. })
        ));
    }

    #[test]
    fn service_and_route_follow_the_web_process() {
        let a = web_app();
        let o = owner(&a);
        let svc = service(&a, &o).expect("ok").expect("service");
        let port = &svc.spec.as_ref().and_then(|s| s.ports.as_ref()).expect("ports")[0];
        assert_eq!(
            (port.port, port.target_port.clone()),
            (80, Some(IntOrString::Int(3000)))
        );

        let p = platform();
        assert_eq!(
            hostnames(&a, &p),
            vec![
                "api.acme.com".to_owned(),
                "api-shop-prod.apps.example.com".to_owned()
            ]
        );
        assert_eq!(url(&a, &p).as_deref(), Some("https://api.acme.com"));
        let route = http_route(&a, &p, &o).expect("ok").expect("route");
        assert_eq!(route["spec"]["parentRefs"][0]["namespace"], "kuben-system");
        assert_eq!(route["spec"]["rules"][0]["backendRefs"][0]["port"], 80);
        assert!(
            http_route(&a, &Platform::default(), &o).expect("ok").is_none(),
            "no gateway → no route"
        );
    }

    #[test]
    fn volumes_are_retained_mounted_and_force_recreate() {
        let a = app(json!({
            "source": { "image": "postgres:17-alpine" },
            "runtime": { "processes": { "db": { "port": 5432, "protocol": "tcp" } } },
            "volumes": [{ "name": "data", "mountPath": "/var/lib/postgresql/data", "size": "5Gi" }]
        }));
        let pvcs = persistent_volume_claims(&a);
        assert_eq!(pvcs.len(), 1);
        let pvc = &pvcs[0];
        assert_eq!(pvc.metadata.name.as_deref(), Some("api-data"));
        assert!(pvc.metadata.owner_references.is_none(), "data outlives the app");
        assert_eq!(pvc.metadata.annotations.as_ref().expect("ann")[RETAIN], "true");
        let req = pvc
            .spec
            .as_ref()
            .and_then(|s| s.resources.as_ref())
            .and_then(|r| r.requests.as_ref())
            .expect("requests");
        assert_eq!(req["storage"], q("5Gi"));

        let deps = deployments(&a, &Platform::default(), &owner(&a)).expect("deployments");
        let spec = deps[0].spec.as_ref().expect("spec");
        assert_eq!(
            spec.strategy.as_ref().and_then(|s| s.type_.as_deref()),
            Some("Recreate")
        );
        let pod = spec.template.spec.as_ref().expect("pod");
        assert_eq!(
            pod.volumes.as_ref().expect("volumes")[0]
                .persistent_volume_claim
                .as_ref()
                .map(|c| c.claim_name.as_str()),
            Some("api-data")
        );
        assert_eq!(
            pod.containers[0].volume_mounts.as_ref().expect("mounts")[0].mount_path,
            "/var/lib/postgresql/data"
        );
        assert!(
            pod.containers[0].liveness_probe.is_none(),
            "no health path → no liveness"
        );

        let scaled = app(json!({
            "source": { "image": "x" },
            "runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 1, "max": 3 } } } },
            "volumes": [{ "name": "data", "mountPath": "/data" }]
        }));
        assert_eq!(validate(&scaled), Err(BuildError::VolumeNeedsSingleReplica));
    }

    #[test]
    fn tcp_processes_get_a_real_port_and_no_route() {
        let a = app(json!({
            "source": { "image": "redis:7-alpine" },
            "runtime": { "processes": { "redis": { "port": 6379, "protocol": "tcp" } } }
        }));
        let o = owner(&a);
        let svc = service(&a, &o).expect("ok").expect("service");
        let port = &svc.spec.as_ref().and_then(|s| s.ports.as_ref()).expect("ports")[0];
        assert_eq!(
            (port.port, port.name.as_deref(), port.app_protocol.as_deref()),
            (6379, Some("tcp"), None)
        );
        assert!(
            http_route(&a, &platform(), &o).expect("ok").is_none(),
            "tcp is never public"
        );
    }

    #[test]
    fn scheduled_processes_become_cron_jobs() {
        let a = app(json!({
            "source": { "image": "ghcr.io/acme/api:1.2.3" },
            "runtime": { "processes": {
                "web": { "port": 3000 },
                "report": { "command": ["bin/report"], "schedule": "0 3 * * *", "timeZone": "Europe/Berlin" }
            } }
        }));
        let o = owner(&a);
        let deps = deployments(&a, &Platform::default(), &o).expect("deployments");
        assert_eq!(deps.len(), 1, "scheduled processes get no Deployment");
        let crons = cron_jobs(&a, &Platform::default(), &o).expect("crons");
        let spec = crons[0].spec.as_ref().expect("spec");
        assert_eq!(crons[0].metadata.name.as_deref(), Some("api-report"));
        assert_eq!(spec.schedule, "0 3 * * *");
        assert_eq!(spec.time_zone.as_deref(), Some("Europe/Berlin"));
        assert_eq!(spec.concurrency_policy.as_deref(), Some("Forbid"));
        let pod = spec
            .job_template
            .spec
            .as_ref()
            .and_then(|j| j.template.spec.as_ref())
            .expect("pod");
        assert_eq!(pod.restart_policy.as_deref(), Some("Never"));
        assert!(pod.containers[0].readiness_probe.is_none());

        let mut live = crons[0].clone();
        live.metadata.uid = Some("cron-uid".into());
        let job = job_from_cron(&live, "api-report-manual-1").expect("job");
        assert_eq!(
            job.metadata.owner_references.as_ref().expect("owner")[0].kind,
            "CronJob"
        );
        assert!(job.spec.is_some());

        assert!(valid_schedule("*/5 * * * *") && valid_schedule("@daily"));
        assert!(!valid_schedule("every day") && !valid_schedule("* * * *") && !valid_schedule("@often"));
        let bad = app(json!({
            "source": { "image": "x" },
            "runtime": { "processes": { "job": { "port": 80, "schedule": "@hourly" } } }
        }));
        assert_eq!(validate(&bad), Err(BuildError::ScheduledWithPort("job".into())));
    }

    #[test]
    fn tls_routes_attach_to_host_listeners() {
        let a = web_app();
        let route = http_route(&a, &platform(), &owner(&a))
            .expect("ok")
            .expect("route");
        let refs = route["spec"]["parentRefs"].as_array().expect("refs");
        let mut sections: Vec<String> = refs
            .iter()
            .map(|r| r["sectionName"].as_str().unwrap_or_default().to_owned())
            .collect();
        sections.sort();
        let mut expected = vec![
            super::super::gateway::host_listener_name("api.acme.com"),
            super::super::gateway::host_listener_name("api-shop-prod.apps.example.com"),
        ];
        expected.sort();
        assert_eq!(sections, expected);
    }

    #[test]
    fn platform_defaults_include_size_presets() {
        let p = Platform::default();
        assert!(p.size("small").is_some());
        assert!(p.gateway.is_none() && !p.tls);
    }

    #[test]
    fn domains_carry_their_tls_mode_into_the_route_and_a_grant() {
        let a = app(json!({
            "source": { "image": "ghcr.io/acme/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
            "runtime": { "processes": { "web": { "port": 3000 } } },
            "domains": [
                { "host": "Shop.acme.com" },
                { "host": "legacy.acme.com", "tls": "none" },
                { "host": "own.acme.com", "tls": "acme-cert" }
            ]
        }));
        validate(&a).expect("valid");
        let p = platform();
        let o = owner(&a);
        let claims = domain_claims(&a, &p);
        assert_eq!(
            claims
                .iter()
                .map(|c| (c.host.as_str(), c.tls.as_str()))
                .collect::<Vec<_>>(),
            [
                ("shop.acme.com", "auto"),
                ("legacy.acme.com", "none"),
                ("own.acme.com", "acme-cert"),
                ("api-shop-prod.apps.example.com", "auto"),
            ]
        );
        assert_eq!(url(&a, &p).as_deref(), Some("https://shop.acme.com"));

        let route = http_route(&a, &p, &o).expect("ok").expect("route");
        let annotation = route["metadata"]["annotations"][DOMAINS_ANNOTATION]
            .as_str()
            .expect("annotation");
        assert_eq!(
            serde_json::from_str::<Vec<DomainClaim>>(annotation).expect("claims"),
            claims
        );
        let sections: BTreeSet<&str> = route["spec"]["parentRefs"]
            .as_array()
            .expect("parents")
            .iter()
            .filter_map(|r| r["sectionName"].as_str())
            .collect();
        assert!(sections.contains(super::super::gateway::plain_listener_name("legacy.acme.com").as_str()));
        assert!(sections.contains(super::super::gateway::host_listener_name("own.acme.com").as_str()));
        assert_eq!(sections.len(), 4, "{sections:?}");

        let grant = reference_grant(&a, &p, &o).expect("ok").expect("grant");
        assert_eq!(grant["kind"], "ReferenceGrant");
        assert_eq!(grant["metadata"]["name"], "api-tls");
        assert_eq!(grant["spec"]["from"][0]["namespace"], "kuben-system");
        assert_eq!(
            grant["spec"]["to"],
            json!([{ "group": "", "kind": "Secret", "name": "acme-cert" }])
        );

        // TLS unavailable: no grant, no listener names, plain URLs.
        let mut off = platform();
        off.tls = false;
        assert!(reference_grant(&a, &off, &o).expect("ok").is_none());
        let plain = http_route(&a, &off, &o).expect("ok").expect("route");
        assert!(plain["spec"]["parentRefs"][0].get("sectionName").is_none());
        assert_eq!(url(&a, &off).as_deref(), Some("http://shop.acme.com"));
    }

    #[test]
    fn a_domain_tls_that_is_no_secret_name_is_refused() {
        assert_eq!(TlsMode::parse(""), Some(TlsMode::Auto));
        assert_eq!(TlsMode::parse("none"), Some(TlsMode::Plain));
        assert_eq!(
            TlsMode::parse("certs.acme"),
            Some(TlsMode::Secret("certs.acme".into()))
        );
        for bad in ["Bad_Name", "-x", "a..b", "UPPER"] {
            assert_eq!(TlsMode::parse(bad), None, "{bad}");
        }
        let a = app(json!({
            "source": { "image": "nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
            "runtime": { "processes": { "web": { "port": 80 } } },
            "domains": [{ "host": "a.acme.com", "tls": "Bad_Name" }]
        }));
        assert_eq!(validate(&a).map_err(|e| e.reason()), Err("InvalidDomainTls"),);
    }
}

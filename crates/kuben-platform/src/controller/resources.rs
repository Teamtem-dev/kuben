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

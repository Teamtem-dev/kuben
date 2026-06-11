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

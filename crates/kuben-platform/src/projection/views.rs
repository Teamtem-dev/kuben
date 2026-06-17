//! View types. Small, `PartialEq` (so unchanged objects produce no delta) and
//! directly serializable for the UI. Every view carries its `org` so streams
//! can be filtered per tenant.

use k8s_openapi::api::core::v1::Pod;
use kube::ResourceExt;
use kuben_crd::{App, Condition, Environment, EnvironmentType, Project, condition::READY, labels};
use serde::Serialize;

use crate::controller::resources::namespace_name;

fn ready_condition(conditions: &[Condition]) -> Option<&Condition> {
    conditions.iter().find(|c| c.type_ == READY)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum PodPhase {
    Pending,
    Running,
    Succeeded,
    Failed,
    Unknown,
}

impl From<Option<&str>> for PodPhase {
    fn from(s: Option<&str>) -> Self {
        match s {
            Some("Pending") => Self::Pending,
            Some("Running") => Self::Running,
            Some("Succeeded") => Self::Succeeded,
            Some("Failed") => Self::Failed,
            _ => Self::Unknown,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct PodView {
    /// `namespace/name`.
    pub key: String,
    pub namespace: String,
    pub name: String,
    pub org: Option<String>,
    pub app: Option<String>,
    pub process: Option<String>,
    pub phase: PodPhase,
    pub ready: bool,
    pub restarts: i32,
    /// `CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`, ...
    pub reason: Option<String>,
    pub node: Option<String>,
    pub started_at: Option<String>,
}

impl From<&Pod> for PodView {
    fn from(pod: &Pod) -> Self {
        let namespace = pod.namespace().unwrap_or_default();
        let name = pod.name_any();
        let labels = pod.labels();
        let status = pod.status.as_ref();
        let containers = status.and_then(|s| s.container_statuses.as_ref());
        let ready = containers.is_some_and(|cs| !cs.is_empty() && cs.iter().all(|c| c.ready));
        let restarts = containers.map_or(0, |cs| cs.iter().map(|c| c.restart_count).sum());
        let reason = containers
            .and_then(|cs| {
                cs.iter().find_map(|c| {
                    let state = c.state.as_ref()?;
                    state
                        .waiting
                        .as_ref()
                        .and_then(|w| w.reason.clone())
                        .or_else(|| state.terminated.as_ref().and_then(|t| t.reason.clone()))
                })
            })
            .or_else(|| status.and_then(|s| s.reason.clone()));
        Self {
            key: format!("{namespace}/{name}"),
            namespace,
            name,
            org: labels.get(labels::ORG).cloned(),
            app: labels.get(labels::APP).cloned(),
            process: labels.get(labels::PROCESS).cloned(),
            phase: PodPhase::from(status.and_then(|s| s.phase.as_deref())),
            ready,
            restarts,
            reason,
            node: pod.spec.as_ref().and_then(|s| s.node_name.clone()),
            started_at: status
                .and_then(|s| s.start_time.as_ref())
                .map(|t| t.0.to_string()),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ProjectView {
    pub name: String,
    pub uid: Option<String>,
    pub display_name: String,
    pub description: Option<String>,
    pub org: Option<String>,
    pub environments: u32,
    pub ready: bool,
    pub deleting: bool,
    pub created_at: Option<String>,
}

impl From<&Project> for ProjectView {
    fn from(p: &Project) -> Self {
        let status = p.status.as_ref();
        Self {
            name: p.name_any(),
            uid: p.uid(),
            display_name: p.spec.display_name.clone(),
            description: p.spec.description.clone(),
            org: p.labels().get(labels::ORG).cloned(),
            environments: status.map_or(0, |s| s.environments),
            ready: status
                .and_then(|s| ready_condition(&s.conditions))
                .is_some_and(|c| c.status == "True"),
            deleting: p.metadata.deletion_timestamp.is_some(),
            created_at: p.creation_timestamp().map(|t| t.0.to_string()),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct EnvironmentView {
    pub name: String,
    pub uid: Option<String>,
    pub project: String,
    pub org: Option<String>,
    /// `standard | production | preview`.
    pub env_type: &'static str,
    pub namespace: String,
    /// `Pending | Ready | Terminating | Degraded`.
    pub phase: Option<String>,
    pub ready: bool,
    pub message: Option<String>,
    pub deleting: bool,
    pub deletion_scheduled_at: Option<String>,
    pub created_at: Option<String>,
}

impl From<&Environment> for EnvironmentView {
    fn from(e: &Environment) -> Self {
        let status = e.status.as_ref();
        let ready = status.and_then(|s| ready_condition(&s.conditions));
        Self {
            name: e.name_any(),
            uid: e.uid(),
            project: e.spec.project.clone(),
            org: e.labels().get(labels::ORG).cloned(),
            env_type: match e.spec.type_ {
                EnvironmentType::Standard => "standard",
                EnvironmentType::Production => "production",
                EnvironmentType::Preview => "preview",
            },
            namespace: status
                .and_then(|s| s.namespace.clone())
                .unwrap_or_else(|| namespace_name(&e.name_any())),
            phase: status.and_then(|s| s.phase.clone()),
            ready: ready.is_some_and(|c| c.status == "True"),
            message: ready.and_then(|c| c.message.clone()),
            deleting: e.metadata.deletion_timestamp.is_some(),
            deletion_scheduled_at: status.and_then(|s| s.deletion_scheduled_at.clone()),
            created_at: e.creation_timestamp().map(|t| t.0.to_string()),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ProcessView {
    pub name: String,
    pub command: Vec<String>,
    pub port: Option<u16>,
    pub size: String,
    pub min_replicas: u32,
    pub max_replicas: u32,
    /// Cron expression for scheduled processes.
    pub schedule: Option<String>,
    /// `http` or `tcp`.
    pub protocol: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]

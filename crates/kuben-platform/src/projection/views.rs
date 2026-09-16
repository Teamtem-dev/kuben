//! View types. Small, `PartialEq` (so unchanged objects produce no delta) and
//! directly serializable for the UI. Every view carries its `org` so streams
//! can be filtered per tenant.

use k8s_openapi::api::core::v1::Pod;
use kube::{ResourceExt, api::DynamicObject};
use kuben_crd::{App, Condition, Environment, EnvironmentType, Project, condition::READY, labels};
use serde::Serialize;
use serde_json::Value;

use crate::controller::resources::{DOMAINS_ANNOTATION, DomainClaim, namespace_name};

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
pub struct VolumeView {
    pub name: String,
    pub mount_path: String,
    pub size: String,
}

/// Environment variable *reference*. Values never travel through the shared
/// stream; the app detail endpoint returns them after an authorization check.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct EnvVarRef {
    pub name: String,
    /// `secret/key` for secret-backed variables.
    pub secret: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct AppView {
    /// `namespace/name`.
    pub key: String,
    pub namespace: String,
    pub name: String,
    pub uid: Option<String>,
    pub org: Option<String>,
    pub project: Option<String>,
    pub environment: Option<String>,
    pub image: Option<String>,
    pub git_repo: Option<String>,
    pub url: Option<String>,
    pub ready: bool,
    pub reason: Option<String>,
    pub message: Option<String>,
    pub processes: Vec<ProcessView>,
    pub env: Vec<EnvVarRef>,
    pub domains: Vec<String>,
    pub volumes: Vec<VolumeView>,
    pub created_at: Option<String>,
}

impl From<&App> for AppView {
    fn from(a: &App) -> Self {
        let status = a.status.as_ref();
        let ready = status.and_then(|s| ready_condition(&s.conditions));
        let image = a.spec.source.image.clone();
        let git_repo = a.spec.source.git.as_ref().map(|g| g.repo.clone());
        let namespace = a.namespace().unwrap_or_default();
        let name = a.name_any();
        let l = a.labels();
        Self {
            key: format!("{namespace}/{name}"),
            namespace,
            name,
            uid: a.uid(),
            org: l.get(labels::ORG).cloned(),
            project: l.get(labels::PROJECT).cloned(),
            environment: l.get(labels::ENVIRONMENT).cloned(),
            image,
            git_repo,
            url: status.and_then(|s| s.url.clone()),
            ready: ready.is_some_and(|c| c.status == "True"),
            reason: ready.and_then(|c| c.reason.clone()),
            message: ready.and_then(|c| c.message.clone()),
            processes: a
                .spec
                .runtime
                .processes
                .iter()
                .map(|(n, p)| ProcessView {
                    name: n.clone(),
                    command: p.command.clone(),
                    port: p.port,
                    size: p.size.clone(),
                    min_replicas: p.replicas.min,
                    max_replicas: p.replicas.max,
                    schedule: p.schedule.clone(),
                    protocol: if p.protocol.is_http() { "http" } else { "tcp" }.into(),
                })
                .collect(),
            env: a
                .spec
                .env
                .iter()
                .map(|e| EnvVarRef {
                    name: e.name.clone(),
                    secret: e
                        .from_secret
                        .as_ref()
                        .or(e.from_service.as_ref())
                        .map(|r| format!("{}/{}", r.name, r.key)),
                })
                .collect(),
            domains: a.spec.domains.iter().map(|d| d.host.clone()).collect(),
            volumes: a
                .spec
                .volumes
                .iter()
                .map(|v| VolumeView {
                    name: v.name.clone(),
                    mount_path: v.mount_path.clone(),
                    size: v.size.clone(),
                })
                .collect(),
            created_at: a.creation_timestamp().map(|t| t.0.to_string()),
        }
    }
}

/// An app's `HTTPRoute` (M2.3): its domains and whether a Gateway took it.
/// Routes of both delivery paths (App controller and agent) look alike.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RouteView {
    /// `namespace/name`; the name is the app's.
    pub key: String,
    pub namespace: String,
    pub name: String,
    /// `namespace/name` of each parent Gateway.
    pub gateways: Vec<String>,
    /// Listener names the route attaches to (`sectionName`), if any.
    pub sections: Vec<String>,
    pub domains: Vec<DomainClaim>,
    /// A Gateway accepted the route and resolved its references; `None`
    /// until a gateway controller answered.
    pub accepted: Option<bool>,
    pub message: Option<String>,
}

/// The `Accepted` and `ResolvedRefs` verdicts of every parent, folded.
fn route_status(data: &Value) -> (Option<bool>, Option<String>) {
    let parents = data
        .pointer("/status/parents")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let mut accepted = None;
    for parent in &parents {
        let Some(conditions) = parent["conditions"].as_array() else {
            continue;
        };
        for c in conditions {
            if c["type"] != "Accepted" && c["type"] != "ResolvedRefs" {
                continue;
            }
            if c["status"] == "True" {
                accepted.get_or_insert(true);
            } else {
                let message = c["message"]
                    .as_str()
                    .filter(|m| !m.is_empty())
                    .or_else(|| c["reason"].as_str())
                    .map(str::to_owned);
                return (Some(false), message);
            }
        }
    }
    (accepted, None)
}

impl From<&DynamicObject> for RouteView {
    fn from(route: &DynamicObject) -> Self {
        let namespace = route.namespace().unwrap_or_default();
        let name = route.name_any();
        let spec = &route.data["spec"];
        let parents = spec["parentRefs"].as_array().cloned().unwrap_or_default();
        let gateways = parents
            .iter()
            .filter_map(|p| {
                let gw = p["name"].as_str()?;
                let ns = p["namespace"].as_str().unwrap_or(&namespace);
                Some(format!("{ns}/{gw}"))
            })
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        let sections = parents
            .iter()
            .filter_map(|p| p["sectionName"].as_str().map(str::to_owned))
            .collect();
        let domains = route
            .annotations()
            .get(DOMAINS_ANNOTATION)
            .and_then(|a| serde_json::from_str::<Vec<DomainClaim>>(a).ok())
            .unwrap_or_else(|| {
                // Routes written before M2.3 carry only their hostnames.
                spec["hostnames"]
                    .as_array()
                    .map(|hosts| {
                        hosts
                            .iter()
                            .filter_map(Value::as_str)
                            .map(|h| DomainClaim {
                                host: h.to_owned(),
                                tls: "auto".into(),
                            })
                            .collect()
                    })
                    .unwrap_or_default()
            });
        let (accepted, message) = route_status(&route.data);
        Self {
            key: format!("{namespace}/{name}"),
            namespace,
            name,
            gateways,
            sections,
            domains,
            accepted,
            message,
        }
    }
}

/// A cert-manager `Certificate` Kuben's gateway ordered (`kuben-tls-*`).
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct CertificateView {
    /// `namespace/name`.
    pub key: String,
    pub ready: bool,
    pub message: Option<String>,
    /// `status.notAfter`.
    pub not_after: Option<String>,
}

impl From<&DynamicObject> for CertificateView {
    fn from(cert: &DynamicObject) -> Self {
        let (ready, message) = crate::discovery::condition(&cert.data, "Ready").unwrap_or((false, None));
        Self {
            key: format!("{}/{}", cert.namespace().unwrap_or_default(), cert.name_any()),
            ready,
            message: message.filter(|m| !ready && !m.is_empty()),
            not_after: cert
                .data
                .pointer("/status/notAfter")
                .and_then(Value::as_str)
                .map(str::to_owned),
        }
    }
}

/// How one hostname of an app is reached.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct HostExposure {
    pub host: String,
    /// `auto`, `none` or `secret`.
    pub tls: &'static str,
    /// For `auto` hosts with a certificate of their own: whether it is issued.
    pub certificate_ready: Option<bool>,
    pub certificate_message: Option<String>,
}

/// Whether an app can be reached through the gateway (I09: separate from
/// being applied and healthy).
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ExposureView {
    pub accepted: Option<bool>,
    pub message: Option<String>,
    pub hosts: Vec<HostExposure>,
}

#[cfg(test)]
mod tests {
    use k8s_openapi::api::core::v1::{ContainerState, ContainerStateWaiting, ContainerStatus, PodStatus};
    use kube::api::ObjectMeta;

    use super::*;

    #[test]
    fn pod_view_extracts_reason_and_readiness() {
        let mut labels = std::collections::BTreeMap::new();
        labels.insert(labels::APP.to_string(), "api".to_string());
        labels.insert(labels::PROCESS.to_string(), "web".to_string());
        labels.insert(labels::ORG.to_string(), "org-1".to_string());
        let pod = Pod {
            metadata: ObjectMeta {
                name: Some("api-web-abc".into()),
                namespace: Some("kb-shop-prod".into()),
                labels: Some(labels),
                ..ObjectMeta::default()
            },
            status: Some(PodStatus {
                phase: Some("Running".into()),
                container_statuses: Some(vec![ContainerStatus {
                    name: "web".into(),
                    ready: false,
                    restart_count: 4,
                    state: Some(ContainerState {
                        waiting: Some(ContainerStateWaiting {
                            reason: Some("CrashLoopBackOff".into()),
                            ..ContainerStateWaiting::default()
                        }),
                        ..ContainerState::default()
                    }),
                    ..ContainerStatus::default()
                }]),
                ..PodStatus::default()
            }),
            ..Pod::default()
        };
        let v = PodView::from(&pod);
        assert_eq!(v.key, "kb-shop-prod/api-web-abc");
        assert_eq!(v.app.as_deref(), Some("api"));
        assert_eq!(v.process.as_deref(), Some("web"));
        assert_eq!(v.org.as_deref(), Some("org-1"));
        assert_eq!(v.phase, PodPhase::Running);
        assert!(!v.ready);
        assert_eq!(v.restarts, 4);
        assert_eq!(v.reason.as_deref(), Some("CrashLoopBackOff"));
    }

    #[test]
    fn app_view_never_carries_env_values() {
        let spec = serde_json::from_value(serde_json::json!({
            "source": { "image": "nginx:1.27" },
            "runtime": { "processes": { "web": { "port": 80 } } },
            "env": [
                { "name": "MODE", "value": "s3cr3t-plain-value" },
                { "name": "TOKEN", "fromSecret": { "name": "api", "key": "token" } }
            ]
        }))
        .expect("spec");
        let mut app = App::new("web", spec);
        app.metadata.namespace = Some("kb-shop-prod".into());
        let v = AppView::from(&app);
        assert_eq!(v.key, "kb-shop-prod/web");
        assert_eq!(v.image.as_deref(), Some("nginx:1.27"));
        assert_eq!(
            v.env[0],
            EnvVarRef {
                name: "MODE".into(),
                secret: None
            }
        );
        assert_eq!(v.env[1].secret.as_deref(), Some("api/token"));
        let json = serde_json::to_string(&v).expect("json");
        assert!(
            !json.contains("s3cr3t-plain-value"),
            "plain values must not be serialized: {json}"
        );
    }
}

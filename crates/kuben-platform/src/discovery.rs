//! Cluster capability discovery (M2.1, ADR-031).
//!
//! "Installed" is not "compatible": features are gated on what the cluster
//! reports, not on what `KubenConfig` asks for. [`discover`] reads the
//! Gateway API CRDs (bundle version, channel, served kinds), the
//! GatewayClasses and whether their controller accepted them, cert-manager and
//! the readiness of its ClusterIssuers, and the Metrics API. A probe that
//! fails (RBAC, timeout) makes its facts unknown, never absent.
//!
//! [`run`] repeats discovery, publishes the facts to the controllers and the
//! materializer through a watch channel, and records them in SQL for every
//! organization's `primary` cluster, where the API and Doctor read them.

use std::{collections::BTreeSet, sync::Arc, time::Duration};

use futures::Stream;
use k8s_openapi::{
    api::{apps::v1::DaemonSet, core::v1::Node, storage::v1::StorageClass},
    apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
};
use kube::{
    Api, Client,
    api::{ApiResource, DynamicObject, GroupVersionKind, ListParams},
};
use kuben_store::Store;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::{sync::watch, time::Instant};
use tokio_util::sync::CancellationToken;

use crate::registry::{self, ClusterRegistry};

pub const GATEWAY_GROUP: &str = "gateway.networking.k8s.io";
pub const CERT_MANAGER_GROUP: &str = "cert-manager.io";
pub const METRICS_GROUP: &str = "metrics.k8s.io";
const GATEWAY_CRD: &str = "gateways.gateway.networking.k8s.io";
const BUNDLE_VERSION: &str = "gateway.networking.k8s.io/bundle-version";
const CHANNEL: &str = "gateway.networking.k8s.io/channel";
/// How often discovery runs.
const INTERVAL: Duration = Duration::from_mins(1);
/// Until the first discovery succeeds, retry sooner.
const FIRST_RETRY: Duration = Duration::from_secs(5);
/// Unchanged facts are still written this often, so organizations and
/// clusters created meanwhile get them.
const REWRITE: Duration = Duration::from_mins(10);
/// Bound on each call to the API server.
const PROBE_TIMEOUT: Duration = Duration::from_secs(10);

/// What a cluster can do, as last discovered.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ClusterFacts {
    /// The Gateway API CRDs, when installed.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gateway_api: Option<GatewayApi>,
    #[serde(default)]
    pub gateway_classes: Vec<Readiness>,
    #[serde(default)]
    pub cert_manager: bool,
    #[serde(default)]
    pub cluster_issuers: Vec<Readiness>,
    #[serde(default)]
    pub metrics_api: bool,
    /// The API server's version, e.g. `v1.36.4+k3s1`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kubernetes_version: Option<String>,
    /// The StorageClass volumes get when they name none.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub default_storage_class: Option<String>,
    /// What enforces NetworkPolicies (e.g. `cilium`, `k3s`), when found.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub network_policy: Option<String>,
    /// Probes that failed; what they would have found is unknown.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub unknown: Vec<String>,
}

/// The installed Gateway API.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GatewayApi {
    /// `gateway.networking.k8s.io/bundle-version` of the Gateway CRD.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bundle_version: Option<String>,
    /// `standard` or `experimental`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub channel: Option<String>,
    /// Served kinds, e.g. `GRPCRoute`, `HTTPRoute`, `ReferenceGrant`.
    #[serde(default)]
    pub kinds: BTreeSet<String>,
}

/// A GatewayClass or ClusterIssuer and whether its controller made it usable.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Readiness {
    pub name: String,
    /// A GatewayClass's `spec.controllerName`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub controller: Option<String>,
    /// `Accepted` for a GatewayClass, `Ready` for a ClusterIssuer.
    pub ready: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

/// Whether a named dependency can be used.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Availability {
    Ready,
    NotReady(String),
    Missing,
    /// The probe failed: neither usable nor proven missing.
    Unknown,
}

impl Availability {
    /// Usable, or not proven otherwise.
    #[must_use]
    pub const fn usable_or_unknown(&self) -> bool {
        matches!(self, Self::Ready | Self::Unknown)
    }
}

const PROBE_GATEWAY_CLASSES: &str = "gatewayClasses";
const PROBE_CLUSTER_ISSUERS: &str = "clusterIssuers";
const PROBE_API_GROUPS: &str = "apiGroups";

impl ClusterFacts {
    fn find(list: &[Readiness], name: &str, probe: &str, unknown: &[String]) -> Availability {
        match list.iter().find(|r| r.name == name) {
            Some(r) if r.ready => Availability::Ready,
            Some(r) => Availability::NotReady(r.message.clone().unwrap_or_default()),
            None if unknown.iter().any(|u| u == probe || u == PROBE_API_GROUPS) => Availability::Unknown,
            None => Availability::Missing,
        }
    }

    /// The ClusterIssuer `name`.
    #[must_use]
    pub fn issuer(&self, name: &str) -> Availability {
        Self::find(&self.cluster_issuers, name, PROBE_CLUSTER_ISSUERS, &self.unknown)
    }

    /// The GatewayClass `name`.
    #[must_use]
    pub fn gateway_class(&self, name: &str) -> Availability {
        Self::find(&self.gateway_classes, name, PROBE_GATEWAY_CLASSES, &self.unknown)
    }

    /// The Gateway API serves `kind` (e.g. `GRPCRoute`).
    #[must_use]
    pub fn serves(&self, kind: &str) -> bool {
        self.gateway_api.as_ref().is_some_and(|g| g.kinds.contains(kind))
    }

    /// One line for logs and Doctor.
    #[must_use]
    pub fn summary(&self) -> String {
        let gateway = self.gateway_api.as_ref().map_or_else(
            || "no Gateway API".to_owned(),
            |g| {
                format!(
                    "Gateway API {} ({})",
                    g.bundle_version.as_deref().unwrap_or("unknown version"),
                    g.channel.as_deref().unwrap_or("unknown channel")
                )
            },
        );
        let ready = |list: &[Readiness]| {
            let names: Vec<&str> = list.iter().filter(|r| r.ready).map(|r| r.name.as_str()).collect();
            if names.is_empty() {
                "none ready".to_owned()
            } else {
                names.join(", ")
            }
        };
        format!(
            "{gateway}; gateway classes: {}; cert-manager: {}; cluster issuers: {}; metrics: {}{}",
            ready(&self.gateway_classes),
            if self.cert_manager { "yes" } else { "no" },
            ready(&self.cluster_issuers),
            if self.metrics_api { "yes" } else { "no" },
            if self.unknown.is_empty() {
                String::new()
            } else {
                format!("; unknown: {}", self.unknown.join(", "))
            }
        )
    }
}

/// The condition `type_` of a status, as `(status is True, message)`.
pub(crate) fn condition(obj: &Value, type_: &str) -> Option<(bool, Option<String>)> {
    obj.pointer("/status/conditions")?
        .as_array()?
        .iter()
        .find(|c| c["type"] == type_)
        .map(|c| (c["status"] == "True", c["message"].as_str().map(str::to_owned)))
}

fn readiness(obj: &DynamicObject, condition_type: &str) -> Readiness {
    let (ready, message) = condition(&obj.data, condition_type).unwrap_or((false, None));
    Readiness {
        name: obj.metadata.name.clone().unwrap_or_default(),
        controller: obj
            .data
            .pointer("/spec/controllerName")
            .and_then(Value::as_str)
            .map(str::to_owned),
        ready,
        message: message.filter(|m| !ready && !m.is_empty()),
    }
}

async fn timed<T>(f: impl Future<Output = kube::Result<T>>) -> Result<T, String> {
    match tokio::time::timeout(PROBE_TIMEOUT, f).await {
        Ok(Ok(v)) => Ok(v),
        Ok(Err(e)) => Err(e.to_string()),
        Err(_) => Err("timed out".into()),
    }
}

async fn list_ready(
    client: &Client,
    group: &str,
    version: &str,
    kind: &str,
    plural: &str,
    condition_type: &str,
) -> Result<Vec<Readiness>, String> {
    let gvk = GroupVersionKind::gvk(group, version, kind);
    let api =
        Api::<DynamicObject>::all_with(client.clone(), &ApiResource::from_gvk_with_plural(&gvk, plural));
    let list = timed(api.list(&ListParams::default())).await?;
    let mut out: Vec<Readiness> = list.items.iter().map(|o| readiness(o, condition_type)).collect();
    out.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(out)
}

async fn gateway_api(client: &Client, versions: &[String], unknown: &mut Vec<String>) -> GatewayApi {
    let mut api = GatewayApi::default();
    match timed(Api::<CustomResourceDefinition>::all(client.clone()).get_opt(GATEWAY_CRD)).await {
        Ok(Some(crd)) => {
            let annotations = crd.metadata.annotations.unwrap_or_default();
            api.bundle_version = annotations.get(BUNDLE_VERSION).cloned();
            api.channel = annotations.get(CHANNEL).cloned();
        }
        Ok(None) => {}
        Err(e) => {
            tracing::debug!(error = %e, "cannot read the Gateway CRD");
            unknown.push("gatewayApiVersion".into());
        }
    }
    for version in versions {
        match timed(client.list_api_group_resources(version)).await {
            Ok(list) => api.kinds.extend(
                list.resources
                    .into_iter()
                    .filter(|r| !r.name.contains('/'))
                    .map(|r| r.kind),
            ),
            Err(e) => {
                tracing::debug!(error = %e, %version, "cannot list Gateway API resources");
                unknown.push(format!("resources:{version}"));
            }
        }
    }
    api
}

const DEFAULT_CLASS: &str = "storageclass.kubernetes.io/is-default-class";
/// DaemonSets of CNIs that enforce NetworkPolicies, and the name reported.
const POLICY_ENFORCERS: [(&str, &str); 6] = [
    ("cilium", "cilium"),
    ("calico-node", "calico"),
    ("canal", "canal"),
    ("antrea-agent", "antrea"),
    ("kube-router", "kube-router"),
    ("weave-net", "weave"),
];

/// The default StorageClass among `classes`.
fn default_class(classes: &[StorageClass]) -> Option<String> {
    classes
        .iter()
        .find(|c| {
            c.metadata
                .annotations
                .as_ref()
                .and_then(|a| a.get(DEFAULT_CLASS))
                .is_some_and(|v| v == "true")
        })
        .and_then(|c| c.metadata.name.clone())
}

/// What enforces NetworkPolicies: a known CNI's DaemonSet, or k3s, whose
/// embedded controller does unless it was turned off.
fn policy_enforcer(daemonsets: &[String], kubelet_versions: &[String]) -> Option<String> {
    POLICY_ENFORCERS
        .iter()
        .find(|(daemonset, _)| daemonsets.iter().any(|d| d == daemonset))
        .map(|(_, name)| (*name).to_owned())
        .or_else(|| {
            kubelet_versions
                .iter()
                .any(|v| v.contains("+k3s"))
                .then(|| "k3s".to_owned())
        })
}

/// The workload facts: version, storage and NetworkPolicy enforcement.
async fn workload_facts(client: &Client, facts: &mut ClusterFacts) {
    match timed(client.apiserver_version()).await {
        Ok(v) => facts.kubernetes_version = Some(v.git_version),
        Err(_) => facts.unknown.push("version".into()),
    }
    match timed(Api::<StorageClass>::all(client.clone()).list(&ListParams::default())).await {
        Ok(list) => facts.default_storage_class = default_class(&list.items),
        Err(_) => facts.unknown.push("storageClasses".into()),
    }
    let daemonsets = timed(Api::<DaemonSet>::all(client.clone()).list_metadata(&ListParams::default())).await;
    let nodes = timed(Api::<Node>::all(client.clone()).list(&ListParams::default())).await;
    match (daemonsets, nodes) {
        (Ok(daemonsets), Ok(nodes)) => {
            let names: Vec<String> = daemonsets
                .items
                .into_iter()
                .filter_map(|d| d.metadata.name)
                .collect();
            let versions: Vec<String> = nodes
                .items
                .into_iter()
                .filter_map(|n| n.status?.node_info.map(|i| i.kubelet_version))
                .collect();
            facts.network_policy = policy_enforcer(&names, &versions);
        }
        _ => facts.unknown.push("networkPolicy".into()),
    }
}

/// Discover what `client`'s cluster can do. Never fails: failed probes are
/// listed in [`ClusterFacts::unknown`].
pub async fn discover(client: &Client) -> ClusterFacts {
    let mut facts = ClusterFacts::default();
    let groups = match timed(client.list_api_groups()).await {
        Ok(groups) => groups.groups,
        Err(e) => {
            tracing::warn!(error = %e, "cannot list API groups; cluster capabilities are unknown");
            facts.unknown.push(PROBE_API_GROUPS.into());
            return facts;
        }
    };
    let served = |name: &str| -> Option<Vec<String>> {
        groups
            .iter()
            .find(|g| g.name == name)
            .map(|g| g.versions.iter().map(|v| v.group_version.clone()).collect())
    };
    if let Some(versions) = served(GATEWAY_GROUP) {
        facts.gateway_api = Some(gateway_api(client, &versions, &mut facts.unknown).await);
        match list_ready(
            client,
            GATEWAY_GROUP,
            "v1",
            "GatewayClass",
            "gatewayclasses",
            "Accepted",
        )
        .await
        {
            Ok(classes) => facts.gateway_classes = classes,
            Err(e) => {
                tracing::debug!(error = %e, "cannot list GatewayClasses");
                facts.unknown.push(PROBE_GATEWAY_CLASSES.into());
            }
        }
    }
    if served(CERT_MANAGER_GROUP).is_some() {
        facts.cert_manager = true;
        match list_ready(
            client,
            CERT_MANAGER_GROUP,
            "v1",
            "ClusterIssuer",
            "clusterissuers",
            "Ready",
        )
        .await
        {
            Ok(issuers) => facts.cluster_issuers = issuers,
            Err(e) => {
                tracing::debug!(error = %e, "cannot list ClusterIssuers");
                facts.unknown.push(PROBE_CLUSTER_ISSUERS.into());
            }
        }
    }
    facts.metrics_api = served(METRICS_GROUP).is_some();
    workload_facts(client, &mut facts).await;
    facts
}

/// The latest facts: `None` until the first discovery.
pub type Facts = watch::Receiver<Option<Arc<ClusterFacts>>>;
pub type FactsSender = watch::Sender<Option<Arc<ClusterFacts>>>;

/// A channel with no facts yet.
#[must_use]
pub fn channel() -> (FactsSender, Facts) {
    watch::channel(None)
}

/// The facts now, if any were discovered.
#[must_use]
pub fn current(facts: &Facts) -> Option<Arc<ClusterFacts>> {
    facts.borrow().clone()
}

/// A stream that yields once for every change of `facts`, for controllers
/// that reconcile everything when the cluster's capabilities change.
pub fn changes(facts: Facts) -> impl Stream<Item = ()> + Send + 'static {
    futures::stream::unfold(facts, |mut rx| async move {
        rx.changed().await.ok().map(|()| ((), rx))
    })
}

/// Discover the primary cluster's capabilities until cancelled: publish them
/// on `tx` when they change and record them in SQL.
pub async fn run(
    registry: ClusterRegistry,
    store: Store,
    tx: Arc<FactsSender>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let client = registry.primary();
    let mut written: Option<(Arc<ClusterFacts>, Instant)> = None;
    loop {
        let facts = Arc::new(discover(&client).await);
        let changed = tx.borrow().as_deref() != Some(facts.as_ref());
        if changed {
            tracing::info!(capabilities = %facts.summary(), "cluster capabilities");
            tx.send_replace(Some(facts.clone()));
        }
        let due = written
            .as_ref()
            .is_none_or(|(last, at)| last != &facts || at.elapsed() >= REWRITE);
        if due {
            let json = serde_json::to_value(facts.as_ref())?;
            match store
                .record_capabilities_everywhere(
                    registry::ClusterId::PRIMARY,
                    &json,
                    kuben_core::time::now_ms(),
                )
                .await
            {
                Ok(_) => written = Some((facts.clone(), Instant::now())),
                Err(e) => tracing::warn!(error = %e, "cannot record the cluster capabilities"),
            }
        }
        let pause = if facts.unknown.iter().any(|u| u == PROBE_API_GROUPS) {
            FIRST_RETRY
        } else {
            INTERVAL
        };
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep(pause) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    fn object(name: &str, data: &Value) -> DynamicObject {
        let mut obj: DynamicObject = serde_json::from_value(json!({
            "apiVersion": "x/v1", "kind": "X", "metadata": { "name": name }
        }))
        .expect("object");
        obj.data = data.clone();
        obj
    }

    #[test]
    fn readiness_reads_the_condition_and_keeps_only_useful_messages() {
        let accepted = object(
            "traefik",
            &json!({
                "spec": { "controllerName": "traefik.io/gateway-controller" },
                "status": { "conditions": [{ "type": "Accepted", "status": "True", "message": "ok" }] }
            }),
        );
        assert_eq!(
            readiness(&accepted, "Accepted"),
            Readiness {
                name: "traefik".into(),
                controller: Some("traefik.io/gateway-controller".into()),
                ready: true,
                message: None,
            }
        );
        let failing = object(
            "letsencrypt",
            &json!({ "status": { "conditions": [{ "type": "Ready", "status": "False", "message": "account not registered" }] } }),
        );
        let r = readiness(&failing, "Ready");
        assert!(!r.ready);
        assert_eq!(r.message.as_deref(), Some("account not registered"));
        assert!(
            !readiness(&object("new", &json!({})), "Ready").ready,
            "no status yet"
        );
    }

    #[test]
    fn availability_separates_missing_from_unknown() {
        let facts = ClusterFacts {
            cert_manager: true,
            cluster_issuers: vec![
                Readiness {
                    name: "le".into(),
                    ready: true,
                    ..Readiness::default()
                },
                Readiness {
                    name: "broken".into(),
                    message: Some("no account".into()),
                    ..Readiness::default()
                },
            ],
            ..ClusterFacts::default()
        };
        assert_eq!(facts.issuer("le"), Availability::Ready);
        assert_eq!(
            facts.issuer("broken"),
            Availability::NotReady("no account".into())
        );
        assert_eq!(facts.issuer("other"), Availability::Missing);
        assert_eq!(facts.gateway_class("traefik"), Availability::Missing);

        let blind = ClusterFacts {
            cert_manager: true,
            unknown: vec![PROBE_CLUSTER_ISSUERS.into()],
            ..ClusterFacts::default()
        };
        assert_eq!(blind.issuer("le"), Availability::Unknown);
        assert!(blind.issuer("le").usable_or_unknown());
        let offline = ClusterFacts {
            unknown: vec![PROBE_API_GROUPS.into()],
            ..ClusterFacts::default()
        };
        assert_eq!(offline.gateway_class("traefik"), Availability::Unknown);
        assert!(!Availability::Missing.usable_or_unknown());
    }

    #[test]
    fn facts_roundtrip_compactly_and_summarize() {
        let facts = ClusterFacts {
            gateway_api: Some(GatewayApi {
                bundle_version: Some("v1.5.1".into()),
                channel: Some("standard".into()),
                kinds: ["GRPCRoute", "HTTPRoute"].map(str::to_owned).into(),
            }),
            gateway_classes: vec![Readiness {
                name: "traefik".into(),
                ready: true,
                ..Readiness::default()
            }],
            metrics_api: true,
            ..ClusterFacts::default()
        };
        let json = serde_json::to_value(&facts).expect("json");
        assert!(json.get("unknown").is_none());
        assert_eq!(json["gatewayApi"]["channel"], "standard");
        assert_eq!(serde_json::from_value::<ClusterFacts>(json).expect("back"), facts);
        assert!(facts.serves("GRPCRoute") && !facts.serves("TLSRoute"));
        assert_eq!(
            facts.summary(),
            "Gateway API v1.5.1 (standard); gateway classes: traefik; cert-manager: no; \
             cluster issuers: none ready; metrics: yes"
        );
        assert_eq!(
            serde_json::from_value::<ClusterFacts>(json!({})).expect("empty"),
            ClusterFacts::default(),
            "older or partial records still read"
        );
    }

    #[test]
    fn storage_and_policy_enforcement_are_read_from_the_cluster() {
        let class = |name: &str, default: bool| {
            let mut c = StorageClass::default();
            c.metadata.name = Some(name.into());
            if default {
                c.metadata.annotations = Some([(DEFAULT_CLASS.to_owned(), "true".to_owned())].into());
            }
            c
        };
        assert_eq!(
            default_class(&[class("slow", false), class("local-path", true)]),
            Some("local-path".into())
        );
        assert_eq!(default_class(&[class("slow", false)]), None);

        let names = |n: &[&str]| n.iter().map(|s| (*s).to_owned()).collect::<Vec<_>>();
        assert_eq!(
            policy_enforcer(&names(&["kube-proxy", "cilium"]), &[]),
            Some("cilium".into())
        );
        assert_eq!(
            policy_enforcer(&names(&["svclb-traefik"]), &names(&["v1.36.4+k3s1"])),
            Some("k3s".into())
        );
        assert_eq!(
            policy_enforcer(&names(&["kube-flannel-ds"]), &names(&["v1.34.1"])),
            None
        );
    }
}

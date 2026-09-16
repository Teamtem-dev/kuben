//! The renderer (WS-G, M1.9c, ADR-026): an App's spec and the cluster's
//! capabilities become a RenderPlan — the normalized resources the App
//! controller would build for it, their inventory and digests — frozen once
//! per deployment run and never rendered again for that generation.
//!
//! It reuses the App controller's builder (`controller::app::build`, which
//! calls `controller::resources`) unchanged, so a plan holds exactly what the
//! controller applies today. Normalization:
//!
//! * owner references are left out: the object that owns the children (the
//!   `ApplicationRuntime`, M1.9) exists only at apply time, and the agent
//!   adds it there;
//! * `status` and null fields are dropped;
//! * objects are ordered by kind (volumes, workloads, autoscalers, cron jobs,
//!   service, route) and then by name;
//! * the JSON is canonical — object keys sorted, no whitespace — so the
//!   digest depends on content only, never on field order or on how
//!   `serde_json` was built.
//!
//! A plan larger than the envelope bound ([`MAX_ENVELOPE_BYTES`]) is refused.

use std::fmt::Write as _;

use k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference;
use kube::ResourceExt;
use kuben_crd::{App, InventoryItem, MAX_ENVELOPE_BYTES, PlanEnvelope, SizePreset};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest as _, Sha256};

use crate::controller::{
    app::build,
    resources::{BuildError, GatewayRef, Platform},
};

/// Recorded with every plan; a renderer change that alters output is a new
/// version, and older plans are never rendered again with it.
pub const RENDERER_VERSION: &str = "kuben-renderer/1";

/// The cluster facts a plan depends on (ADR-026's capability snapshot): the
/// size presets, domains, gateway and TLS settings of `KubenConfig`, narrowed
/// to what the cluster can do (`Platform::gated`).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Capabilities {
    pub sizes: Vec<SizePreset>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub base_domain: Option<String>,
    /// `namespace/name` of the Gateway routes attach to.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gateway: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cluster_issuer: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub wildcard_tls_secret: Option<String>,
    /// A ClusterIssuer is configured but was not Ready when the plan was
    /// rendered (M2.1): routes are served over plain HTTP.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub tls_unavailable: bool,
}

impl Capabilities {
    /// The snapshot of the platform settings the App controller uses.
    #[must_use]
    pub fn of(platform: &Platform) -> Self {
        Self {
            sizes: platform.sizes.clone(),
            base_domain: platform.base_domain.clone(),
            gateway: platform
                .gateway
                .as_ref()
                .map(|g| format!("{}/{}", g.namespace, g.name)),
            cluster_issuer: platform.cluster_issuer.clone(),
            wildcard_tls_secret: platform.wildcard_tls_secret.clone(),
            tls_unavailable: platform.cluster_issuer.is_some() && !platform.tls,
        }
    }

    /// The platform settings the builder reads, from the snapshot alone.
    #[must_use]
    pub fn platform(&self) -> Platform {
        Platform {
            sizes: self.sizes.clone(),
            base_domain: self.base_domain.clone(),
            gateway: self.gateway.as_deref().and_then(|g| {
                let (namespace, name) = g.split_once('/')?;
                (!namespace.is_empty() && !name.is_empty()).then(|| GatewayRef {
                    namespace: namespace.into(),
                    name: name.into(),
                })
            }),
            // Rendering never needs it: the gateway controller owns the Gateway.
            gateway_class: None,
            tls: self.cluster_issuer.is_some() && !self.tls_unavailable,
            cluster_issuer: self.cluster_issuer.clone(),
            wildcard_tls_secret: self.wildcard_tls_secret.clone(),
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum RenderPlanError {
    /// The App's spec cannot be built (the controller would report it too).
    #[error(transparent)]
    Build(#[from] BuildError),
    #[error("the rendered resources take {bytes} bytes, more than the {max}-byte envelope")]
    TooLarge { bytes: usize, max: usize },
    #[error("a rendered object has no {0}")]
    Incomplete(&'static str),
    #[error("a rendered object does not serialize: {0}")]
    Serialize(#[from] serde_json::Error),
}

/// A rendered, normalized plan, ready to freeze (`Tenant::freeze_render_plan`)
/// and to carry in an `ApplicationRuntime` envelope.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Plan {
    pub capabilities: Capabilities,
    /// Canonical JSON of the resources: an array of objects, in apply order.
    resources: String,
    pub inventory: Vec<InventoryItem>,
    /// `sha256:` of the canonical resources.
    pub digest: String,
    /// `sha256:` of the canonical inventory (kinds and names only).
    pub inventory_hash: String,
}

impl Plan {
    #[must_use]
    pub const fn renderer_version(&self) -> &'static str {
        RENDERER_VERSION
    }

    /// The resources as canonical JSON.
    #[must_use]
    pub fn resources_json(&self) -> &str {
        &self.resources
    }

    /// The resources as a JSON value (for the SQL row).
    pub fn resources(&self) -> Result<Value, serde_json::Error> {
        serde_json::from_str(&self.resources)
    }

    /// The capability snapshot as a JSON value (for the SQL row).
    pub fn capabilities_json(&self) -> Result<Value, serde_json::Error> {
        serde_json::to_value(&self.capabilities)
    }

    /// The envelope an `ApplicationRuntime` carries for this plan, frozen as
    /// `plan_id`.
    #[must_use]
    pub fn envelope(&self, plan_id: &str) -> PlanEnvelope {
        PlanEnvelope {
            id: plan_id.to_owned(),
            renderer_version: RENDERER_VERSION.to_owned(),
            digest: self.digest.clone(),
            resources: self.resources.clone(),
        }
    }
}

/// Render `app` against `capabilities`.
pub fn render(app: &App, capabilities: &Capabilities) -> Result<Plan, RenderPlanError> {
    let desired = build(app, &capabilities.platform(), &unowned(app))?;
    let mut objects = Vec::new();
    for v in &desired.volumes {
        objects.push(serde_json::to_value(v)?);
    }
    for d in &desired.deployments {
        objects.push(serde_json::to_value(d)?);
    }
    for h in &desired.autoscalers {
        objects.push(serde_json::to_value(h)?);
    }
    for c in &desired.cron_jobs {
        objects.push(serde_json::to_value(c)?);
    }
    if let Some(s) = &desired.service {
        objects.push(serde_json::to_value(s)?);
    }
    if let Some(r) = &desired.route {
        objects.push(r.clone());
    }
    for object in &mut objects {
        normalize(object);
    }
    let mut entries = objects
        .into_iter()
        .map(|o| Ok((item(&o)?, o)))
        .collect::<Result<Vec<_>, RenderPlanError>>()?;
    entries.sort_by(|(a, _), (b, _)| (rank(&a.kind), &a.name).cmp(&(rank(&b.kind), &b.name)));
    let (inventory, objects): (Vec<_>, Vec<_>) = entries.into_iter().unzip();

    let resources = canonical(&Value::Array(objects));
    if resources.len() > MAX_ENVELOPE_BYTES {
        return Err(RenderPlanError::TooLarge {
            bytes: resources.len(),
            max: MAX_ENVELOPE_BYTES,
        });
    }
    let inventory_hash = sha256(&canonical(&serde_json::to_value(&inventory)?));
    Ok(Plan {
        capabilities: capabilities.clone(),
        digest: sha256(&resources),
        resources,
        inventory,
        inventory_hash,
    })
}

/// The builder wants an owner; the plan leaves it out ([`normalize`]).
fn unowned(app: &App) -> OwnerReference {
    OwnerReference {
        api_version: "kuben.dev/v1alpha1".into(),
        kind: "App".into(),
        name: app.name_any(),
        uid: String::new(),
        controller: Some(true),
        block_owner_deletion: Some(true),
    }
}

fn normalize(object: &mut Value) {
    if let Some(map) = object.as_object_mut() {
        map.remove("status");
        if let Some(meta) = map.get_mut("metadata").and_then(Value::as_object_mut) {
            for server_side in [
                "ownerReferences",
                "uid",
                "resourceVersion",
                "generation",
                "managedFields",
                "creationTimestamp",
            ] {
                meta.remove(server_side);
            }
        }
    }
    drop_nulls(object);
}

fn drop_nulls(value: &mut Value) {
    match value {
        Value::Object(map) => {
            map.retain(|_, v| !v.is_null());
            map.values_mut().for_each(drop_nulls);
        }
        Value::Array(items) => items.iter_mut().for_each(drop_nulls),
        _ => {}
    }
}

fn item(object: &Value) -> Result<InventoryItem, RenderPlanError> {
    let field = |path: &[&str], what: &'static str| {
        path.iter()
            .try_fold(object, |v, key| v.get(key))
            .and_then(Value::as_str)
            .map(str::to_owned)
            .ok_or(RenderPlanError::Incomplete(what))
    };
    Ok(InventoryItem {
        api_version: field(&["apiVersion"], "apiVersion")?,
        kind: field(&["kind"], "kind")?,
        name: field(&["metadata", "name"], "metadata.name")?,
        uid: None,
    })
}

/// Apply order: storage before the workloads that mount it, workloads
/// before what points at them.
fn rank(kind: &str) -> u8 {
    match kind {
        "PersistentVolumeClaim" => 0,
        "Deployment" => 1,
        "HorizontalPodAutoscaler" => 2,
        "CronJob" => 3,
        "Service" => 4,
        "HTTPRoute" => 5,
        _ => 9,
    }
}

/// JSON with object keys sorted and no whitespace.
pub(crate) fn canonical(value: &Value) -> String {
    fn write(value: &Value, out: &mut String) {
        match value {
            Value::Object(map) => {
                let mut keys: Vec<&String> = map.keys().collect();
                keys.sort();
                out.push('{');
                for (i, key) in keys.into_iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    out.push_str(&Value::String(key.clone()).to_string());
                    out.push(':');
                    write(&map[key], out);
                }
                out.push('}');
            }
            Value::Array(items) => {
                out.push('[');
                for (i, v) in items.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write(v, out);
                }
                out.push(']');
            }
            scalar => out.push_str(&scalar.to_string()),
        }
    }
    let mut out = String::new();
    write(value, &mut out);
    out
}

pub(crate) fn sha256(text: &str) -> String {
    Sha256::digest(text.as_bytes())
        .iter()
        .fold(String::from("sha256:"), |mut s, b| {
            let _ = write!(s, "{b:02x}");
            s
        })
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use kuben_crd::{AppSpec, KubenConfigSpec, labels};
    use serde_json::json;

    use super::*;

    fn app(spec: Value) -> App {
        let mut a = App::new("api", serde_json::from_value::<AppSpec>(spec).expect("app spec"));
        a.metadata.namespace = Some("kb-shop-prod".into());
        a.metadata.labels = Some(BTreeMap::from([
            (labels::PROJECT.to_owned(), "shop".to_owned()),
            (labels::ENVIRONMENT.to_owned(), "shop-prod".to_owned()),
        ]));
        a
    }

    fn web_app() -> App {
        app(json!({
            "source": { "image": "ghcr.io/acme/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
            "runtime": { "processes": {
                "web": { "port": 3000, "size": "small", "replicas": { "min": 2, "max": 4 } },
                "worker": { "command": ["bin/worker"], "size": "nano" }
            }, "healthCheck": { "path": "/healthz" } },
            "env": [
                { "name": "LOG_LEVEL", "value": "info" },
                { "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } }
            ],
            "domains": [{ "host": "api.acme.com" }]
        }))
    }

    fn capabilities() -> Capabilities {
        let spec = serde_json::from_value::<KubenConfigSpec>(json!({
            "baseDomain": "apps.example.com",
            "gateway": "kuben-system/kuben",
            "clusterIssuer": "letsencrypt"
        }))
        .expect("config");
        Capabilities::of(&Platform::from_spec(Some(&spec)))
    }

    fn pretty(plan: &Plan) -> String {
        let value: Value = serde_json::from_str(plan.resources_json()).expect("canonical JSON");
        serde_json::to_string_pretty(&value).expect("pretty")
    }

    #[test]
    fn the_capability_snapshot_round_trips() {
        let caps = capabilities();
        let platform = caps.platform();
        assert_eq!(Capabilities::of(&platform), caps);
        assert!(platform.tls, "a cluster issuer means TLS routes");
        let json = serde_json::to_value(&caps).expect("json");
        assert_eq!(serde_json::from_value::<Capabilities>(json).expect("back"), caps);
    }

    #[test]
    fn an_issuer_that_is_not_ready_blocks_only_tls() {
        use crate::discovery::{ClusterFacts, Readiness};

        let spec = serde_json::from_value::<KubenConfigSpec>(json!({
            "baseDomain": "apps.example.com",
            "gateway": "kuben-system/kuben",
            "clusterIssuer": "letsencrypt"
        }))
        .expect("config");
        let issuer = |ready| ClusterFacts {
            cert_manager: true,
            cluster_issuers: vec![Readiness {
                name: "letsencrypt".into(),
                ready,
                ..Readiness::default()
            }],
            ..ClusterFacts::default()
        };

        let blocked = Capabilities::of(&Platform::from_spec(Some(&spec)).gated(Some(&issuer(false))));
        assert!(blocked.tls_unavailable && !blocked.platform().tls);
        let json = serde_json::to_value(&blocked).expect("json");
        assert_eq!(json["tlsUnavailable"], true);
        assert_eq!(
            serde_json::from_value::<Capabilities>(json).expect("back"),
            blocked
        );
        let plan = render(&web_app(), &blocked).expect("render");
        let route = plan
            .resources()
            .expect("resources")
            .as_array()
            .and_then(|r| r.iter().find(|o| o["kind"] == "HTTPRoute").cloned())
            .expect("the route is still rendered");
        assert!(
            route["spec"]["parentRefs"][0].get("sectionName").is_none(),
            "plain HTTP: the route attaches to the gateway, not to an HTTPS listener"
        );

        let ready = Capabilities::of(&Platform::from_spec(Some(&spec)).gated(Some(&issuer(true))));
        assert_eq!(ready, capabilities(), "a Ready issuer changes nothing");
        assert_eq!(
            Capabilities::of(&Platform::from_spec(Some(&spec)).gated(None)),
            capabilities(),
            "unknown facts keep the configured intent"
        );
        assert!(
            serde_json::to_value(&ready)
                .expect("json")
                .get("tlsUnavailable")
                .is_none(),
            "older snapshots stay byte-identical"
        );
    }

    #[test]
    fn the_plan_holds_what_the_app_controller_builds_without_owners() {
        let plan = render(&web_app(), &capabilities()).expect("render");
        let inventory: Vec<(&str, &str)> = plan
            .inventory
            .iter()
            .map(|i| (i.kind.as_str(), i.name.as_str()))
            .collect();
        assert_eq!(
            inventory,
            [
                ("Deployment", "api-web"),
                ("Deployment", "api-worker"),
                ("HorizontalPodAutoscaler", "api-web"),
                ("Service", "api"),
                ("HTTPRoute", "api"),
            ]
        );
        assert!(!plan.resources_json().contains("ownerReferences"));
        assert!(!plan.resources_json().contains("\"status\""));
        assert_eq!(plan.renderer_version(), RENDERER_VERSION);
        let envelope = plan.envelope("plan-1");
        assert_eq!(envelope.digest, plan.digest);
        assert_eq!(envelope.resources, plan.resources_json());
    }

    #[test]
    fn rendering_is_deterministic_and_content_addressed() {
        let first = render(&web_app(), &capabilities()).expect("render");
        assert_eq!(render(&web_app(), &capabilities()).expect("again"), first);
        assert_eq!(first.digest, sha256(first.resources_json()));

        let mut changed = web_app();
        changed.spec.env[0].value = Some("debug".into());
        let other = render(&changed, &capabilities()).expect("render");
        assert_ne!(other.digest, first.digest, "another env value");
        assert_eq!(other.inventory_hash, first.inventory_hash, "the same objects");

        let mut caps = capabilities();
        caps.gateway = None;
        let unrouted = render(&web_app(), &caps).expect("render");
        assert_ne!(unrouted.digest, first.digest, "another capability snapshot");
        assert_ne!(
            unrouted.inventory_hash, first.inventory_hash,
            "no route without a gateway"
        );
    }

    #[test]
    fn canonical_json_sorts_keys_at_every_level() {
        let value = json!({ "b": [ { "y": 1, "x": null } ], "a": "\"q\"" });
        assert_eq!(canonical(&value), r#"{"a":"\"q\"","b":[{"x":null,"y":1}]}"#);
    }

    #[test]
    fn a_plan_larger_than_the_envelope_is_refused() {
        let mut big = web_app();
        big.spec.env[0].value = Some("x".repeat(MAX_ENVELOPE_BYTES));
        match render(&big, &capabilities()) {
            Err(RenderPlanError::TooLarge { bytes, max }) => assert!(bytes > max),
            other => panic!("expected TooLarge, got {other:?}"),
        }
    }

    #[test]
    fn a_spec_the_controller_refuses_is_refused() {
        let broken = app(json!({
            "source": { "image": "ghcr.io/acme/api:1.2.3" },
            "runtime": { "processes": { "web": { "port": 3000, "size": "no-such-size" } } }
        }));
        assert!(matches!(
            render(&broken, &capabilities()),
            Err(RenderPlanError::Build(_))
        ));
    }

    #[test]
    fn golden_web_app() {
        let plan = render(&web_app(), &capabilities()).expect("render");
        insta::assert_snapshot!(pretty(&plan));
    }

    #[test]
    fn golden_cron_job_and_volume() {
        let cron = app(json!({
            "source": { "image": "busybox@sha256:2222222222222222222222222222222222222222222222222222222222222222" },
            "runtime": { "processes": {
                "tick": { "command": ["sh", "-c", "echo tick"], "schedule": "*/30 * * * *", "size": "nano" }
            } }
        }));
        let store = app(json!({
            "source": { "image": "nginx@sha256:3333333333333333333333333333333333333333333333333333333333333333" },
            "runtime": { "processes": { "web": { "port": 8080, "size": "small" } } },
            "volumes": [{ "name": "data", "mountPath": "/data", "size": "5Gi" }]
        }));
        // The CRD defaults: the size presets, no gateway and no TLS.
        let caps = Capabilities::of(&Platform::default());
        insta::assert_snapshot!("cron_job", pretty(&render(&cron, &caps).expect("cron")));
        insta::assert_snapshot!("volume", pretty(&render(&store, &caps).expect("volume")));
    }
}

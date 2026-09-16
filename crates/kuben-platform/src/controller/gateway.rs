//! Gateway listeners and automatic HTTPS (scenario 9, M2.2).
//!
//! Kuben writes the listeners of one Gateway, `KubenConfig.spec.gateway`, and
//! only when that Gateway is Kuben's: the one it creates itself when
//! `spec.gatewayClassName` is set, or an existing one an operator dedicated to
//! it with the label `kuben.dev/gateway-owner=kuben`. Any other Gateway is left
//! untouched and reported on `KubenConfig`'s `Gateway` condition: Kuben never
//! overwrites a resource it does not own (ADR-031).
//!
//! With TLS (a Ready ClusterIssuer, `Platform::gated`):
//!
//! * `http` (:80) — only the platform redirect route and cert-manager's
//!   HTTP-01 solver routes attach here (`allowedRoutes: Same`), so tenants
//!   can never serve plain HTTP;
//! * `https` (:443, `*.<baseDomain>`) — only when `wildcardTlsSecret` is set;
//!   covers every generated `<app>-<env>.<baseDomain>` host;
//! * `h-<hash>` (:443) — one listener per other hostname, admitting routes
//!   **only from the namespace that owns the host** (first come, first
//!   served), so one tenant cannot hijack another tenant's domain.
//!
//! Without TLS, `http` admits the routes of Kuben's namespaces and is the only
//! listener: plain HTTP keeps working while TLS is unavailable.
//!
//! The `cert-manager.io/cluster-issuer` annotation makes cert-manager's
//! gateway-shim issue one certificate per listener into `kuben-tls-<hash>`.

use std::{collections::BTreeMap, sync::Arc, time::Duration};

use kube::{
    Api, ResourceExt,
    api::{ApiResource, DeleteParams, DynamicObject, GroupVersionKind, Patch, PatchParams},
};
use kuben_crd::{Condition, FIELD_MANAGER, KubenConfig, labels};
use serde_json::{Value, json};
use tokio::sync::broadcast::error::RecvError;
use tokio_util::sync::CancellationToken;

use super::{
    Ctx, condition, is_not_found,
    resources::{DomainClaim, GatewayRef, Platform, TlsMode},
};
use crate::{
    discovery::{self, Availability, ClusterFacts},
    projection::{Projections, RouteView},
};

pub const HTTP_LISTENER: &str = "http";
pub const WILDCARD_LISTENER: &str = "https";
pub const REDIRECT_ROUTE: &str = "kuben-https-redirect";
pub const ISSUER_ANNOTATION: &str = "cert-manager.io/cluster-issuer";
/// Condition type on `KubenConfig`: can apps be exposed through the Gateway?
pub const GATEWAY_CONDITION: &str = "Gateway";
/// Gateway API allows at most 64 listeners; leave headroom for hand-made ones.
pub const MAX_LISTENERS: usize = 60;
/// How often the Gateway's readiness is read again.
const STATUS_REFRESH: Duration = Duration::from_secs(30);

/// 64-bit FNV-1a. Stable forever (unlike `DefaultHasher`), because listener
/// and secret names derived from it must not change between releases.
fn fnv1a(s: &str) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in s.bytes() {
        h ^= u64::from(b);
        h = h.wrapping_mul(0x0100_0000_01b3);
    }
    h
}

fn host_hash(host: &str) -> String {
    format!("{:012x}", fnv1a(host) >> 16)
}

#[must_use]
pub fn host_listener_name(host: &str) -> String {
    format!("h-{}", host_hash(host))
}

/// Prefix of the certificate Secrets (and cert-manager Certificates) Kuben's
/// gateway orders.
pub const TLS_SECRET_PREFIX: &str = "kuben-tls-";

#[must_use]
pub fn host_secret_name(host: &str) -> String {
    format!("{TLS_SECRET_PREFIX}{}", host_hash(host))
}

/// The plain-HTTP listener of a `tls: none` host.
#[must_use]
pub fn plain_listener_name(host: &str) -> String {
    format!("p-{}", host_hash(host))
}

/// `*.base` matches exactly one additional DNS label.
fn covered_by_wildcard(host: &str, platform: &Platform) -> bool {
    match (&platform.base_domain, &platform.wildcard_tls_secret) {
        (Some(base), Some(_)) => host
            .strip_suffix(base.as_str())
            .and_then(|rest| rest.strip_suffix('.'))
            .is_some_and(|label| !label.is_empty() && !label.contains('.')),
        _ => false,
    }
}

/// Listener (`sectionName`) an app route must attach to for `host`.
#[must_use]
pub fn section_for_host(host: &str, platform: &Platform) -> String {
    if covered_by_wildcard(host, platform) {
        WILDCARD_LISTENER.to_owned()
    } else {
        host_listener_name(host)
    }
}

/// Listener an app route attaches to for `claim` while TLS is on: its own
/// certificate always gets a listener of its own.
#[must_use]
pub fn section_for(claim: &DomainClaim, platform: &Platform) -> String {
    match claim.mode() {
        TlsMode::Auto => section_for_host(&claim.host, platform),
        TlsMode::Secret(_) => host_listener_name(&claim.host),
        TlsMode::Plain => plain_listener_name(&claim.host),
    }
}

/// Desired listeners plus what could not be served.
#[derive(Debug, Default, PartialEq)]
pub struct ListenerPlan {
    pub listeners: Vec<Value>,
    /// `host` claimed by more than one namespace (the first one keeps it).
    pub conflicts: Vec<String>,
    /// Hosts without a listener because [`MAX_LISTENERS`] was reached.
    pub skipped: Vec<String>,
}

fn tls(secret: &str) -> Value {
    json!({ "mode": "Terminate", "certificateRefs": [{ "kind": "Secret", "name": secret }] })
}

/// Routes from the namespaces Kuben manages.
fn kuben_namespaces() -> Value {
    json!({ "namespaces": { "from": "Selector", "selector": {
        "matchLabels": { labels::MANAGED_BY: labels::MANAGER }
    } } })
}

/// `hosts`: every routed domain with its namespace. Deterministic: the same
/// input always yields the same listeners in the same order.
#[must_use]
pub fn plan_listeners(hosts: &[(DomainClaim, String)], platform: &Platform) -> ListenerPlan {
    let http_routes = if platform.tls {
        json!({ "namespaces": { "from": "Same" } })
    } else {
        kuben_namespaces()
    };
    let mut listeners = vec![json!({
        "name": HTTP_LISTENER,
        "protocol": "HTTP",
        "port": 80,
        "allowedRoutes": http_routes,
    })];
    if !platform.tls {
        return ListenerPlan {
            listeners,
            ..ListenerPlan::default()
        };
    }

    let mut sorted: Vec<&(DomainClaim, String)> = hosts.iter().collect();
    sorted.sort();
    let mut owners: BTreeMap<&str, (&str, TlsMode)> = BTreeMap::new();
    let mut conflicts = Vec::new();
    for (claim, ns) in sorted {
        match owners.get(claim.host.as_str()) {
            Some((existing, _)) if *existing != ns.as_str() => {
                conflicts.push(format!(
                    "{}: owned by {existing}, also requested by {ns}",
                    claim.host
                ));
            }
            Some(_) => {}
            None => {
                owners.insert(&claim.host, (ns, claim.mode()));
            }
        }
    }
    if let (Some(base), Some(secret)) = (&platform.base_domain, &platform.wildcard_tls_secret) {
        listeners.push(json!({
            "name": WILDCARD_LISTENER,
            "protocol": "HTTPS",
            "port": 443,
            "hostname": format!("*.{base}"),
            "tls": tls(secret),
            "allowedRoutes": kuben_namespaces(),
        }));
    }
    let mut skipped = Vec::new();
    for (host, (ns, mode)) in owners {
        if mode == TlsMode::Auto && covered_by_wildcard(host, platform) {
            continue;
        }
        if listeners.len() >= MAX_LISTENERS {
            skipped.push(host.to_owned());
            continue;
        }
        let only_owner = json!({ "namespaces": { "from": "Selector", "selector": {
            "matchLabels": { "kubernetes.io/metadata.name": ns }
        } } });
        listeners.push(match mode {
            TlsMode::Plain => json!({
                "name": plain_listener_name(host),
                "protocol": "HTTP",
                "port": 80,
                "hostname": host,
                "allowedRoutes": only_owner,
            }),
            TlsMode::Auto | TlsMode::Secret(_) => {
                let certificate = match &mode {
                    // The app's own certificate, readable through its ReferenceGrant.
                    TlsMode::Secret(secret) => json!({
                        "mode": "Terminate",
                        "certificateRefs": [{ "kind": "Secret", "name": secret, "namespace": ns }]
                    }),
                    _ => tls(&host_secret_name(host)),
                };
                json!({
                    "name": host_listener_name(host),
                    "protocol": "HTTPS",
                    "port": 443,
                    "hostname": host,
                    "tls": certificate,
                    "allowedRoutes": only_owner,
                })
            }
        });
    }
    ListenerPlan {
        listeners,
        conflicts,
        skipped,
    }
}

/// Server-side-apply body for the Gateway: Kuben's field manager owns only the
/// listeners it lists, the issuer annotation while TLS is on, and — on the
/// Gateway it creates — the class and the ownership labels.
#[must_use]
pub fn gateway_patch(gateway: &GatewayRef, platform: &Platform, plan: &ListenerPlan) -> Value {
    let mut body = json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "Gateway",
        "metadata": { "name": gateway.name, "namespace": gateway.namespace },
        "spec": { "listeners": plan.listeners },
    });
    if let Some(class) = &platform.gateway_class {
        body["metadata"]["labels"] = json!({
            labels::MANAGED_BY: labels::MANAGER,
            labels::GATEWAY_OWNER: labels::MANAGER,
        });
        body["spec"]["gatewayClassName"] = json!(class);
    }
    if let (true, Some(issuer)) = (platform.tls, &platform.cluster_issuer) {
        body["metadata"]["annotations"] = json!({ ISSUER_ANNOTATION: issuer });
    }
    body
}

/// Platform route answering every plain-HTTP request with a 301 to HTTPS.
/// cert-manager's solver routes carry an exact hostname and therefore win.
#[must_use]
pub fn redirect_route(gateway: &GatewayRef) -> Value {
    json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "HTTPRoute",
        "metadata": {
            "name": REDIRECT_ROUTE,
            "namespace": gateway.namespace,
            "labels": { labels::MANAGED_BY: labels::MANAGER },
        },
        "spec": {
            "parentRefs": [{ "name": gateway.name, "namespace": gateway.namespace, "sectionName": HTTP_LISTENER }],
            "rules": [{ "filters": [{
                "type": "RequestRedirect",
                "requestRedirect": { "scheme": "https", "statusCode": 301 }
            }] }],
        },
    })
}

/// Who a Gateway belongs to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Ownership {
    /// Created by Kuben or dedicated to it with [`labels::GATEWAY_OWNER`].
    Kuben,
    /// Someone else's: never written.
    Foreign,
    Absent,
}

#[must_use]
pub fn ownership(live: Option<&DynamicObject>) -> Ownership {
    match live {
        None => Ownership::Absent,
        Some(g) if g.labels().get(labels::GATEWAY_OWNER).map(String::as_str) == Some(labels::MANAGER) => {
            Ownership::Kuben
        }
        Some(_) => Ownership::Foreign,
    }
}

/// The `Gateway` condition: `(ok, reason, message)`.
type Verdict = (bool, &'static str, String);

/// Why Kuben does not write `gateway`, if it must not.
#[must_use]
pub fn refusal(gateway: &GatewayRef, platform: &Platform, owner: Ownership) -> Option<Verdict> {
    let name = format!("{}/{}", gateway.namespace, gateway.name);
    match (owner, &platform.gateway_class) {
        (Ownership::Foreign, _) => Some((
            false,
            "GatewayNotOwned",
            format!(
                "Gateway {name} is not dedicated to Kuben; label it {}={} or name another Gateway",
                labels::GATEWAY_OWNER,
                labels::MANAGER
            ),
        )),
        (Ownership::Absent, None) => Some((
            false,
            "GatewayMissing",
            format!(
                "Gateway {name} does not exist; create it, or set spec.gatewayClassName for Kuben to create it"
            ),
        )),
        _ => None,
    }
}

/// Readiness of a Gateway Kuben writes, from its `Programmed` condition.
#[must_use]
pub fn readiness(live: &DynamicObject, platform: &Platform, facts: Option<&ClusterFacts>) -> Verdict {
    match discovery::condition(&live.data, "Programmed") {
        Some((true, _)) => {
            let addresses: Vec<&str> = live
                .data
                .pointer("/status/addresses")
                .and_then(Value::as_array)
                .map(|a| a.iter().filter_map(|x| x["value"].as_str()).collect())
                .unwrap_or_default();
            let mut message = if addresses.is_empty() {
                "programmed, no address yet".to_owned()
            } else {
                format!("programmed at {}", addresses.join(", "))
            };
            if platform.cluster_issuer.is_some() && !platform.tls {
                message.push_str("; HTTPS is off until the ClusterIssuer is Ready");
            }
            (true, "Programmed", message)
        }
        Some((false, message)) => (false, "NotProgrammed", message.unwrap_or_default()),
        None => match (&platform.gateway_class, facts) {
            (Some(class), Some(facts)) => match facts.gateway_class(class) {
                Availability::Missing => (
                    false,
                    "GatewayClassMissing",
                    format!("GatewayClass {class} does not exist"),
                ),
                Availability::NotReady(why) => (
                    false,
                    "GatewayClassNotAccepted",
                    format!("GatewayClass {class} is not accepted: {why}"),
                ),
                Availability::Ready | Availability::Unknown => {
                    (false, "Pending", "waiting for the gateway controller".to_owned())
                }
            },
            _ => (false, "Pending", "waiting for the gateway controller".to_owned()),
        },
    }
}

/// Every domain routed to `gateway`, with its route's namespace. Routes of
/// both delivery paths carry their domains (M2.3).
#[must_use]
pub fn routed_hosts(routes: &[Arc<RouteView>], gateway: &GatewayRef) -> Vec<(DomainClaim, String)> {
    let key = format!("{}/{}", gateway.namespace, gateway.name);
    routes
        .iter()
        .filter(|r| r.gateways.contains(&key))
        .flat_map(|r| r.domains.iter().map(|d| (d.clone(), r.namespace.clone())))
        .collect()
}

fn dynamic_api(ctx: &Ctx, ns: &str, kind: &str, plural: &str) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gateway.networking.k8s.io", "v1", kind);
    Api::namespaced_with(
        ctx.client.clone(),
        ns,
        &ApiResource::from_gvk_with_plural(&gvk, plural),
    )
}

/// Record `verdict` as the `Gateway` condition of `KubenConfig` (none: drop
/// it), writing only on change.
async fn report(ctx: &Ctx, verdict: Option<Verdict>) -> anyhow::Result<()> {
    let Some(config) = ctx.config() else {
        return Ok(());
    };
    let previous = config
        .status
        .as_ref()
        .map_or(&[][..], |s| s.conditions.as_slice());
    let mut conditions: Vec<Condition> = previous
        .iter()
        .filter(|c| c.type_ != GATEWAY_CONDITION)
        .cloned()
        .collect();
    if let Some((ok, reason, message)) = verdict {
        conditions.push(condition(
            previous,
            GATEWAY_CONDITION,
            ok,
            reason,
            &message,
            config.metadata.generation,
        ));
    }
    if conditions == previous {
        return Ok(());
    }
    let status = json!({ "status": {
        "observedGeneration": config.metadata.generation,
        "conditions": conditions,
    } });
    Api::<KubenConfig>::all(ctx.client.clone())
        .patch_status(
            &config.name_any(),
            &PatchParams::default(),
            &Patch::Merge(&status),
        )
        .await?;
    Ok(())
}

/// Keep the redirect route while TLS is on; remove Kuben's when it is off.
async fn sync_redirect(ctx: &Ctx, gateway: &GatewayRef, tls: bool, pp: &PatchParams) -> anyhow::Result<()> {
    let routes = dynamic_api(ctx, &gateway.namespace, "HTTPRoute", "httproutes");
    if tls {
        routes
            .patch(REDIRECT_ROUTE, pp, &Patch::Apply(&redirect_route(gateway)))
            .await?;
        return Ok(());
    }
    if let Some(route) = routes.get_opt(REDIRECT_ROUTE).await?
        && route.labels().get(labels::MANAGED_BY).map(String::as_str) == Some(labels::MANAGER)
    {
        match routes.delete(REDIRECT_ROUTE, &DeleteParams::default()).await {
            Ok(_) => {}
            Err(e) if is_not_found(&e) => {}
            Err(e) => return Err(e.into()),
        }
    }
    Ok(())
}

async fn reconcile(ctx: &Ctx, projections: &Projections, last: &mut Option<Value>) -> anyhow::Result<()> {
    let platform = ctx.platform();
    let facts = ctx.facts();
    let Some(gateway) = &platform.gateway else {
        return report(ctx, None).await; // Apps are not exposed; nothing to say.
    };
    if facts
        .as_deref()
        .is_some_and(|f| f.gateway_api.is_none() && f.unknown.is_empty())
    {
        let why = "the Gateway API CRDs are not installed (standard channel)".to_owned();
        return report(ctx, Some((false, "GatewayAPIMissing", why))).await;
    }
    let gateways = dynamic_api(ctx, &gateway.namespace, "Gateway", "gateways");
    let live = gateways.get_opt(&gateway.name).await?;
    if let Some(verdict) = refusal(gateway, &platform, ownership(live.as_ref())) {
        *last = None;
        return report(ctx, Some(verdict)).await;
    }

    let plan = plan_listeners(&routed_hosts(&projections.routes(), gateway), &platform);
    for conflict in &plan.conflicts {
        tracing::warn!(%conflict, "hostname requested by two namespaces; the first keeps it");
    }
    if !plan.skipped.is_empty() {
        tracing::warn!(hosts = ?plan.skipped, max = MAX_LISTENERS, "gateway listener limit reached");
    }
    let body = gateway_patch(gateway, &platform, &plan);
    if live.is_none() || last.as_ref() != Some(&body) {
        let pp = PatchParams::apply(FIELD_MANAGER).force();
        if let Err(e) = gateways.patch(&gateway.name, &pp, &Patch::Apply(&body)).await {
            report(ctx, Some((false, "GatewayWriteFailed", e.to_string()))).await?;
            return Err(e.into());
        }
        sync_redirect(ctx, gateway, platform.tls, &pp).await?;
        tracing::info!(
            gateway = %format!("{}/{}", gateway.namespace, gateway.name),
            listeners = plan.listeners.len(),
            tls = platform.tls,
            "gateway listeners applied"
        );
        *last = Some(body);
    }
    let verdict = match gateways.get_opt(&gateway.name).await? {
        Some(live) => readiness(&live, &platform, facts.as_deref()),
        None => (false, "Pending", "the Gateway is being created".to_owned()),
    };
    report(ctx, Some(verdict)).await
}

/// Debounced reconciler: any projection change marks the listener set dirty;
/// at most one apply every 2 s, skipped when nothing changed; readiness read
/// every 30 s; full resync every 5 minutes.
pub async fn run(
    ctx: Arc<Ctx>,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let mut deltas = projections.subscribe();
    let mut debounce = tokio::time::interval(Duration::from_secs(2));
    let mut status = tokio::time::interval(STATUS_REFRESH);
    let mut resync = tokio::time::interval(Duration::from_mins(5));
    let mut dirty = true;
    let mut last: Option<Value> = None;
    loop {
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            delta = deltas.recv() => match delta {
                Ok(_) | Err(RecvError::Lagged(_)) => dirty = true,
                Err(RecvError::Closed) => return Ok(()),
            },
            _ = status.tick() => dirty = true,
            _ = resync.tick() => {
                dirty = true;
                last = None;
            }
            _ = debounce.tick() => {
                if dirty {
                    dirty = false;
                    if let Err(e) = reconcile(&ctx, &projections, &mut last).await {
                        tracing::warn!(error = %e, "gateway reconcile failed; retrying");
                        last = None;
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use kuben_crd::KubenConfigSpec;

    use super::*;

    fn platform(wildcard: bool) -> Platform {
        let mut spec = json!({
            "baseDomain": "apps.example.com",
            "gateway": "kuben-system/kuben",
            "clusterIssuer": "letsencrypt"
        });
        if wildcard {
            spec["wildcardTlsSecret"] = json!("apps-wildcard");
        }
        Platform::from_spec(Some(
            &serde_json::from_value::<KubenConfigSpec>(spec).expect("spec"),
        ))
    }

    fn hosts(pairs: &[(&str, &str)]) -> Vec<(DomainClaim, String)> {
        pairs
            .iter()
            .map(|(h, n)| {
                (
                    DomainClaim {
                        host: (*h).to_owned(),
                        tls: "auto".into(),
                    },
                    (*n).to_owned(),
                )
            })
            .collect()
    }

    #[test]
    fn listener_names_are_stable_and_distinct() {
        assert_eq!(
            host_listener_name("api.acme.com"),
            host_listener_name("api.acme.com")
        );
        assert_ne!(
            host_listener_name("api.acme.com"),
            host_listener_name("www.acme.com")
        );
        let name = host_listener_name("api.acme.com");
        assert!(name.starts_with("h-") && name.len() == 14, "{name}");
        assert!(host_secret_name("api.acme.com").starts_with("kuben-tls-"));
    }

    #[test]
    fn wildcard_covers_exactly_one_label() {
        let p = platform(true);
        assert_eq!(
            section_for_host("api-shop-prod.apps.example.com", &p),
            WILDCARD_LISTENER
        );
        assert_ne!(section_for_host("a.b.apps.example.com", &p), WILDCARD_LISTENER);
        assert_ne!(section_for_host("apps.example.com", &p), WILDCARD_LISTENER);
        assert_ne!(section_for_host("api.acme.com", &p), WILDCARD_LISTENER);
        assert_ne!(
            section_for_host("api-shop-prod.apps.example.com", &platform(false)),
            WILDCARD_LISTENER,
            "no wildcard secret → per-host listener"
        );
    }

    #[test]
    fn hosts_are_first_come_and_scoped_to_their_namespace() {
        let plan = plan_listeners(
            &hosts(&[
                ("shop.acme.com", "kb-b"),
                ("shop.acme.com", "kb-a"),
                ("api.acme.com", "kb-a"),
            ]),
            &platform(false),
        );
        assert_eq!(plan.conflicts.len(), 1, "{:?}", plan.conflicts);
        let shop = plan
            .listeners
            .iter()
            .find(|l| l["hostname"] == "shop.acme.com")
            .expect("shop listener");
        assert_eq!(
            shop["allowedRoutes"]["namespaces"]["selector"]["matchLabels"]["kubernetes.io/metadata.name"],
            "kb-a",
            "sorted input: kb-a claims the host first"
        );
        assert_eq!(
            shop["tls"]["certificateRefs"][0]["name"],
            host_secret_name("shop.acme.com")
        );
        let http = &plan.listeners[0];
        assert_eq!(
            (
                http["name"].as_str(),
                http["allowedRoutes"]["namespaces"]["from"].as_str()
            ),
            (Some("http"), Some("Same"))
        );
    }

    #[test]
    fn wildcard_listener_absorbs_generated_hosts_and_the_cap_holds() {
        let p = platform(true);
        let plan = plan_listeners(&hosts(&[("api-shop-prod.apps.example.com", "kb-shop-prod")]), &p);
        assert_eq!(plan.listeners.len(), 2, "http + wildcard only");
        assert_eq!(plan.listeners[1]["hostname"], "*.apps.example.com");

        let names: Vec<String> = (0..70).map(|i| format!("h{i}.acme.com")).collect();
        let pairs: Vec<(&str, &str)> = names.iter().map(|n| (n.as_str(), "kb-x")).collect();
        let many = hosts(&pairs);
        let capped = plan_listeners(&many, &platform(false));
        assert_eq!(capped.listeners.len(), MAX_LISTENERS);
        assert_eq!(capped.skipped.len(), 70 - (MAX_LISTENERS - 1));
        assert_eq!(plan_listeners(&many, &platform(false)), capped, "deterministic");
    }

    #[test]
    fn gateway_patch_and_redirect_route() {
        let p = platform(false);
        let gw = p.gateway.clone().expect("gateway");
        let plan = plan_listeners(&hosts(&[("api.acme.com", "kb-a")]), &p);
        let patch = gateway_patch(&gw, &p, &plan);
        assert_eq!(patch["metadata"]["annotations"][ISSUER_ANNOTATION], "letsencrypt");
        assert_eq!(patch["spec"]["listeners"].as_array().map(Vec::len), Some(2));
        assert!(
            patch["metadata"].get("labels").is_none() && patch["spec"].get("gatewayClassName").is_none(),
            "an existing Gateway keeps its own class and labels"
        );
        let route = redirect_route(&gw);
        assert_eq!(route["spec"]["parentRefs"][0]["sectionName"], HTTP_LISTENER);
        assert_eq!(
            route["spec"]["rules"][0]["filters"][0]["requestRedirect"]["scheme"],
            "https"
        );
    }

    #[test]
    fn without_tls_only_plain_http_is_listed_for_kubens_namespaces() {
        let mut p = platform(true);
        p.tls = false;
        let plan = plan_listeners(&hosts(&[("api.acme.com", "kb-a"), ("api.acme.com", "kb-b")]), &p);
        assert_eq!(
            plan.listeners.len(),
            1,
            "no HTTPS listener without a Ready issuer"
        );
        assert!(plan.conflicts.is_empty() && plan.skipped.is_empty());
        assert_eq!(
            plan.listeners[0]["allowedRoutes"]["namespaces"]["selector"]["matchLabels"][labels::MANAGED_BY],
            labels::MANAGER
        );
        let gw = p.gateway.clone().expect("gateway");
        let patch = gateway_patch(&gw, &p, &plan);
        assert!(
            patch["metadata"].get("annotations").is_none(),
            "no issuer annotation: cert-manager must not order certificates"
        );
    }

    fn owned_platform() -> Platform {
        Platform::from_spec(Some(
            &serde_json::from_value::<KubenConfigSpec>(json!({
                "baseDomain": "apps.example.com",
                "gatewayClassName": "traefik",
                "clusterIssuer": "letsencrypt"
            }))
            .expect("spec"),
        ))
    }

    fn gateway_object(labels: &Value, status: &Value) -> DynamicObject {
        let mut obj: DynamicObject = serde_json::from_value(json!({
            "apiVersion": "gateway.networking.k8s.io/v1",
            "kind": "Gateway",
            "metadata": { "name": "kuben", "namespace": "kuben-system", "labels": labels },
        }))
        .expect("gateway");
        obj.data = json!({ "status": status });
        obj
    }

    #[test]
    fn kuben_writes_only_a_gateway_it_owns() {
        let owned = owned_platform();
        let gw = owned.gateway.clone().expect("default gateway");
        assert_eq!(gw, GatewayRef::owned_default());
        let mine = gateway_object(&json!({ labels::GATEWAY_OWNER: labels::MANAGER }), &json!({}));
        let theirs = gateway_object(&json!({ "team": "edge" }), &json!({}));
        assert_eq!(ownership(Some(&mine)), Ownership::Kuben);
        assert_eq!(ownership(Some(&theirs)), Ownership::Foreign);
        assert_eq!(ownership(None), Ownership::Absent);

        assert_eq!(refusal(&gw, &owned, Ownership::Absent), None, "created by Kuben");
        assert_eq!(refusal(&gw, &owned, Ownership::Kuben), None);
        let foreign = refusal(&gw, &owned, Ownership::Foreign).expect("refused");
        assert_eq!((foreign.0, foreign.1), (false, "GatewayNotOwned"));
        assert!(
            foreign.2.contains("kuben.dev/gateway-owner=kuben"),
            "{}",
            foreign.2
        );

        let named = platform(false);
        let gw = named.gateway.clone().expect("gateway");
        assert_eq!(
            refusal(&gw, &named, Ownership::Absent).map(|v| v.1),
            Some("GatewayMissing")
        );
        assert_eq!(refusal(&gw, &named, Ownership::Kuben), None, "dedicated by label");

        let plan = plan_listeners(&[], &owned);
        let patch = gateway_patch(&GatewayRef::owned_default(), &owned, &plan);
        assert_eq!(patch["spec"]["gatewayClassName"], "traefik");
        assert_eq!(
            patch["metadata"]["labels"][labels::GATEWAY_OWNER],
            labels::MANAGER
        );
    }

    #[test]
    fn readiness_follows_programmed_and_explains_the_class() {
        let owned = owned_platform();
        let programmed = gateway_object(
            &json!({}),
            &json!({
                "conditions": [{ "type": "Programmed", "status": "True" }],
                "addresses": [{ "type": "IPAddress", "value": "203.0.113.7" }]
            }),
        );
        assert_eq!(
            readiness(&programmed, &owned, None),
            (true, "Programmed", "programmed at 203.0.113.7".to_owned())
        );
        let mut blocked = owned.clone();
        blocked.tls = false;
        assert!(readiness(&programmed, &blocked, None).2.contains("HTTPS is off"));

        let failing = gateway_object(
            &json!({}),
            &json!({ "conditions": [{ "type": "Programmed", "status": "False", "message": "no address" }] }),
        );
        assert_eq!(readiness(&failing, &owned, None).1, "NotProgrammed");

        let fresh = gateway_object(&json!({}), &json!({}));
        let facts = ClusterFacts {
            gateway_api: Some(discovery::GatewayApi::default()),
            ..ClusterFacts::default()
        };
        assert_eq!(readiness(&fresh, &owned, Some(&facts)).1, "GatewayClassMissing");
        assert_eq!(readiness(&fresh, &owned, None).1, "Pending");
    }

    fn route(namespace: &str, gateway: &str, domains: &[(&str, &str)]) -> Arc<RouteView> {
        Arc::new(RouteView {
            key: format!("{namespace}/api"),
            namespace: namespace.into(),
            name: "api".into(),
            gateways: vec![gateway.into()],
            sections: vec![],
            domains: domains
                .iter()
                .map(|(host, tls)| DomainClaim {
                    host: (*host).into(),
                    tls: (*tls).into(),
                })
                .collect(),
            accepted: None,
            message: None,
        })
    }

    #[test]
    fn hosts_come_from_the_routes_of_this_gateway() {
        let gw = platform(false).gateway.expect("gateway");
        let routes = vec![
            route(
                "kb-a",
                "kuben-system/kuben",
                &[("api.acme.com", "auto"), ("old.acme.com", "none")],
            ),
            route("kb-b", "other/edge", &[("edge.acme.com", "auto")]),
        ];
        let got = routed_hosts(&routes, &gw);
        assert_eq!(got.len(), 2, "only routes of Kuben's gateway: {got:?}");
        assert!(got.iter().all(|(_, ns)| ns == "kb-a"));
    }

    #[test]
    fn each_tls_mode_gets_its_listener() {
        let p = platform(false);
        let claim = |host: &str, tls: &str| DomainClaim {
            host: host.into(),
            tls: tls.into(),
        };
        let hosts = vec![
            (claim("auto.acme.com", "auto"), "kb-a".to_owned()),
            (claim("plain.acme.com", "none"), "kb-a".to_owned()),
            (claim("own.acme.com", "acme-cert"), "kb-a".to_owned()),
        ];
        let plan = plan_listeners(&hosts, &p);
        let by_host = |host: &str| {
            plan.listeners
                .iter()
                .find(|l| l["hostname"] == host)
                .cloned()
                .expect("listener")
        };
        let http_only = by_host("plain.acme.com");
        assert_eq!(
            (
                http_only["name"].as_str(),
                http_only["protocol"].as_str(),
                http_only["port"].as_i64()
            ),
            (
                Some(plain_listener_name("plain.acme.com").as_str()),
                Some("HTTP"),
                Some(80)
            )
        );
        let own = by_host("own.acme.com");
        assert_eq!(
            own["tls"]["certificateRefs"][0],
            json!({ "kind": "Secret", "name": "acme-cert", "namespace": "kb-a" })
        );
        assert_eq!(
            by_host("auto.acme.com")["tls"]["certificateRefs"][0]["name"],
            host_secret_name("auto.acme.com")
        );
        assert_eq!(
            section_for(&claim("plain.acme.com", "none"), &p),
            plain_listener_name("plain.acme.com")
        );
        assert_eq!(
            section_for(&claim("own.acme.com", "acme-cert"), &p),
            host_listener_name("own.acme.com")
        );

        // A wildcard covers generated hosts but never an app's own certificate.
        let wild = platform(true);
        let generated = "api-shop-prod.apps.example.com";
        assert_eq!(section_for(&claim(generated, "auto"), &wild), WILDCARD_LISTENER);
        assert_eq!(
            section_for(&claim(generated, "acme-cert"), &wild),
            host_listener_name(generated)
        );
    }
}

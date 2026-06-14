//! Automatic HTTPS for app hostnames (scenario 9).
//!
//! When `KubenConfig.spec.clusterIssuer` is set, Kuben owns the listeners of
//! the Gateway named in `spec.gateway` (which must be dedicated to Kuben):
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
//! The `cert-manager.io/cluster-issuer` annotation makes cert-manager's
//! gateway-shim issue one certificate per listener into `kuben-tls-<hash>`.

use std::{collections::BTreeMap, sync::Arc, time::Duration};

use kube::{
    Api,
    api::{ApiResource, DynamicObject, GroupVersionKind, Patch, PatchParams},
};
use kuben_crd::{FIELD_MANAGER, labels};
use serde_json::{Value, json};
use tokio::sync::broadcast::error::RecvError;
use tokio_util::sync::CancellationToken;

use super::{
    Ctx,
    resources::{GatewayRef, Platform, hostnames_for},
};
use crate::projection::{AppView, Projections};

pub const HTTP_LISTENER: &str = "http";
pub const WILDCARD_LISTENER: &str = "https";
pub const REDIRECT_ROUTE: &str = "kuben-https-redirect";
pub const ISSUER_ANNOTATION: &str = "cert-manager.io/cluster-issuer";
/// Gateway API allows at most 64 listeners; leave headroom for hand-made ones.
pub const MAX_LISTENERS: usize = 60;

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

#[must_use]
pub fn host_secret_name(host: &str) -> String {
    format!("kuben-tls-{}", host_hash(host))
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

/// `hosts`: `(hostname, namespace)` for every routed app. Deterministic:
/// the same input always yields the same listeners in the same order.
#[must_use]
pub fn plan_listeners(hosts: &[(String, String)], platform: &Platform) -> ListenerPlan {
    let mut sorted: Vec<&(String, String)> = hosts.iter().collect();
    sorted.sort();
    let mut owners: BTreeMap<&str, &str> = BTreeMap::new();
    let mut conflicts = Vec::new();
    for (host, ns) in sorted {
        match owners.get(host.as_str()) {
            Some(existing) if *existing != ns.as_str() => {
                conflicts.push(format!("{host}: owned by {existing}, also requested by {ns}"));
            }
            Some(_) => {}
            None => {
                owners.insert(host, ns);
            }
        }
    }

    let mut listeners = vec![json!({
        "name": HTTP_LISTENER,
        "protocol": "HTTP",
        "port": 80,
        "allowedRoutes": { "namespaces": { "from": "Same" } },
    })];
    if let (Some(base), Some(secret)) = (&platform.base_domain, &platform.wildcard_tls_secret) {
        listeners.push(json!({
            "name": WILDCARD_LISTENER,
            "protocol": "HTTPS",
            "port": 443,
            "hostname": format!("*.{base}"),
            "tls": tls(secret),
            "allowedRoutes": { "namespaces": { "from": "Selector", "selector": {
                "matchLabels": { labels::MANAGED_BY: labels::MANAGER }
            } } },
        }));
    }
    let mut skipped = Vec::new();
    for (host, ns) in owners {
        if covered_by_wildcard(host, platform) {
            continue;
        }
        if listeners.len() >= MAX_LISTENERS {
            skipped.push(host.to_owned());
            continue;
        }
        listeners.push(json!({
            "name": host_listener_name(host),
            "protocol": "HTTPS",
            "port": 443,
            "hostname": host,
            "tls": tls(&host_secret_name(host)),
            "allowedRoutes": { "namespaces": { "from": "Selector", "selector": {
                "matchLabels": { "kubernetes.io/metadata.name": ns }
            } } },
        }));
    }
    ListenerPlan {
        listeners,
        conflicts,
        skipped,
    }
}

/// Server-side-apply body for the Gateway: Kuben's field manager owns only the
/// listeners it lists and the issuer annotation.
#[must_use]
pub fn gateway_patch(gateway: &GatewayRef, issuer: &str, plan: &ListenerPlan) -> Value {
    json!({
        "apiVersion": "gateway.networking.k8s.io/v1",
        "kind": "Gateway",
        "metadata": {
            "name": gateway.name,
            "namespace": gateway.namespace,
            "annotations": { ISSUER_ANNOTATION: issuer },
        },
        "spec": { "listeners": plan.listeners },
    })
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

/// `(hostname, namespace)` of every app whose web process is routed over HTTP.
#[must_use]
pub fn routed_hosts(apps: &[Arc<AppView>], platform: &Platform) -> Vec<(String, String)> {
    let mut out = Vec::new();
    for app in apps {
        let routed = app
            .processes
            .iter()
            .any(|p| p.port.is_some() && p.schedule.is_none() && p.protocol == "http");
        if !routed {
            continue;
        }
        let domains = app.domains.iter().map(String::as_str);
        for host in hostnames_for(&app.name, app.environment.as_deref(), domains, platform) {
            out.push((host, app.namespace.clone()));
        }
    }
    out
}

fn dynamic_api(ctx: &Ctx, ns: &str, kind: &str, plural: &str) -> Api<DynamicObject> {
    let gvk = GroupVersionKind::gvk("gateway.networking.k8s.io", "v1", kind);
    Api::namespaced_with(
        ctx.client.clone(),
        ns,
        &ApiResource::from_gvk_with_plural(&gvk, plural),
    )
}

async fn reconcile(ctx: &Ctx, projections: &Projections, last: &mut Option<Value>) -> anyhow::Result<()> {
    let platform = ctx.platform();
    let (Some(gateway), Some(issuer)) = (&platform.gateway, &platform.cluster_issuer) else {
        return Ok(()); // TLS off: the Gateway is left untouched.
    };
    let plan = plan_listeners(&routed_hosts(&projections.apps(), &platform), &platform);
    for conflict in &plan.conflicts {
        tracing::warn!(%conflict, "hostname requested by two namespaces; the first keeps it");
    }
    if !plan.skipped.is_empty() {
        tracing::warn!(hosts = ?plan.skipped, max = MAX_LISTENERS, "gateway listener limit reached");
    }
    let body = gateway_patch(gateway, issuer, &plan);
    if last.as_ref() == Some(&body) {
        return Ok(());
    }
    let pp = PatchParams::apply(FIELD_MANAGER).force();
    dynamic_api(ctx, &gateway.namespace, "Gateway", "gateways")
        .patch(&gateway.name, &pp, &Patch::Apply(&body))
        .await?;
    dynamic_api(ctx, &gateway.namespace, "HTTPRoute", "httproutes")
        .patch(REDIRECT_ROUTE, &pp, &Patch::Apply(&redirect_route(gateway)))
        .await?;
    tracing::info!(listeners = plan.listeners.len(), "gateway listeners applied");
    *last = Some(body);
    Ok(())
}

/// Debounced reconciler: any projection change marks the listener set dirty;
/// at most one apply every 2 s, skipped when nothing changed; full resync
/// every 5 minutes.
pub async fn run(
    ctx: Arc<Ctx>,
    projections: Arc<Projections>,
    token: CancellationToken,
) -> anyhow::Result<()> {
    let mut deltas = projections.subscribe();
    let mut debounce = tokio::time::interval(Duration::from_secs(2));
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
            _ = resync.tick() => {
                dirty = true;
                last = None;
            }
            _ = debounce.tick() => {
                if dirty {
                    dirty = false;
                    if let Err(e) = reconcile(&ctx, &projections, &mut last).await {
                        tracing::warn!(error = %e, "gateway reconcile failed; retrying at the next resync");
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
    use crate::projection::ProcessView;

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

    fn hosts(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        pairs
            .iter()
            .map(|(h, n)| ((*h).to_owned(), (*n).to_owned()))
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

        let many: Vec<(String, String)> = (0..70)
            .map(|i| (format!("h{i}.acme.com"), "kb-x".to_owned()))
            .collect();
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
        let patch = gateway_patch(&gw, "letsencrypt", &plan);
        assert_eq!(patch["metadata"]["annotations"][ISSUER_ANNOTATION], "letsencrypt");
        assert_eq!(patch["spec"]["listeners"].as_array().map(Vec::len), Some(2));
        let route = redirect_route(&gw);
        assert_eq!(route["spec"]["parentRefs"][0]["sectionName"], HTTP_LISTENER);
        assert_eq!(
            route["spec"]["rules"][0]["filters"][0]["requestRedirect"]["scheme"],
            "https"
        );
    }

    #[test]
    fn only_http_web_processes_are_routed() {
        let view = |name: &str, protocol: &str, schedule: Option<&str>| {
            Arc::new(AppView {
                key: format!("kb-shop-prod/{name}"),
                namespace: "kb-shop-prod".into(),
                name: name.into(),
                uid: None,
                org: None,
                project: Some("shop".into()),
                environment: Some("shop-prod".into()),
                image: None,
                git_repo: None,
                url: None,
                ready: true,
                reason: None,
                message: None,
                processes: vec![ProcessView {
                    name: "web".into(),
                    command: vec![],
                    port: schedule.is_none().then_some(80),
                    size: "small".into(),
                    min_replicas: 1,
                    max_replicas: 1,
                    schedule: schedule.map(str::to_owned),
                    protocol: protocol.into(),
                }],
                env: vec![],
                domains: vec![format!("{name}.acme.com")],
                volumes: vec![],
                created_at: None,
            })
        };
        let apps = vec![
            view("api", "http", None),
            view("db", "tcp", None),
            view("job", "http", Some("@daily")),
        ];
        let got = routed_hosts(&apps, &platform(false));
        assert_eq!(
            got,
            vec![
                ("api.acme.com".to_owned(), "kb-shop-prod".to_owned()),
                (
                    "api-shop-prod.apps.example.com".to_owned(),
                    "kb-shop-prod".to_owned()
                ),
            ]
        );
    }
}

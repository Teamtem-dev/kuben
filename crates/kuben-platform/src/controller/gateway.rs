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

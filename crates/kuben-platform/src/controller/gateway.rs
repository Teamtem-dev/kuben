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

//! DNS check of an app's hostnames against the gateway (scenario 9).

use std::{net::IpAddr, time::Duration};

use axum::{
    Json,
    extract::{Path, State},
};
use kube::{
    Api,
    api::{ApiResource, DynamicObject, GroupVersionKind},
};
use kuben_core::perm::Perm;
use kuben_crd::KubenConfig;
use kuben_platform::controller::{KUBEN_CONFIG_NAME, Platform, resources};
use serde::Serialize;
use utoipa::ToSchema;

use crate::{authz::Authz, error::ApiResult, routes::scope, state::ApiState};

#[derive(Debug, Serialize, ToSchema)]
pub struct DomainCheck {
    pub host: String,
    /// Addresses the host currently resolves to.
    pub addresses: Vec<String>,
    /// Addresses of the gateway.
    pub expected: Vec<String>,
    /// `ok`, `mismatch`, `unresolved` or `unknown`.
    pub status: String,
    pub message: String,
}

/// Compare what a host resolves to with the gateway's addresses.
#[must_use]
pub fn domain_verdict(resolved: &[IpAddr], expected: &[IpAddr]) -> (&'static str, String) {
    let join = |ips: &[IpAddr]| ips.iter().map(ToString::to_string).collect::<Vec<_>>().join(", ");
    if resolved.is_empty() {
        return (
            "unresolved",
            "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway".into(),
        );
    }
    if expected.is_empty() {
        return (
            "unknown",
            format!(
                "resolves to {}; the gateway reports no address to compare with",
                join(resolved)
            ),
        );

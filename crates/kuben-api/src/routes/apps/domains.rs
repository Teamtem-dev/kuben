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
    }
    if resolved.iter().any(|ip| expected.contains(ip)) {
        ("ok", "points at the gateway".into())
    } else {
        (
            "mismatch",
            format!(
                "points at {} but the gateway is {}",
                join(resolved),
                join(expected)
            ),
        )
    }
}

async fn resolve(host: &str) -> Vec<IpAddr> {
    match tokio::time::timeout(Duration::from_secs(3), tokio::net::lookup_host((host, 443))).await {
        Ok(Ok(addrs)) => {
            let mut ips: Vec<IpAddr> = addrs.map(|a| a.ip()).collect();
            ips.sort();
            ips.dedup();
            ips
        }
        _ => Vec::new(),
    }
}

async fn platform(client: &kube::Client) -> Platform {
    let config = Api::<KubenConfig>::all(client.clone())
        .get_opt(KUBEN_CONFIG_NAME)
        .await
        .ok()
        .flatten();
    Platform::from_spec(config.as_ref().map(|c| &c.spec))
}

async fn gateway_addresses(client: &kube::Client, platform: &Platform) -> Vec<IpAddr> {
    let Some(gw) = &platform.gateway else {
        return Vec::new();
    };
    let gvk = GroupVersionKind::gvk("gateway.networking.k8s.io", "v1", "Gateway");
    let api: Api<DynamicObject> = Api::namespaced_with(
        client.clone(),
        &gw.namespace,
        &ApiResource::from_gvk_with_plural(&gvk, "gateways"),
    );
    let Ok(Some(gateway)) = api.get_opt(&gw.name).await else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for address in gateway.data["status"]["addresses"]
        .as_array()
        .into_iter()
        .flatten()
    {
        let Some(value) = address["value"].as_str() else {
            continue;
        };
        match value.parse::<IpAddr>() {
            Ok(ip) => out.push(ip),
            Err(_) => out.extend(resolve(value).await),
        }
    }
    out
}

/// Check that every hostname of the app points at the gateway.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/domains", operation_id = "checkAppDomains",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = Vec<DomainCheck>), (status = 503, body = crate::error::Problem))
)]
pub async fn domains(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<Vec<DomainCheck>>> {
    let a = scope::app(&state, &authz, &project, &environment, &app)?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let client = scope::cluster(&state)?;
    let platform = platform(&client).await;
    let expected = gateway_addresses(&client, &platform).await;
    let hosts = resources::hostnames_for(
        &a.view.name,
        a.view.environment.as_deref(),
        a.view.domains.iter().map(String::as_str),
        &platform,
    );
    let checks = hosts.iter().map(|host| {
        let expected = expected.clone();
        async move {
            let resolved = resolve(host).await;
            let (status, message) = domain_verdict(&resolved, &expected);
            DomainCheck {
                host: host.clone(),
                addresses: resolved.iter().map(ToString::to_string).collect(),
                expected: expected.iter().map(ToString::to_string).collect(),
                status: status.into(),
                message,
            }
        }
    });
    Ok(Json(futures::future::join_all(checks).await))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn domain_verdicts() {
        let gw: IpAddr = "203.0.113.10".parse().expect("ip");
        let other: IpAddr = "198.51.100.7".parse().expect("ip");
        assert_eq!(domain_verdict(&[gw], &[gw]).0, "ok");
        assert_eq!(domain_verdict(&[other], &[gw]).0, "mismatch");
        assert_eq!(domain_verdict(&[], &[gw]).0, "unresolved");
        assert_eq!(domain_verdict(&[other], &[]).0, "unknown");
    }
}

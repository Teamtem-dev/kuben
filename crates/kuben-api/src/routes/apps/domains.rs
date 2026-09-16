//! DNS check of an app's hostnames against the gateway (scenario 9).

use axum::{
    Json,
    extract::{Path, State},
};
use kuben_core::perm::Perm;
use kuben_platform::{
    controller::{Platform, resources},
    doctor,
};
use serde::Serialize;
use utoipa::ToSchema;

use super::desired_spec;
use crate::{
    authz::Authz,
    error::ApiResult,
    routes::scope::{self, AppScope},
    state::ApiState,
};

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

/// The public hostnames of the app: its domains, or the generated one.
pub(super) fn app_hosts(a: &AppScope, platform: &Platform) -> Vec<String> {
    let environment = a.env.resource_name();
    let domains: Vec<String> = desired_spec(&a.app)
        .map(|spec| spec.domains.into_iter().map(|d| d.host).collect())
        .unwrap_or_default();
    resources::hostnames_for(
        a.slug(),
        Some(environment.as_str()),
        domains.iter().map(String::as_str),
        platform,
    )
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
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let client = scope::cluster(&state)?;
    let platform = doctor::read_platform(&client).await;
    let expected = doctor::read_gateway(&client, &platform)
        .await
        .addresses()
        .to_vec();
    let hosts = app_hosts(&a, &platform);
    let checks = hosts.iter().map(|host| {
        let expected = expected.clone();
        async move {
            let resolved = doctor::resolve(host).await;
            let (status, message) = doctor::dns_verdict(&resolved, &expected);
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

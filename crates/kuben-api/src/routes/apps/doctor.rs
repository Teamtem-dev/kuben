//! Doctor of an app (M2.13): every check between the app and a visitor, from
//! the Gateway's class to the agent that delivers it.

use axum::{
    Json,
    extract::{Path, State},
};
use kuben_core::perm::Perm;
use kuben_platform::{
    discovery::ClusterFacts,
    doctor::{self, AgentState, Check, Status},
    registry::ClusterId,
};
use kuben_store::repo::Delivery;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::domains::app_hosts;
use crate::{
    authz::Authz,
    error::ApiResult,
    routes::scope::{self, AppScope},
    state::ApiState,
};

#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct DoctorCheck {
    /// `gateway-class`, `gateway`, `issuer`, `port-80`, `port-443`, `route`,
    /// `certificate`, `dns`, `claim`, `delegation`, `proxy` or `agent`.
    pub id: String,
    /// What was checked (a host, a class, a port), when there are several.
    pub subject: String,
    /// `ok`, `warn`, `unknown` (could not be checked) or `fail`.
    pub status: String,
    pub detail: String,
    /// What to do about it.
    pub hint: Option<String>,
}

#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct DoctorReport {
    /// The worst status of the checks; `unknown` is never `ok`.
    pub status: String,
    pub checks: Vec<DoctorCheck>,
}

fn status_name(status: Status) -> String {
    match status {
        Status::Ok => "ok",
        Status::Warn => "warn",
        Status::Unknown => "unknown",
        Status::Fail => "fail",
    }
    .to_owned()
}

impl From<Check> for DoctorCheck {
    fn from(c: Check) -> Self {
        Self {
            id: c.id.to_owned(),
            subject: c.subject,
            status: status_name(c.status),
            detail: c.detail,
            hint: c.hint,
        }
    }
}

/// Why the app is or is not reachable: the GatewayClass and the Gateway, the
/// issuer, ports 80 and 443 (from the server), the route, each host's
/// certificate and DNS, and the agent that delivers it.
#[utoipa::path(
    get,
    path = "/projects/{project}/environments/{environment}/apps/{app}/doctor", operation_id = "getAppDoctor",
    tag = "apps",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    responses((status = 200, body = DoctorReport), (status = 503, body = crate::error::Problem))
)]
pub async fn doctor(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
) -> ApiResult<Json<DoctorReport>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppRead, &a.chain())?;
    let client = scope::cluster(&state)?;
    let platform = doctor::read_platform(&client).await;
    let gateway = doctor::read_gateway(&client, &platform).await;
    let facts = facts(&state, &a, &client).await?;
    let addresses = gateway.addresses().to_vec();

    let mut checks = doctor::platform_checks(facts.as_ref(), &platform, &gateway);
    let hosts = app_hosts(&a, &platform);
    let (http, https) = tokio::join!(
        doctor::probe_port(&addresses, 80),
        doctor::probe_port(&addresses, 443)
    );
    checks.push(doctor::port_check(80, http));
    if platform.tls {
        checks.push(doctor::port_check(443, https));
    }
    checks.extend(doctor::exposure_checks(
        state.projections.exposure(a.namespace(), a.slug()).as_ref(),
        !hosts.is_empty(),
    ));
    let dns = hosts.iter().map(|host| {
        let addresses = &addresses;
        async move { doctor::dns_check(host, &doctor::resolve(host).await, addresses) }
    });
    checks.extend(futures::future::join_all(dns).await);
    checks.extend(domain_checks(&state, &a).await?);
    if a.app.delivery == Delivery::Agent {
        let stale_after = (state.cfg.agent.heartbeat_secs * 3).max(30);
        checks.push(doctor::agent_check(&agent(&state, &a).await?, stale_after));
    }
    Ok(Json(DoctorReport {
        status: status_name(doctor::overall(&checks)),
        checks: checks.into_iter().map(DoctorCheck::from).collect(),
    }))
}

/// Each custom domain's claim, delegation and proxy (M5.2).
async fn domain_checks(state: &ApiState, a: &AppScope) -> ApiResult<Vec<Check>> {
    let org = a.env.project.org;
    let hosts: Vec<String> = super::desired_spec(&a.app)
        .map(|spec| spec.domains.into_iter().map(|d| d.host).collect())
        .unwrap_or_default();
    let mut checks = Vec::new();
    for host in hosts {
        let Ok(host) = kuben_core::domain::canonical(&host) else {
            continue;
        };
        let owner = match state.store.domain_owner(&host).await? {
            Some((domain, owner)) if owner == org => doctor::ClaimOwner::Ours(domain),
            Some(_) => doctor::ClaimOwner::Others,
            None => doctor::ClaimOwner::Nobody,
        };
        checks.push(doctor::claim_check(
            &host,
            &owner,
            state.cfg.domains.require_claim,
        ));
        let zone = kuben_core::domain::lock_key(&host).to_owned();
        let servers = state.dns.ns(&zone).await.map_err(|e| e.to_string());
        checks.push(doctor::delegation_check(
            &host,
            &zone,
            servers.as_deref().map_err(String::as_str),
        ));
        checks.push(doctor::proxy_check(&host, proxied(state, org, &host).await));
    }
    Ok(checks)
}

/// Whether a DNS provider account of `org` proxies `host`'s record; `None`
/// when no account holds it (or none answered).
async fn proxied(state: &ApiState, org: kuben_core::ids::OrgId, host: &str) -> Option<bool> {
    let keyring = state.keyring.as_deref()?;
    let mut tenant = state.store.tenant(org).await.ok()?;
    for p in tenant.dns_providers().await.ok()? {
        let Ok(Some((kind, sealed))) = tenant.dns_provider_secret(p.id).await else {
            continue;
        };
        let Some(token) = crate::notify::open_secret(keyring, org, p.id, &sealed)
            .ok()
            .and_then(|t| String::from_utf8(t).ok())
        else {
            continue;
        };
        let Some(api) = state.dns.provider(&kind, &token) else {
            continue;
        };
        if let Ok(Some(zone)) = api.zone_for(host).await
            && let Ok(records) = api.records(&zone, host).await
            && !records.is_empty()
        {
            return Some(records.iter().any(|r| r.proxied));
        }
    }
    None
}

/// A recorded observation older than this is asked again.
const FACTS_FRESH_MS: i64 = 5 * 60 * 1000;

/// What discovery recorded of the app's cluster, or, when that is missing
/// or old, what the cluster says now.
async fn facts(state: &ApiState, a: &AppScope, client: &kube::Client) -> ApiResult<Option<ClusterFacts>> {
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let record = tenant.cluster_capabilities(ClusterId::PRIMARY).await?;
    drop(tenant);
    let now = kuben_core::time::now_ms();
    if let Some(facts) = record
        .filter(|r| now - r.observed_at < FACTS_FRESH_MS)
        .and_then(|r| serde_json::from_value(r.facts).ok())
    {
        return Ok(Some(facts));
    }
    Ok(Some(kuben_platform::discovery::discover(client).await))
}

async fn agent(state: &ApiState, a: &AppScope) -> ApiResult<AgentState> {
    let mut tenant = state.store.tenant(a.env.project.org).await?;
    let cluster = tenant.ensure_cluster(ClusterId::PRIMARY).await?;
    drop(tenant);
    let now = kuben_core::time::now_ms();
    Ok(match state.store.cluster_agent(cluster).await? {
        None => AgentState::None,
        Some(agent) if agent.revoked_at.is_some() => AgentState::Revoked,
        Some(agent) => AgentState::Seen(
            agent
                .last_seen_at
                .map(|seen| u64::try_from((now - seen).max(0) / 1000).unwrap_or(0)),
        ),
    })
}

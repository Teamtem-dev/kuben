//! Domain claims, DNS provider accounts and an app's DNS records (M5.2).
//!
//! An organization claims a domain and proves it in one of two ways:
//! - a TXT record `_kuben-challenge.<domain>` holding the claim's token,
//!   read through DNS-over-HTTPS;
//! - a DNS provider account that holds the domain's zone.
//!
//! A verified domain is the organization's alone: no other organization
//! can verify an overlapping name or serve a host below it.

use std::{net::IpAddr, sync::Arc};

use axum::{
    Json,
    extract::{Path, Query, State},
    http::StatusCode,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use kuben_core::{Error, domain, ids::OrgId, perm::Perm, time::now_ms};
use kuben_store::repo::{DnsProvider, DomainClaim, NewDnsRecord, Tenant, Verified};
use ring::rand::{SecureRandom, SystemRandom};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::{apps::desired_spec, request, scope};
use crate::{
    authz::Authz,
    dns::{self, Change, DnsError},
    error::{ApiError, ApiResult},
    notify,
    state::ApiState,
};

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct ClaimDto {
    pub id: Uuid,
    pub domain: String,
    /// `pending`, `verified` or `revoked`.
    pub status: String,
    /// The TXT record that proves the claim, and its value.
    pub challenge_name: String,
    pub challenge_value: String,
    /// `txt` or the provider kind that proved it.
    pub method: Option<String>,
    pub created_by: String,
    pub created_at: String,
    pub verified_at: Option<String>,
    pub last_checked_at: Option<String>,
    pub last_error: Option<String>,
}

impl From<DomainClaim> for ClaimDto {
    fn from(c: DomainClaim) -> Self {
        Self {
            id: c.id,
            challenge_name: domain::challenge_name(&c.domain),
            challenge_value: c.token,
            domain: c.domain,
            status: c.status,
            method: c.method,
            created_by: c.created_by,
            created_at: request::timestamp(c.created_at),
            verified_at: c.verified_at.map(request::timestamp),
            last_checked_at: c.last_checked_at.map(request::timestamp),
            last_error: c.last_error,
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct CreateClaim {
    #[schema(example = "example.com")]
    pub domain: String,
}

#[derive(Debug, Default, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct VerifyClaim {
    /// Prove the claim with this DNS provider account instead of a TXT record.
    #[serde(default)]
    pub provider: Option<String>,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct ClaimQuery {
    /// Include revoked claims.
    #[serde(default)]
    pub all: bool,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct DnsProviderDto {
    pub id: Uuid,
    pub name: String,
    /// `cloudflare`.
    pub kind: String,
    pub created_by: String,
    pub created_at: String,
}

impl From<DnsProvider> for DnsProviderDto {
    fn from(p: DnsProvider) -> Self {
        Self {
            id: p.id,
            name: p.name,
            kind: p.kind,
            created_by: p.created_by,
            created_at: request::timestamp(p.created_at),
        }
    }
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct CreateDnsProvider {
    #[schema(example = "cloudflare")]
    pub name: String,
    /// `cloudflare`.
    pub kind: String,
    /// An API token that may edit the zones' DNS (write-only).
    pub token: String,
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct SyncDns {
    /// The DNS provider account to write through.
    pub provider: String,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct DnsChangeDto {
    pub host: String,
    pub record_type: Option<String>,
    pub content: Option<String>,
    /// `created`, `updated`, `unchanged`, `deleted`, `conflict`, `skipped` or
    /// `failed`.
    pub action: String,
    pub detail: Option<String>,
}

fn org_of(authz: &Authz) -> Result<OrgId, ApiError> {
    authz.org_ids().first().copied().ok_or(ApiError(Error::Forbidden))
}

fn random_token(bytes: usize) -> Result<String, Error> {
    let mut raw = vec![0u8; bytes];
    SystemRandom::new()
        .fill(&mut raw)
        .map_err(|_| Error::Internal("no randomness".into()))?;
    Ok(URL_SAFE_NO_PAD.encode(raw))
}

/// The organization's domain claims.
#[utoipa::path(
    get, path = "/domains", operation_id = "listDomainClaims", tag = "domains",
    params(ClaimQuery),
    responses((status = 200, body = Vec<ClaimDto>))
)]
pub async fn list(
    State(state): State<ApiState>,
    authz: Authz,
    Query(q): Query<ClaimQuery>,
) -> ApiResult<Json<Vec<ClaimDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &kuben_core::authz::ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let claims = tenant.claims(q.all).await?;
    Ok(Json(claims.into_iter().map(ClaimDto::from).collect()))
}

/// Claim a domain; verify it next.
#[utoipa::path(
    post, path = "/domains", operation_id = "createDomainClaim", tag = "domains",
    request_body = CreateClaim,
    responses(
        (status = 201, body = ClaimDto),
        (status = 409, body = crate::error::Problem, description = "Claimed already, here or elsewhere"),
        (status = 422, body = crate::error::Problem),
    )
)]
pub async fn create(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateClaim>,
) -> ApiResult<(StatusCode, Json<ClaimDto>)> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &kuben_core::authz::ScopeChain::org(org))?;
    let name = domain::canonical(&body.domain).map_err(|e| Error::Validation(e.to_string()))?;
    if state
        .store
        .domain_owner(&name)
        .await?
        .is_some_and(|(_, owner)| owner != org)
    {
        return Err(Error::Conflict(format!("`{name}` is claimed by another organization")).into());
    }
    let id = Uuid::now_v7();
    let token = format!("kuben-{}", random_token(32)?);
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(org).await?;
    tenant
        .create_claim(id, &name, &token, &actor)
        .await
        .map_err(|e| request::duplicate(e, &format!("a claim on `{name}`")))?;
    tenant
        .append_audit(request::audit(&authz, "domain.claimed", "domain", name))
        .await?;
    let claim = tenant
        .claim(id)
        .await?
        .ok_or_else(|| Error::Internal("the new claim is missing".into()))?;
    tenant.commit().await?;
    Ok((StatusCode::CREATED, Json(ClaimDto::from(claim))))
}

/// The provider `name` of `org`, ready to call.
async fn provider(
    state: &ApiState,
    tenant: &mut Tenant,
    org: OrgId,
    name: &str,
) -> Result<(Uuid, String, Arc<dyn dns::DnsProvider>), ApiError> {
    let found = tenant
        .dns_providers()
        .await?
        .into_iter()
        .find(|p| p.name == name)
        .ok_or_else(|| Error::NotFound(format!("DNS provider `{name}`")))?;
    let (kind, sealed) = tenant
        .dns_provider_secret(found.id)
        .await?
        .ok_or_else(|| Error::NotFound(format!("DNS provider `{name}`")))?;
    let keyring = state
        .keyring
        .as_deref()
        .ok_or_else(|| Error::Unavailable("secret encryption is not configured".into()))?;
    let token = notify::open_secret(keyring, org, found.id, &sealed).map_err(Error::Internal)?;
    let token =
        String::from_utf8(token).map_err(|_| Error::Internal("a provider token is not text".into()))?;
    let api = state
        .dns
        .provider(&kind, &token)
        .ok_or_else(|| Error::Internal(format!("unknown DNS provider kind `{kind}`")))?;
    Ok((found.id, kind, api))
}

/// How claim `claim` is proven now: the method and provider, or why not.
async fn prove(
    state: &ApiState,
    tenant: &mut Tenant,
    org: OrgId,
    claim: &DomainClaim,
    with: Option<&str>,
) -> Result<Result<(String, Option<Uuid>), String>, ApiError> {
    if let Some(name) = with {
        let (id, kind, api) = provider(state, tenant, org, name).await?;
        return Ok(match api.zone_for(&claim.domain).await {
            Ok(Some(zone)) if domain::covers(&zone.name, &claim.domain) => Ok((kind, Some(id))),
            Ok(_) => Err(format!("the `{name}` account holds no zone for {}", claim.domain)),
            Err(e) => Err(e.to_string()),
        });
    }
    Ok(
        match state.dns.txt(&domain::challenge_name(&claim.domain)).await {
            Ok(values) if values.iter().any(|v| v == &claim.token) => Ok(("txt".into(), None)),
            Ok(values) if values.is_empty() => {
                Err(format!("no TXT record {}", domain::challenge_name(&claim.domain)))
            }
            Ok(_) => Err(format!(
                "{} does not hold the claim's value",
                domain::challenge_name(&claim.domain)
            )),
            Err(e) => Err(e.to_string()),
        },
    )
}

/// Verify a claim through its TXT record or a DNS provider account.
#[utoipa::path(
    post, path = "/domains/{id}/verify", operation_id = "verifyDomainClaim", tag = "domains",
    params(("id" = Uuid, Path, description = "Claim id")),
    request_body = VerifyClaim,
    responses(
        (status = 200, body = ClaimDto, description = "The claim; `lastError` says why it is still pending"),
        (status = 404, body = crate::error::Problem),
        (status = 409, body = crate::error::Problem, description = "Another organization verified an overlapping domain"),
    )
)]
pub async fn verify(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
    Json(body): Json<VerifyClaim>,
) -> ApiResult<Json<ClaimDto>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &kuben_core::authz::ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let claim = tenant
        .claim(id)
        .await?
        .ok_or_else(|| Error::NotFound(format!("claim `{id}`")))?;
    if claim.status != "pending" {
        return Ok(Json(ClaimDto::from(claim)));
    }
    match prove(&state, &mut tenant, org, &claim, body.provider.as_deref()).await? {
        Ok((method, provider)) => match tenant.verify_claim(id, &method, provider).await? {
            Verified::Verified | Verified::NotPending => {
                tenant
                    .append_audit(request::audit(
                        &authz,
                        "domain.verified",
                        "domain",
                        claim.domain.clone(),
                    ))
                    .await?;
            }
            Verified::Taken(other) => {
                return Err(Error::Conflict(format!("`{other}` is verified by another organization")).into());
            }
        },
        Err(why) => tenant.claim_checked(id, Some(&why)).await?,
    }
    let claim = tenant
        .claim(id)
        .await?
        .ok_or_else(|| Error::Internal("the claim is missing".into()))?;
    tenant.commit().await?;
    Ok(Json(ClaimDto::from(claim)))
}

/// Revoke a claim: its apps keep their domains, but nothing protects them.
#[utoipa::path(
    delete, path = "/domains/{id}", operation_id = "revokeDomainClaim", tag = "domains",
    params(("id" = Uuid, Path, description = "Claim id")),
    responses((status = 204, description = "Revoked"), (status = 404, body = crate::error::Problem))
)]
pub async fn revoke(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &kuben_core::authz::ScopeChain::org(org))?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(org).await?;
    let claim = tenant
        .claim(id)
        .await?
        .ok_or_else(|| Error::NotFound(format!("claim `{id}`")))?;
    if !tenant.revoke_claim(id, &actor).await? {
        return Err(Error::NotFound(format!("open claim `{id}`")).into());
    }
    tenant
        .append_audit(request::audit(&authz, "domain.revoked", "domain", claim.domain))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// The organization's DNS provider accounts.
#[utoipa::path(
    get, path = "/dns-providers", operation_id = "listDnsProviders", tag = "domains",
    responses((status = 200, body = Vec<DnsProviderDto>))
)]
pub async fn list_providers(
    State(state): State<ApiState>,
    authz: Authz,
) -> ApiResult<Json<Vec<DnsProviderDto>>> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgRead, &kuben_core::authz::ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    let found = tenant.dns_providers().await?;
    Ok(Json(found.into_iter().map(DnsProviderDto::from).collect()))
}

/// Add a DNS provider account; its token is checked first and kept sealed.
#[utoipa::path(
    post, path = "/dns-providers", operation_id = "createDnsProvider", tag = "domains",
    request_body = CreateDnsProvider,
    responses(
        (status = 201, body = DnsProviderDto),
        (status = 409, body = crate::error::Problem),
        (status = 422, body = crate::error::Problem, description = "Unknown kind, or the provider refused the token"),
    )
)]
pub async fn create_provider(
    State(state): State<ApiState>,
    authz: Authz,
    Json(body): Json<CreateDnsProvider>,
) -> ApiResult<(StatusCode, Json<DnsProviderDto>)> {
    authz.forbid_token()?;
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &kuben_core::authz::ScopeChain::org(org))?;
    super::validate::dns_label("name", &body.name, 32)?;
    if body.token.trim().is_empty() || body.token.len() > 512 {
        return Err(Error::Validation("token must be 1 to 512 characters".into()).into());
    }
    let api = state
        .dns
        .provider(&body.kind, body.token.trim())
        .ok_or_else(|| Error::Validation(format!("unknown DNS provider kind `{}`", body.kind)))?;
    api.verify().await.map_err(|e| match e {
        DnsError::Unavailable(why) => Error::Unavailable(why),
        other => Error::Validation(other.to_string()),
    })?;
    let keyring = state
        .keyring
        .as_deref()
        .ok_or_else(|| Error::Unavailable("secret encryption is not configured".into()))?;
    let id = Uuid::now_v7();
    let sealed =
        notify::seal_secret(keyring, org, id, body.token.trim().as_bytes()).map_err(Error::Internal)?;
    let (_, actor) = request::actor(&authz);
    let mut tenant = state.store.tenant(org).await?;
    tenant
        .create_dns_provider(id, &body.name, &body.kind, &sealed, &actor)
        .await
        .map_err(|e| request::duplicate(e, &format!("DNS provider `{}`", body.name)))?;
    tenant
        .append_audit(request::audit(
            &authz,
            "dns.provider.created",
            "dns-provider",
            body.name.clone(),
        ))
        .await?;
    tenant.commit().await?;
    Ok((
        StatusCode::CREATED,
        Json(DnsProviderDto {
            id,
            name: body.name,
            kind: body.kind,
            created_by: actor,
            created_at: request::timestamp(now_ms()),
        }),
    ))
}

/// Remove a DNS provider account (its records stay at the provider).
#[utoipa::path(
    delete, path = "/dns-providers/{id}", operation_id = "deleteDnsProvider", tag = "domains",
    params(("id" = Uuid, Path, description = "Provider id")),
    responses((status = 204, description = "Removed"), (status = 404, body = crate::error::Problem))
)]
pub async fn delete_provider(
    State(state): State<ApiState>,
    authz: Authz,
    Path(id): Path<Uuid>,
) -> ApiResult<StatusCode> {
    let org = org_of(&authz)?;
    let _proof = authz.require(&state, Perm::OrgAdmin, &kuben_core::authz::ScopeChain::org(org))?;
    let mut tenant = state.store.tenant(org).await?;
    if !tenant.delete_dns_provider(id).await? {
        return Err(Error::NotFound(format!("DNS provider `{id}`")).into());
    }
    tenant
        .append_audit(request::audit(
            &authz,
            "dns.provider.deleted",
            "dns-provider",
            id.to_string(),
        ))
        .await?;
    tenant.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// The addresses records point at: the Gateway's public ones.
async fn gateway_addresses(state: &ApiState) -> Result<Vec<IpAddr>, ApiError> {
    let client = scope::cluster(state)?;
    let platform = kuben_platform::doctor::read_platform(&client).await;
    Ok(kuben_platform::doctor::read_gateway(&client, &platform)
        .await
        .addresses()
        .to_vec())
}

fn change_dto(
    host: &str,
    action: &str,
    spec: Option<&dns::RecordSpec>,
    detail: Option<String>,
) -> DnsChangeDto {
    DnsChangeDto {
        host: host.to_owned(),
        record_type: spec.map(|s| s.record_type.clone()),
        content: spec.map(|s| s.content.clone()),
        action: action.to_owned(),
        detail,
    }
}

/// Carry out `changes` for `host` through `api`, recording what was
/// written.
async fn apply_changes(
    tenant: &mut Tenant,
    (provider, api, zone): (Uuid, &dyn dns::DnsProvider, &dns::Zone),
    target: kuben_core::ids::TargetId,
    host: &str,
    changes: Vec<Change>,
    tag: &str,
) -> Result<Vec<DnsChangeDto>, ApiError> {
    let known = tenant.dns_records(target).await?;
    let mut out = Vec::new();
    for change in changes {
        let (action, spec, written) = match change {
            Change::Create(spec) => (
                "created",
                Some(spec.clone()),
                api.create(zone, &spec, tag).await.map(Some),
            ),
            Change::Update { id, spec } => (
                "updated",
                Some(spec.clone()),
                api.update(zone, &id, &spec, tag).await.map(Some),
            ),
            Change::Unchanged { id, spec } => {
                let record = dns::ProviderRecord {
                    id,
                    name: spec.name.clone(),
                    record_type: spec.record_type.clone(),
                    content: spec.content.clone(),
                    proxied: false,
                    comment: Some(tag.to_owned()),
                };
                ("unchanged", Some(spec), Ok(Some(record)))
            }
            Change::Delete { id, .. } => {
                let deleted = api.delete(zone, &id).await;
                if deleted.is_ok() {
                    for r in known
                        .iter()
                        .filter(|r| r.provider_ref.as_deref() == Some(id.as_str()))
                    {
                        tenant.forget_dns_record(r.id).await?;
                    }
                }
                ("deleted", None, deleted.map(|()| None))
            }
            Change::Conflict(what) => {
                out.push(change_dto(
                    host,
                    "conflict",
                    None,
                    Some(DnsError::Conflict(what).to_string()),
                ));
                continue;
            }
        };
        match written {
            Ok(Some(record)) => {
                tenant
                    .record_dns(&NewDnsRecord {
                        provider,
                        target: Some(target),
                        name: &record.name,
                        record_type: &record.record_type,
                        content: &record.content,
                        zone_id: &zone.id,
                        provider_ref: &record.id,
                    })
                    .await?;
                out.push(change_dto(host, action, spec.as_ref(), None));
            }
            Ok(None) => out.push(change_dto(host, action, spec.as_ref(), None)),
            Err(e) => out.push(change_dto(host, "failed", spec.as_ref(), Some(e.to_string()))),
        }
    }
    Ok(out)
}

fn app_domain_hosts(app: &kuben_store::repo::AppRecord) -> Vec<String> {
    desired_spec(app)
        .map(|spec| spec.domains.into_iter().map(|d| d.host).collect())
        .or_else(|| {
            app.config.as_ref().and_then(|c| {
                c.get("domains").and_then(|d| d.as_array()).map(|arr| {
                    arr.iter()
                        .filter_map(|d| d.get("host").and_then(|h| h.as_str()).map(ToOwned::to_owned))
                        .collect()
                })
            })
        })
        .unwrap_or_default()
}

/// Write an app's DNS records: every custom domain the organization
/// verified points at the Gateway (or `domains.cname_target`).
#[utoipa::path(
    post,
    path = "/projects/{project}/environments/{environment}/apps/{app}/dns", operation_id = "syncAppDns",
    tag = "domains",
    params(
        ("project" = String, Path, description = "Project name"),
        ("environment" = String, Path, description = "Environment short name"),
        ("app" = String, Path, description = "App name"),
    ),
    request_body = SyncDns,
    responses((status = 200, body = Vec<DnsChangeDto>), (status = 404, body = crate::error::Problem))
)]
pub async fn sync_app(
    State(state): State<ApiState>,
    authz: Authz,
    Path((project, environment, app)): Path<(String, String, String)>,
    Json(body): Json<SyncDns>,
) -> ApiResult<Json<Vec<DnsChangeDto>>> {
    let a = scope::app(&state, &authz, &project, &environment, &app).await?;
    let _proof = authz.require(&state, Perm::AppWrite, &a.chain())?;
    let org = a.env.project.org;
    let hosts = app_domain_hosts(&a.app);
    let mut tenant = state.store.tenant(org).await?;
    let (provider_id, _, api) = provider(&state, &mut tenant, org, &body.provider).await?;
    let addresses = match state.cfg.domains.cname_target {
        Some(_) => Vec::new(),
        None => gateway_addresses(&state).await?,
    };
    let tag = format!("kuben:{org}");
    let mut out = Vec::new();
    for host in hosts {
        let Ok(host) = domain::canonical(&host) else {
            continue;
        };
        if !matches!(state.store.domain_owner(&host).await?, Some((_, owner)) if owner == org) {
            out.push(change_dto(
                &host,
                "skipped",
                None,
                Some("the domain is not verified for this organization".into()),
            ));
            continue;
        }
        let wanted = dns::gateway_records(&host, &addresses, state.cfg.domains.cname_target.as_deref());
        if wanted.is_empty() {
            out.push(change_dto(
                &host,
                "skipped",
                None,
                Some("the Gateway has no public address yet".into()),
            ));
            continue;
        }
        let zone = match api.zone_for(&host).await {
            Ok(Some(zone)) => zone,
            Ok(None) => {
                out.push(change_dto(
                    &host,
                    "skipped",
                    None,
                    Some("the provider holds no zone for it".into()),
                ));
                continue;
            }
            Err(e) => {
                out.push(change_dto(&host, "failed", None, Some(e.to_string())));
                continue;
            }
        };
        let existing = match api.records(&zone, &host).await {
            Ok(records) => records,
            Err(e) => {
                out.push(change_dto(&host, "failed", None, Some(e.to_string())));
                continue;
            }
        };
        let known: Vec<String> = tenant
            .dns_records(a.app.target)
            .await?
            .into_iter()
            .filter(|r| r.name == host)
            .filter_map(|r| r.provider_ref)
            .collect();
        let changes = dns::plan(&wanted, &existing, &known, &tag);
        out.extend(
            apply_changes(
                &mut tenant,
                (provider_id, api.as_ref(), &zone),
                a.app.target,
                &host,
                changes,
                &tag,
            )
            .await?,
        );
    }
    tenant
        .append_audit(request::audit(
            &authz,
            "app.dns.synced",
            "app",
            format!("{project}/{environment}/{app}"),
        ))
        .await?;
    tenant.commit().await?;
    Ok(Json(out))
}

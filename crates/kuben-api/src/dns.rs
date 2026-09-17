//! DNS for domain claims (M5.2; plan §14.3): lookups over DNS-over-HTTPS
//! and the first provider adapter, Cloudflare.
//!
//! Lookups go to a DNS-over-HTTPS resolver (`domains.doh_url`), so a claim
//! is checked against the public DNS, not the cluster's view of it.
//!
//! The provider adapter writes A, AAAA, CNAME and TXT records. It is careful
//! with what it did not write:
//! - Every record it creates carries the comment `kuben:<org>`.
//! - A record of the same name and type without that comment is someone
//!   else's, and is reported as a conflict instead of being overwritten.
//! - A record is deleted only when the provider still reports the id Kuben
//!   recorded for it.
//! - A create whose answer was lost is found again by name, type and
//!   comment on the next pass.

use std::{sync::Arc, time::Duration};

use bytes::Bytes;
use http::{Method, Request, StatusCode, header};
use http_body_util::Full;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::transport::{Schemes, Transport};

const TIMEOUT: Duration = Duration::from_secs(10);
const MAX_BODY: usize = 1 << 20;
/// DNS record types by number, as DNS-over-HTTPS answers name them.
const TYPE_NS: u64 = 2;
const TYPE_TXT: u64 = 16;

/// Why a DNS operation failed.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum DnsError {
    /// The provider refused the credentials.
    #[error("the DNS provider refused the credentials: {0}")]
    Refused(String),
    /// A record of that name and type exists and is not Kuben's.
    #[error("{0} exists and was not written by Kuben; remove or change it first")]
    Conflict(String),
    #[error("DNS is unavailable: {0}")]
    Unavailable(String),
}

/// A DNS-over-HTTPS resolver (the JSON API of Cloudflare and Google).
#[derive(Clone, Debug)]
pub struct Resolver {
    transport: Transport,
    url: String,
}

#[derive(Deserialize)]
struct DohAnswer {
    #[serde(rename = "type")]
    kind: u64,
    data: String,
}

#[derive(Deserialize)]
struct DohBody {
    #[serde(rename = "Status")]
    status: u64,
    #[serde(rename = "Answer", default)]
    answer: Vec<DohAnswer>,
}

/// The answers of type `kind` in a DNS-over-HTTPS JSON body. A name that
/// does not exist (NXDOMAIN) has none.
pub fn doh_answers(body: &[u8], kind: u64) -> Result<Vec<String>, DnsError> {
    let body: DohBody =
        serde_json::from_slice(body).map_err(|e| DnsError::Unavailable(format!("unexpected answer: {e}")))?;
    match body.status {
        0 | 3 => {}
        other => {
            return Err(DnsError::Unavailable(format!(
                "the resolver answered rcode {other}"
            )));
        }
    }
    Ok(body
        .answer
        .into_iter()
        .filter(|a| a.kind == kind)
        .map(|a| {
            let data = a.data.trim();
            if kind == TYPE_TXT {
                // `"part one" "part two"` → `part onepart two`.
                data.split('"')
                    .enumerate()
                    .filter(|(i, _)| i % 2 == 1)
                    .map(|(_, part)| part)
                    .collect::<String>()
            } else {
                data.trim_end_matches('.').to_ascii_lowercase()
            }
        })
        .collect())
}

impl Resolver {
    #[must_use]
    pub fn new(url: &str) -> Self {
        Self {
            transport: Transport::new(Schemes::HttpsOnly, TIMEOUT),
            url: url.trim_end_matches('/').to_owned(),
        }
    }

    async fn query(&self, name: &str, kind: u64) -> Result<Vec<String>, DnsError> {
        let uri = format!("{}?name={name}&type={kind}", self.url);
        let request = Request::get(uri)
            .header(header::ACCEPT, "application/dns-json")
            .body(Full::new(Bytes::new()))
            .map_err(|e| DnsError::Unavailable(e.to_string()))?;
        let (status, body) = self
            .transport
            .fetch(request, MAX_BODY)
            .await
            .map_err(DnsError::Unavailable)?;
        if !status.is_success() {
            return Err(DnsError::Unavailable(format!(
                "the resolver answered HTTP {status}"
            )));
        }
        doh_answers(&body, kind)
    }

    /// The TXT values of `name`.
    pub async fn txt(&self, name: &str) -> Result<Vec<String>, DnsError> {
        self.query(name, TYPE_TXT).await
    }

    /// The name servers of `name`.
    pub async fn ns(&self, name: &str) -> Result<Vec<String>, DnsError> {
        self.query(name, TYPE_NS).await
    }
}

/// A DNS zone at a provider.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct Zone {
    pub id: String,
    pub name: String,
    /// The name servers the provider assigned to the zone.
    #[serde(default)]
    pub name_servers: Vec<String>,
}

/// A record as the provider reports it.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct ProviderRecord {
    pub id: String,
    pub name: String,
    #[serde(rename = "type")]
    pub record_type: String,
    pub content: String,
    #[serde(default)]
    pub proxied: bool,
    #[serde(default)]
    pub comment: Option<String>,
}

/// A record Kuben wants.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RecordSpec {
    pub name: String,
    #[serde(rename = "type")]
    pub record_type: String,
    pub content: String,
}

/// What one pass does to one record.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Change {
    Create(RecordSpec),
    Update {
        id: String,
        spec: RecordSpec,
    },
    /// A record Kuben wrote and no longer wants.
    Delete {
        id: String,
        name: String,
        record_type: String,
    },
    Unchanged {
        id: String,
        spec: RecordSpec,
    },
    /// Someone else's record stands in the way.
    Conflict(String),
}

/// What makes `existing` (the provider's records of the wanted names) look
/// like `wanted`. Only records tagged `tag` are updated or deleted, and a
/// deletion only of an id in `known` (the ids Kuben recorded).
#[must_use]
pub fn plan(wanted: &[RecordSpec], existing: &[ProviderRecord], known: &[String], tag: &str) -> Vec<Change> {
    let ours = |r: &ProviderRecord| r.comment.as_deref() == Some(tag);
    let mut changes = Vec::new();
    let mut used: Vec<&str> = Vec::new();
    for spec in wanted {
        let same_kind: Vec<&ProviderRecord> = existing
            .iter()
            .filter(|r| r.name.eq_ignore_ascii_case(&spec.name) && r.record_type == spec.record_type)
            .collect();
        if let Some(r) = same_kind.iter().find(|r| ours(r) && r.content == spec.content) {
            used.push(&r.id);
            changes.push(Change::Unchanged {
                id: r.id.clone(),
                spec: spec.clone(),
            });
        } else if let Some(r) = same_kind
            .iter()
            .find(|r| ours(r) && !used.contains(&r.id.as_str()))
        {
            used.push(&r.id);
            changes.push(Change::Update {
                id: r.id.clone(),
                spec: spec.clone(),
            });
        } else if same_kind.iter().any(|r| !ours(r))
            || (spec.record_type == "CNAME"
                && existing
                    .iter()
                    .any(|r| r.name.eq_ignore_ascii_case(&spec.name) && !ours(r)))
        {
            changes.push(Change::Conflict(format!("{} {}", spec.record_type, spec.name)));
        } else {
            changes.push(Change::Create(spec.clone()));
        }
    }
    for r in existing {
        if ours(r) && !used.contains(&r.id.as_str()) && known.contains(&r.id) {
            changes.push(Change::Delete {
                id: r.id.clone(),
                name: r.name.clone(),
                record_type: r.record_type.clone(),
            });
        }
    }
    changes
}

/// A DNS provider account, as far as Kuben needs it.
#[async_trait::async_trait]
pub trait DnsProvider: Send + Sync + std::fmt::Debug {
    /// Check the credentials.
    async fn verify(&self) -> Result<(), DnsError>;
    /// The zone holding `name`, if the account has one.
    async fn zone_for(&self, name: &str) -> Result<Option<Zone>, DnsError>;
    /// The records named `name` in `zone`.
    async fn records(&self, zone: &Zone, name: &str) -> Result<Vec<ProviderRecord>, DnsError>;
    async fn create(&self, zone: &Zone, spec: &RecordSpec, tag: &str) -> Result<ProviderRecord, DnsError>;
    async fn update(
        &self,
        zone: &Zone,
        id: &str,
        spec: &RecordSpec,
        tag: &str,
    ) -> Result<ProviderRecord, DnsError>;
    async fn delete(&self, zone: &Zone, id: &str) -> Result<(), DnsError>;
}

/// The Cloudflare API with an API token.
#[derive(Clone)]
pub struct Cloudflare {
    transport: Transport,
    api: String,
    token: Arc<str>,
}

impl std::fmt::Debug for Cloudflare {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Cloudflare")
            .field("api", &self.api)
            .finish_non_exhaustive()
    }
}

#[derive(Deserialize)]
struct Envelope<T> {
    success: bool,
    #[serde(default)]
    errors: Vec<Value>,
    result: Option<T>,
}

impl Cloudflare {
    #[must_use]
    pub fn new(api: &str, token: &str) -> Self {
        let schemes = if api.starts_with("http://") {
            Schemes::Any
        } else {
            Schemes::HttpsOnly
        };
        Self {
            transport: Transport::new(schemes, TIMEOUT),
            api: api.trim_end_matches('/').to_owned(),
            token: Arc::from(token),
        }
    }

    async fn call<T: for<'de> Deserialize<'de>>(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<T, DnsError> {
        let payload = body.map_or_else(Bytes::new, |b| Bytes::from(b.to_string()));
        let request = Request::builder()
            .method(method)
            .uri(format!("{}{path}", self.api))
            .header(header::AUTHORIZATION, format!("Bearer {}", self.token))
            .header(header::CONTENT_TYPE, "application/json")
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")))
            .body(Full::new(payload))
            .map_err(|e| DnsError::Unavailable(e.to_string()))?;
        let (status, body) = self
            .transport
            .fetch(request, MAX_BODY)
            .await
            .map_err(DnsError::Unavailable)?;
        if matches!(status, StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN) {
            return Err(DnsError::Refused(format!("HTTP {status}")));
        }
        let envelope: Envelope<T> = serde_json::from_slice(&body)
            .map_err(|e| DnsError::Unavailable(format!("HTTP {status}: unexpected answer: {e}")))?;
        match envelope.result {
            Some(result) if envelope.success => Ok(result),
            _ => Err(DnsError::Unavailable(format!(
                "HTTP {status}: {}",
                Value::Array(envelope.errors)
            ))),
        }
    }
}

fn query_value(value: &str) -> String {
    value
        .bytes()
        .map(|b| match b {
            b'a'..=b'z' | b'A'..=b'Z' | b'0'..=b'9' | b'-' | b'.' | b'_' => (b as char).to_string(),
            other => format!("%{other:02X}"),
        })
        .collect()
}

#[async_trait::async_trait]
impl DnsProvider for Cloudflare {
    async fn verify(&self) -> Result<(), DnsError> {
        #[derive(Deserialize)]
        struct TokenStatus {
            status: String,
        }
        let token: TokenStatus = self.call(Method::GET, "/user/tokens/verify", None).await?;
        if token.status == "active" {
            Ok(())
        } else {
            Err(DnsError::Refused(format!("the token is {}", token.status)))
        }
    }

    async fn zone_for(&self, name: &str) -> Result<Option<Zone>, DnsError> {
        for candidate in kuben_core::domain::ancestors(name) {
            let zones: Vec<Zone> = self
                .call(
                    Method::GET,
                    &format!("/zones?name={}", query_value(candidate)),
                    None,
                )
                .await?;
            if let Some(zone) = zones.into_iter().next() {
                return Ok(Some(zone));
            }
        }
        Ok(None)
    }

    async fn records(&self, zone: &Zone, name: &str) -> Result<Vec<ProviderRecord>, DnsError> {
        self.call(
            Method::GET,
            &format!(
                "/zones/{}/dns_records?name={}&per_page=100",
                query_value(&zone.id),
                query_value(name)
            ),
            None,
        )
        .await
    }

    async fn create(&self, zone: &Zone, spec: &RecordSpec, tag: &str) -> Result<ProviderRecord, DnsError> {
        let body = json!({ "type": spec.record_type, "name": spec.name, "content": spec.content,
                           "ttl": 1, "proxied": false, "comment": tag });
        self.call(
            Method::POST,
            &format!("/zones/{}/dns_records", query_value(&zone.id)),
            Some(body),
        )
        .await
    }

    async fn update(
        &self,
        zone: &Zone,
        id: &str,
        spec: &RecordSpec,
        tag: &str,
    ) -> Result<ProviderRecord, DnsError> {
        let body = json!({ "type": spec.record_type, "name": spec.name, "content": spec.content,
                           "ttl": 1, "proxied": false, "comment": tag });
        self.call(
            Method::PUT,
            &format!("/zones/{}/dns_records/{}", query_value(&zone.id), query_value(id)),
            Some(body),
        )
        .await
    }

    async fn delete(&self, zone: &Zone, id: &str) -> Result<(), DnsError> {
        let _: Value = self
            .call(
                Method::DELETE,
                &format!("/zones/{}/dns_records/{}", query_value(&zone.id), query_value(id)),
                None,
            )
            .await?;
        Ok(())
    }
}

/// Where lookups and provider accounts come from; tests replace it.
#[async_trait::async_trait]
pub trait DnsBackend: Send + Sync + std::fmt::Debug {
    /// The TXT values of `name`.
    async fn txt(&self, name: &str) -> Result<Vec<String>, DnsError>;
    /// The name servers of `name`.
    async fn ns(&self, name: &str) -> Result<Vec<String>, DnsError>;
    /// The provider of `kind` with `token`; `None` for an unknown kind.
    fn provider(&self, kind: &str, token: &str) -> Option<Arc<dyn DnsProvider>>;
}

/// The public DNS, through DNS-over-HTTPS, and the real providers.
#[derive(Clone, Debug)]
pub struct PublicDns {
    resolver: Resolver,
    cloudflare_api: String,
}

impl PublicDns {
    #[must_use]
    pub fn new(cfg: &kuben_core::config::DomainsCfg) -> Self {
        Self {
            resolver: Resolver::new(&cfg.doh_url),
            cloudflare_api: cfg.cloudflare_api_url.clone(),
        }
    }
}

#[async_trait::async_trait]
impl DnsBackend for PublicDns {
    async fn txt(&self, name: &str) -> Result<Vec<String>, DnsError> {
        self.resolver.txt(name).await
    }

    async fn ns(&self, name: &str) -> Result<Vec<String>, DnsError> {
        self.resolver.ns(name).await
    }

    fn provider(&self, kind: &str, token: &str) -> Option<Arc<dyn DnsProvider>> {
        match kind {
            "cloudflare" => Some(Arc::new(Cloudflare::new(&self.cloudflare_api, token))),
            _ => None,
        }
    }
}

/// The records pointing `host` at a gateway: a CNAME to `cname` when set,
/// else A and AAAA records of `addresses`.
#[must_use]
pub fn gateway_records(host: &str, addresses: &[std::net::IpAddr], cname: Option<&str>) -> Vec<RecordSpec> {
    if let Some(target) = cname {
        return vec![RecordSpec {
            name: host.to_owned(),
            record_type: "CNAME".into(),
            content: target.trim_end_matches('.').to_owned(),
        }];
    }
    let mut out: Vec<RecordSpec> = addresses
        .iter()
        .filter(|a| !crate::notify::is_private(**a))
        .map(|a| RecordSpec {
            name: host.to_owned(),
            record_type: if a.is_ipv4() { "A" } else { "AAAA" }.into(),
            content: a.to_string(),
        })
        .collect();
    out.sort_by(|a, b| (&a.record_type, &a.content).cmp(&(&b.record_type, &b.content)));
    out.dedup();
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn record(id: &str, kind: &str, content: &str, comment: Option<&str>) -> ProviderRecord {
        ProviderRecord {
            id: id.into(),
            name: "shop.example.com".into(),
            record_type: kind.into(),
            content: content.into(),
            proxied: false,
            comment: comment.map(str::to_owned),
        }
    }

    fn spec(kind: &str, content: &str) -> RecordSpec {
        RecordSpec {
            name: "shop.example.com".into(),
            record_type: kind.into(),
            content: content.into(),
        }
    }

    #[test]
    fn doh_answers_are_read() {
        let body = br#"{"Status":0,"Answer":[{"name":"x.","type":16,"data":"\"abc\" \"def\""},{"type":5,"data":"y."}]}"#;
        assert_eq!(doh_answers(body, TYPE_TXT), Ok(vec!["abcdef".to_owned()]));
        let ns = br#"{"Status":0,"Answer":[{"type":2,"data":"Ada.NS.cloudflare.com."}]}"#;
        assert_eq!(
            doh_answers(ns, TYPE_NS),
            Ok(vec!["ada.ns.cloudflare.com".to_owned()])
        );
        assert_eq!(
            doh_answers(br#"{"Status":3}"#, TYPE_TXT),
            Ok(vec![]),
            "no such name"
        );
        assert!(
            doh_answers(br#"{"Status":2}"#, TYPE_TXT).is_err(),
            "server failure"
        );
        assert!(doh_answers(b"<html>", TYPE_TXT).is_err());
    }

    #[test]
    fn plans_never_touch_records_of_others() {
        let tag = "kuben:org";
        let wanted = [spec("A", "203.0.113.10")];
        assert_eq!(
            plan(&wanted, &[], &[], tag),
            [Change::Create(spec("A", "203.0.113.10"))]
        );
        let theirs = [record("1", "A", "198.51.100.1", None)];
        assert_eq!(
            plan(&wanted, &theirs, &[], tag),
            [Change::Conflict("A shop.example.com".into())]
        );
        let ours = [record("2", "A", "198.51.100.1", Some(tag))];
        assert_eq!(
            plan(&wanted, &ours, &["2".into()], tag),
            [Change::Update {
                id: "2".into(),
                spec: spec("A", "203.0.113.10")
            }]
        );
        let same = [record("3", "A", "203.0.113.10", Some(tag))];
        assert_eq!(
            plan(&wanted, &same, &[], tag),
            [Change::Unchanged {
                id: "3".into(),
                spec: spec("A", "203.0.113.10")
            }]
        );
        let cname_blocked = [record("4", "A", "198.51.100.1", None)];
        assert_eq!(
            plan(&[spec("CNAME", "gw.example.net")], &cname_blocked, &[], tag),
            [Change::Conflict("CNAME shop.example.com".into())]
        );
    }

    #[test]
    fn only_recorded_records_are_deleted() {
        let tag = "kuben:org";
        let existing = [
            record("5", "AAAA", "2001:db8::1", Some(tag)),
            record("6", "AAAA", "2001:db8::2", Some(tag)),
            record("7", "TXT", "x", None),
        ];
        let changes = plan(&[], &existing, &["5".into()], tag);
        assert_eq!(
            changes,
            [Change::Delete {
                id: "5".into(),
                name: "shop.example.com".into(),
                record_type: "AAAA".into()
            }]
        );
    }

    #[test]
    fn gateways_are_pointed_at_by_address_or_name() {
        let addresses: Vec<std::net::IpAddr> = vec![
            "203.0.113.10".parse().expect("ip"),
            "10.0.0.1".parse().expect("ip"),
            "2001:db8::1".parse().expect("ip"),
        ];
        let records = gateway_records("shop.example.com", &addresses, None);
        assert_eq!(
            records
                .iter()
                .map(|r| (r.record_type.as_str(), r.content.as_str()))
                .collect::<Vec<_>>(),
            [("A", "203.0.113.10"), ("AAAA", "2001:db8::1")],
            "private addresses are never published"
        );
        let cname = gateway_records("shop.example.com", &addresses, Some("lb.example.net."));
        assert_eq!(
            cname,
            [RecordSpec {
                name: "shop.example.com".into(),
                record_type: "CNAME".into(),
                content: "lb.example.net".into()
            }]
        );
        assert_eq!(query_value("a b&c"), "a%20b%26c");
    }
}

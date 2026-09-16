//! Doctor (M2.13): why an app is or is not reachable, as a list of checks.
//!
//! The functions here only judge what the caller observed (discovery facts,
//! the Gateway, the route and its certificates, DNS, ports, the agent), so
//! the API and the CLI give the same verdicts. A check that could not be
//! made is `unknown`, and a report with an unknown check is never `ok`.

use std::{net::IpAddr, time::Duration};

use kube::{
    Api, Client,
    api::{ApiResource, DynamicObject, GroupVersionKind},
};
use kuben_crd::KubenConfig;
use serde::Serialize;

use crate::{
    controller::{KUBEN_CONFIG_NAME, resources::Platform},
    discovery::{Availability, ClusterFacts},
    projection::ExposureView,
};

/// The verdict of one check, from best to worst for a report.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Status {
    Ok,
    Warn,
    /// Could not be checked; never counts as fine.
    Unknown,
    Fail,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Check {
    /// Stable id: `gateway-class`, `gateway`, `issuer`, `port-80`, `route`,
    /// `certificate`, `dns`, `agent`.
    pub id: &'static str,
    /// What was checked, e.g. the host.
    pub subject: String,
    pub status: Status,
    pub detail: String,
    /// What to do about it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hint: Option<String>,
}

impl Check {
    fn new(id: &'static str, subject: impl Into<String>, status: Status, detail: impl Into<String>) -> Self {
        Self {
            id,
            subject: subject.into(),
            status,
            detail: detail.into(),
            hint: None,
        }
    }

    #[must_use]
    fn hint(mut self, hint: impl Into<String>) -> Self {
        if self.status != Status::Ok {
            self.hint = Some(hint.into());
        }
        self
    }
}

/// The worst verdict of `checks` (`ok` for none).
#[must_use]
pub fn overall(checks: &[Check]) -> Status {
    checks.iter().map(|c| c.status).max().unwrap_or(Status::Ok)
}

fn availability(
    id: &'static str,
    subject: &str,
    found: &Availability,
    facts_known: bool,
    ready: &str,
) -> Check {
    match (facts_known, found) {
        (false, _) | (true, Availability::Unknown) => {
            Check::new(id, subject, Status::Unknown, "the cluster could not be asked yet")
        }
        (true, Availability::Ready) => Check::new(id, subject, Status::Ok, ready),
        (true, Availability::NotReady(why)) if why.is_empty() => {
            Check::new(id, subject, Status::Fail, "exists but is not ready")
        }
        (true, Availability::NotReady(why)) => Check::new(id, subject, Status::Fail, why.clone()),
        (true, Availability::Missing) => Check::new(id, subject, Status::Fail, "does not exist"),
    }
}

/// What was read of Kuben's Gateway.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub enum GatewayState {
    /// It could not be read.
    #[default]
    Unreadable,
    Missing,
    Found {
        /// Its `Programmed` condition, once its controller answered.
        programmed: Option<bool>,
        message: Option<String>,
        addresses: Vec<IpAddr>,
    },
}

/// The platform an app is exposed through: its GatewayClass, the Gateway,
/// and the issuer of its certificates.
#[must_use]
pub fn platform_checks(
    facts: Option<&ClusterFacts>,
    platform: &Platform,
    gateway: &GatewayState,
) -> Vec<Check> {
    let mut checks = Vec::new();
    let Some(gw) = &platform.gateway else {
        checks.push(
            Check::new(
                "gateway",
                "",
                Status::Fail,
                "no Gateway is configured: apps get no public address",
            )
            .hint("set KubenConfig spec.gatewayClassName (kuben setup does on its own k3s)"),
        );
        return checks;
    };
    if let Some(class) = &platform.gateway_class {
        let found = facts.map_or(Availability::Unknown, |f| f.gateway_class(class));
        checks.push(
            availability("gateway-class", class, &found, facts.is_some(), "accepted by its controller")
                .hint("install the Gateway controller for this class (k3s: Traefik with the Gateway provider; elsewhere e.g. Envoy Gateway)"),
        );
    }
    let name = format!("{}/{}", gw.namespace, gw.name);
    checks.push(match gateway {
        GatewayState::Unreadable => Check::new("gateway", name, Status::Unknown, "could not be read"),
        GatewayState::Missing => Check::new("gateway", name, Status::Fail, "does not exist")
            .hint("Kuben creates it once a GatewayClass is set; see the Gateway condition of KubenConfig"),
        GatewayState::Found {
            programmed: Some(true),
            addresses,
            ..
        } => {
            let at = if addresses.is_empty() {
                "no address reported".to_owned()
            } else {
                join(addresses)
            };
            Check::new("gateway", name, Status::Ok, format!("programmed ({at})"))
        }
        GatewayState::Found {
            programmed: Some(false),
            message,
            ..
        } => Check::new(
            "gateway",
            name,
            Status::Fail,
            message.clone().unwrap_or_else(|| "not programmed".into()),
        )
        .hint("the Gateway controller's log says why"),
        GatewayState::Found { programmed: None, .. } => Check::new(
            "gateway",
            name,
            Status::Unknown,
            "no controller has answered for it yet",
        ),
    });
    match &platform.cluster_issuer {
        Some(issuer) => {
            let found = facts.map_or(Availability::Unknown, |f| f.issuer(issuer));
            checks.push(
                availability("issuer", issuer, &found, facts.is_some(), "ready")
                    .hint("kubectl describe clusterissuer shows why (an ACME account, a CA Secret)"),
            );
        }
        None => checks.push(
            Check::new(
                "issuer",
                "",
                Status::Warn,
                "no ClusterIssuer: apps are served over plain HTTP",
            )
            .hint("kuben setup --acme-email you@example.com, or KubenConfig spec.clusterIssuer"),
        ),
    }
    checks
}

/// Whether the Gateway answered on a public port, from this server.
#[must_use]
pub fn port_check(port: u16, reached: Option<Result<(), String>>) -> Check {
    let id = if port == 80 { "port-80" } else { "port-443" };
    match reached {
        None => Check::new(
            id,
            port.to_string(),
            Status::Unknown,
            "the Gateway reports no address to try",
        ),
        Some(Ok(())) => Check::new(
            id,
            port.to_string(),
            Status::Ok,
            "the Gateway accepts connections",
        ),
        Some(Err(e)) => Check::new(id, port.to_string(), Status::Fail, e)
            .hint("another web server may hold the port, or a firewall blocks it (a cloud firewall too)"),
    }
}

fn join(ips: &[IpAddr]) -> String {
    ips.iter().map(ToString::to_string).collect::<Vec<_>>().join(", ")
}

/// What a hostname resolves to, against the Gateway's addresses: `ok`,
/// `mismatch`, `unresolved` or `unknown`, and why.
#[must_use]
pub fn dns_verdict(resolved: &[IpAddr], gateway: &[IpAddr]) -> (&'static str, String) {
    if resolved.is_empty() {
        return (
            "unresolved",
            "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway".into(),
        );
    }
    if gateway.is_empty() {
        return (
            "unknown",
            format!(
                "resolves to {}; the gateway reports no address to compare with",
                join(resolved)
            ),
        );
    }
    if resolved.iter().any(|ip| gateway.contains(ip)) {
        ("ok", "points at the gateway".into())
    } else {
        (
            "mismatch",
            format!(
                "points at {} but the gateway is {}",
                join(resolved),
                join(gateway)
            ),
        )
    }
}

/// A hostname's DNS against the Gateway's addresses.
#[must_use]
pub fn dns_check(host: &str, resolved: &[IpAddr], gateway: &[IpAddr]) -> Check {
    let (verdict, detail) = dns_verdict(resolved, gateway);
    let status = match verdict {
        "ok" => Status::Ok,
        "unknown" => Status::Unknown,
        _ => Status::Fail,
    };
    Check::new("dns", host, status, detail)
        .hint("point the record at the Gateway's address; DNS may take a while to follow")
}

/// The addresses `host` resolves to (none after three seconds).
pub async fn resolve(host: &str) -> Vec<IpAddr> {
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

/// The platform settings of the cluster's `KubenConfig` (defaults without one).
pub async fn read_platform(client: &Client) -> Platform {
    let config = Api::<KubenConfig>::all(client.clone())
        .get_opt(KUBEN_CONFIG_NAME)
        .await
        .ok()
        .flatten();
    Platform::from_spec(config.as_ref().map(|c| &c.spec))
}

/// The Gateway apps attach to, as its controller reports it.
pub async fn read_gateway(client: &Client, platform: &Platform) -> GatewayState {
    let Some(gw) = &platform.gateway else {
        return GatewayState::Missing;
    };
    let gvk = GroupVersionKind::gvk("gateway.networking.k8s.io", "v1", "Gateway");
    let api: Api<DynamicObject> = Api::namespaced_with(
        client.clone(),
        &gw.namespace,
        &ApiResource::from_gvk_with_plural(&gvk, "gateways"),
    );
    let gateway = match api.get_opt(&gw.name).await {
        Ok(Some(gateway)) => gateway,
        Ok(None) => return GatewayState::Missing,
        Err(_) => return GatewayState::Unreadable,
    };
    let status = &gateway.data["status"];
    let (programmed, message) = match crate::discovery::condition(&gateway.data, "Programmed") {
        Some((ready, message)) => (Some(ready), message),
        None => (None, None),
    };
    let mut addresses = Vec::new();
    for value in status["addresses"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|a| a["value"].as_str())
    {
        match value.parse::<IpAddr>() {
            Ok(ip) => addresses.push(ip),
            Err(_) => addresses.extend(resolve(value).await),
        }
    }
    GatewayState::Found {
        programmed,
        message,
        addresses,
    }
}

impl GatewayState {
    /// The Gateway's addresses, when it was found.
    #[must_use]
    pub fn addresses(&self) -> &[IpAddr] {
        match self {
            Self::Found { addresses, .. } => addresses,
            Self::Unreadable | Self::Missing => &[],
        }
    }
}

/// Whether the first of `addresses` accepts TCP connections on `port`;
/// `None` without an address.
pub async fn probe_port(addresses: &[IpAddr], port: u16) -> Option<Result<(), String>> {
    let ip = *addresses.first()?;
    let connect = tokio::net::TcpStream::connect((ip, port));
    Some(
        match tokio::time::timeout(Duration::from_secs(3), connect).await {
            Ok(Ok(_)) => Ok(()),
            Ok(Err(e)) => Err(format!("{ip}:{port}: {e}")),
            Err(_) => Err(format!("{ip}:{port}: no answer within 3s")),
        },
    )
}

/// The app's route and the certificate of each host.
#[must_use]
pub fn exposure_checks(exposure: Option<&ExposureView>, has_hosts: bool) -> Vec<Check> {
    let Some(exposure) = exposure else {
        return vec![if has_hosts {
            Check::new("route", "", Status::Fail, "the app has no route yet")
                .hint("the App's Exposed condition says why (no Gateway, no base domain)")
        } else {
            Check::new("route", "", Status::Warn, "the app has no public address")
                .hint("add a domain, or set a base domain for generated ones")
        }];
    };
    let mut checks = vec![match exposure.accepted {
        Some(true) => Check::new("route", "", Status::Ok, "accepted by the Gateway"),
        Some(false) => Check::new(
            "route",
            "",
            Status::Fail,
            exposure
                .message
                .clone()
                .unwrap_or_else(|| "refused by the Gateway".into()),
        )
        .hint("a host another environment holds, or a listener the Gateway lacks"),
        None => Check::new("route", "", Status::Unknown, "the Gateway has not answered yet"),
    }];
    for host in &exposure.hosts {
        let check = |status, detail: String| Check::new("certificate", host.host.clone(), status, detail);
        checks.push(match (host.tls, host.certificate_ready) {
            ("none", _) => check(Status::Warn, "served over plain HTTP".into())
                .hint("set a ClusterIssuer, or tls: auto on the domain"),
            ("secret", _) => check(Status::Unknown, "uses a certificate Secret of its own (not checked)".into()),
            (_, Some(true)) => check(Status::Ok, "issued".into()),
            (_, Some(false)) => check(
                Status::Fail,
                host.certificate_message
                    .clone()
                    .unwrap_or_else(|| "not issued yet".into()),
            )
            .hint("HTTP-01 needs DNS pointing at the Gateway and port 80 open; the certificate's events say more"),
            (_, None) => check(Status::Unknown, "no certificate of its own was found".into()),
        });
    }
    checks
}

/// What is known of the agent that delivers the app.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AgentState {
    None,
    Revoked,
    /// Seconds since it was last heard of, if ever.
    Seen(Option<u64>),
}

/// The agent of an app its cluster's agent delivers; `stale_after` is a few
/// heartbeats.
#[must_use]
pub fn agent_check(agent: &AgentState, stale_after: u64) -> Check {
    let check = |status, detail: String| Check::new("agent", "", status, detail);
    match agent {
        AgentState::None => check(Status::Fail, "the cluster has no enrolled agent".into())
            .hint("kuben agent-token --cluster <cluster>, then start the agent with it"),
        AgentState::Revoked => check(Status::Fail, "the cluster's agent was revoked".into())
            .hint("enroll a new agent with a fresh token"),
        AgentState::Seen(None) => check(Status::Fail, "the agent enrolled but never linked".into())
            .hint("check that the agent reaches Kuben's AgentLink port"),
        AgentState::Seen(Some(age)) if *age <= stale_after => {
            check(Status::Ok, format!("linked, heard from {age}s ago"))
        }
        AgentState::Seen(Some(age)) => check(Status::Fail, format!("last heard from {age}s ago"))
            .hint("the agent's log says why it lost its link"),
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::{discovery::Readiness, projection::HostExposure};

    fn platform(issuer: Option<&str>) -> Platform {
        let mut spec = json!({ "gatewayClassName": "traefik" });
        if let Some(issuer) = issuer {
            spec["clusterIssuer"] = json!(issuer);
        }
        Platform::from_spec(Some(&serde_json::from_value(spec).expect("spec")))
    }

    fn facts() -> ClusterFacts {
        ClusterFacts {
            gateway_classes: vec![Readiness {
                name: "traefik".into(),
                ready: true,
                ..Readiness::default()
            }],
            cluster_issuers: vec![Readiness {
                name: "letsencrypt".into(),
                ready: false,
                message: Some("ACME account not registered".into()),
                ..Readiness::default()
            }],
            ..ClusterFacts::default()
        }
    }

    fn by_id<'a>(checks: &'a [Check], id: &str) -> &'a Check {
        checks.iter().find(|c| c.id == id).expect("check")
    }

    #[test]
    fn the_platform_is_judged_from_facts_and_unknown_is_never_ok() {
        let found = GatewayState::Found {
            programmed: Some(true),
            message: None,
            addresses: vec!["203.0.113.7".parse().expect("ip")],
        };
        let checks = platform_checks(Some(&facts()), &platform(Some("letsencrypt")), &found);
        assert_eq!(by_id(&checks, "gateway-class").status, Status::Ok);
        assert_eq!(by_id(&checks, "gateway").status, Status::Ok);
        assert!(by_id(&checks, "gateway").detail.contains("203.0.113.7"));
        let issuer = by_id(&checks, "issuer");
        assert_eq!(
            (issuer.status, issuer.detail.as_str()),
            (Status::Fail, "ACME account not registered")
        );
        assert!(issuer.hint.is_some());
        assert_eq!(overall(&checks), Status::Fail);

        let unknown = platform_checks(None, &platform(None), &GatewayState::Unreadable);
        assert_eq!(by_id(&unknown, "gateway-class").status, Status::Unknown);
        assert_eq!(by_id(&unknown, "gateway").status, Status::Unknown);
        assert_eq!(by_id(&unknown, "issuer").status, Status::Warn);
        assert_eq!(overall(&unknown), Status::Unknown, "unknown outranks a warning");

        let none = platform_checks(None, &Platform::default(), &GatewayState::Unreadable);
        assert_eq!((none.len(), none[0].status), (1, Status::Fail));
    }

    #[test]
    fn a_route_and_its_certificates_explain_themselves() {
        let host = |name: &str, tls, ready| HostExposure {
            host: name.into(),
            tls,
            certificate_ready: ready,
            certificate_message: ready.filter(|r| !r).map(|_| "challenge pending".into()),
        };
        let exposure = ExposureView {
            accepted: Some(true),
            message: None,
            hosts: vec![
                host("a.example.com", "auto", Some(true)),
                host("b.example.com", "auto", Some(false)),
                host("c.example.com", "none", None),
                host("d.example.com", "secret", None),
            ],
        };
        let checks = exposure_checks(Some(&exposure), true);
        let statuses: Vec<_> = checks
            .iter()
            .map(|c| (c.id, c.subject.as_str(), c.status))
            .collect();
        assert_eq!(
            statuses,
            [
                ("route", "", Status::Ok),
                ("certificate", "a.example.com", Status::Ok),
                ("certificate", "b.example.com", Status::Fail),
                ("certificate", "c.example.com", Status::Warn),
                ("certificate", "d.example.com", Status::Unknown),
            ]
        );
        assert_eq!(checks[2].detail, "challenge pending");
        assert_eq!(exposure_checks(None, true)[0].status, Status::Fail);
        assert_eq!(exposure_checks(None, false)[0].status, Status::Warn);
        let waiting = ExposureView {
            accepted: None,
            ..exposure
        };
        assert_eq!(exposure_checks(Some(&waiting), true)[0].status, Status::Unknown);
    }

    #[test]
    fn dns_ports_and_the_agent() {
        let gw: IpAddr = "203.0.113.7".parse().expect("ip");
        let other: IpAddr = "198.51.100.1".parse().expect("ip");
        assert_eq!(dns_check("a", &[gw], &[gw]).status, Status::Ok);
        assert_eq!(dns_check("a", &[other], &[gw]).status, Status::Fail);
        assert_eq!(dns_check("a", &[], &[gw]).status, Status::Fail);
        assert_eq!(dns_check("a", &[other], &[]).status, Status::Unknown);
        assert_eq!(dns_verdict(&[gw], &[gw]).0, "ok");
        assert_eq!(dns_verdict(&[other], &[gw]).0, "mismatch");
        assert_eq!(dns_verdict(&[], &[gw]).0, "unresolved");
        assert_eq!(dns_verdict(&[other], &[]).0, "unknown");
        assert_eq!(port_check(80, Some(Ok(()))).status, Status::Ok);
        assert_eq!(port_check(443, Some(Err("refused".into()))).id, "port-443");
        assert_eq!(port_check(443, None).status, Status::Unknown);
        assert_eq!(agent_check(&AgentState::Seen(Some(5)), 30).status, Status::Ok);
        assert_eq!(agent_check(&AgentState::Seen(Some(300)), 30).status, Status::Fail);
        assert_eq!(agent_check(&AgentState::Seen(None), 30).status, Status::Fail);
        assert_eq!(agent_check(&AgentState::Revoked, 30).status, Status::Fail);
        assert_eq!(agent_check(&AgentState::None, 30).status, Status::Fail);
        assert!(
            Check::new("x", "", Status::Ok, "fine")
                .hint("never shown")
                .hint
                .is_none()
        );
        assert_eq!(overall(&[]), Status::Ok);
    }
}

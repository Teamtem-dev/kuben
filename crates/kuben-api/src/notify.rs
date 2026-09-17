//! Incidents, signed webhooks and GitHub commit statuses (M4.10).
//!
//! The notifier consumes the outbox: every accepted and settled deployment
//! and build becomes an event. A failed deployment or build opens an
//! incident (a repeat counts on the open one) and a success resolves it.
//! Events go to the organization's subscribed webhook endpoints; incident
//! events are held back while a silence covers them. Builds and deployments
//! of Git sources report a commit status.
//!
//! Deliveries are signed: `Kuben-Signature: t=<unix seconds>,v1=<hex
//! HMAC-SHA256 of "<t>.<body>">` with the endpoint's secret. A delivery is
//! retried with backoff and given up after [`MAX_ATTEMPTS`] or a day; an
//! endpoint that keeps failing is disabled. Messages older than
//! [`STALE_EVENT`] are not delivered, so a long outage never ends in a storm
//! of stale events. Private, loopback and link-local targets are refused
//! unless `notify.allow_private_targets` is set.

use std::{net::IpAddr, sync::Arc, time::Duration};

use bytes::Bytes;
use http::{Method, Request, header};
use http_body_util::Full;
use kuben_core::{
    config::NotifyCfg,
    ids::{EnvironmentId, OrgId, ProjectId, TargetId},
    source::RepoName,
    time::now_ms,
};
use kuben_platform::{
    health::Health,
    secrets::{Identity, Keyring},
};
use kuben_store::{
    Store, StoreError,
    repo::{NewIncident, OperationContext, OutboxMessage, WebhookDelivery},
};
use ring::hmac;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

use crate::{
    github::{CommitStatus, GithubApp},
    transport::{Schemes, Transport},
};

const HEALTH: &str = "notifications";
/// Outbox messages or deliveries handled per round.
const BATCH: i64 = 50;
const IDLE: Duration = Duration::from_secs(2);
const TIMEOUT: Duration = Duration::from_secs(10);
/// Deliveries are hidden from other workers this long while in flight.
const LEASE_MS: i64 = 60_000;
/// Attempts of one delivery before it is given up.
pub const MAX_ATTEMPTS: i32 = 12;
/// A delivery older than this is given up.
const MAX_DELIVERY_AGE_MS: i64 = 24 * 3_600_000;
/// Outbox messages older than this are not turned into deliveries.
pub const STALE_EVENT: Duration = Duration::from_hours(1);
/// Who resolves incidents the notifier resolves.
const SYSTEM: &str = "system:notifier";

/// What an outbox message means.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Plan {
    /// The webhook event type.
    pub event: &'static str,
    pub build: bool,
    pub phase: Option<String>,
    pub code: Option<String>,
    /// Open (`Some(true)`) or resolve (`Some(false)`) the incident.
    pub incident: Option<bool>,
    /// The commit status to report.
    pub status: Option<&'static str>,
}

/// The plan of an outbox `topic` with `payload`; `None` for topics nobody
/// is told about.
#[must_use]
pub fn plan(topic: &str, payload: &Value) -> Option<Plan> {
    let phase = payload.get("phase").and_then(Value::as_str).map(str::to_owned);
    let code = payload.get("code").and_then(Value::as_str).map(str::to_owned);
    let base = |event, build, incident, status| Plan {
        event,
        build,
        phase: phase.clone(),
        code: code.clone(),
        incident,
        status,
    };
    Some(match (topic, phase.as_deref()) {
        ("deployment.accepted", _) => base("deployment.started", false, None, Some("pending")),
        ("deployment.settled", Some("succeeded")) => {
            base("deployment.succeeded", false, Some(false), Some("success"))
        }
        ("deployment.settled", Some("failed" | "recoveryFailed" | "manualActionRequired")) => {
            base("deployment.failed", false, Some(true), Some("failure"))
        }
        ("deployment.settled", Some("cancelled")) => base("deployment.cancelled", false, None, Some("error")),
        ("build.queued", _) => base("build.started", true, None, Some("pending")),
        ("build.settled", Some("succeeded")) => base("build.succeeded", true, Some(false), Some("success")),
        ("build.settled", Some("failed")) => base("build.failed", true, Some(true), Some("failure")),
        ("build.settled", Some("cancelled")) => base("build.cancelled", true, None, Some("error")),
        _ => return None,
    })
}

/// The `Kuben-Signature` header of `body` sent at `unix_seconds`.
#[must_use]
pub fn signature(secret: &[u8], unix_seconds: i64, body: &[u8]) -> String {
    let key = hmac::Key::new(hmac::HMAC_SHA256, secret);
    let mut ctx = hmac::Context::with_key(&key);
    ctx.update(unix_seconds.to_string().as_bytes());
    ctx.update(b".");
    ctx.update(body);
    let tag = ctx.sign();
    format!("t={unix_seconds},v1={}", hex(tag.as_ref()))
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut out, b| {
            let _ = write!(out, "{b:02x}");
            out
        })
}

/// When a delivery that failed its `attempts`-th try is tried again; `None`
/// to give it up.
#[must_use]
pub fn retry_at(attempts: i32, created_at: i64, now: i64) -> Option<i64> {
    if attempts >= MAX_ATTEMPTS || now - created_at > MAX_DELIVERY_AGE_MS {
        return None;
    }
    let backoff = 30_000i64
        .saturating_mul(1 << attempts.clamp(0, 10))
        .min(6 * 3_600_000);
    Some(now + backoff)
}

/// Whether `ip` is inside a network a webhook must not reach by default.
#[must_use]
pub fn is_private(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => {
            v4.is_private() || v4.is_loopback() || v4.is_link_local() || v4.is_unspecified() || v4.is_broadcast()
                // 100.64.0.0/10, carrier-grade NAT and many cluster networks.
                || (v4.octets()[0] == 100 && (v4.octets()[1] & 0xc0) == 64)
        }
        IpAddr::V6(v6) => {
            v6.is_loopback()
                || v6.is_unspecified()
                || (v6.segments()[0] & 0xfe00) == 0xfc00
                || (v6.segments()[0] & 0xffc0) == 0xfe80
                || v6.to_ipv4_mapped().is_some_and(|v4| is_private(IpAddr::V4(v4)))
        }
    }
}

/// Why `url` may not receive webhooks under `cfg`, if it may not.
pub async fn check_target(url: &str, cfg: &NotifyCfg) -> Result<(), String> {
    let uri: http::Uri = url.parse().map_err(|_| "not a URL".to_owned())?;
    match uri.scheme_str() {
        Some("https") => {}
        Some("http") if cfg.allow_http => {}
        _ => return Err("only https:// endpoints are allowed".into()),
    }
    let host = uri.host().ok_or("no host")?;
    if cfg.allow_private_targets {
        return Ok(());
    }
    let port = uri.port_u16().unwrap_or(443);
    let addrs = tokio::net::lookup_host((host.trim_matches(['[', ']']), port))
        .await
        .map_err(|e| format!("cannot resolve {host}: {e}"))?;
    for addr in addrs {
        if is_private(addr.ip()) {
            return Err(format!("{host} resolves to a private address ({})", addr.ip()));
        }
    }
    Ok(())
}

/// The notifier of one process.
#[derive(Clone)]
pub struct Notifier {
    store: Store,
    keyring: Arc<Keyring>,
    github: Option<Arc<GithubApp>>,
    transport: Transport,
    cfg: NotifyCfg,
    public_url: Option<String>,
}

impl std::fmt::Debug for Notifier {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Notifier").finish_non_exhaustive()
    }
}

/// Run `notifier` until `token` is cancelled.
pub async fn run(notifier: Notifier, health: Health, token: CancellationToken) -> anyhow::Result<()> {
    health.ok(HEALTH);
    while !token.is_cancelled() {
        let busy = notifier.consume().await? + notifier.deliver().await?;
        if busy == 0 {
            tokio::select! {
                () = token.cancelled() => {}
                () = tokio::time::sleep(IDLE) => {}
            }
        }
    }
    Ok(())
}

impl Notifier {
    #[must_use]
    pub fn new(
        store: Store,
        keyring: Arc<Keyring>,
        github: Option<Arc<GithubApp>>,
        cfg: NotifyCfg,
        public_url: Option<String>,
    ) -> Self {
        let schemes = if cfg.allow_http {
            Schemes::Any
        } else {
            Schemes::HttpsOnly
        };
        Self {
            store,
            keyring,
            github,
            transport: Transport::new(schemes, TIMEOUT),
            cfg,
            public_url: public_url.map(|u| u.trim_end_matches('/').to_owned()),
        }
    }

    /// Turn pending outbox messages into incidents, deliveries and commit
    /// statuses. The number handled.
    pub async fn consume(&self) -> Result<usize, StoreError> {
        let messages = self.store.take_outbox(BATCH, Duration::from_mins(1)).await?;
        let stale_before = now_ms() - i64::try_from(STALE_EVENT.as_millis()).unwrap_or(i64::MAX);
        for message in &messages {
            if message.created_at >= stale_before
                && let Some(plan) = plan(&message.topic, &message.payload)
                && let Err(e) = self.handle(message, &plan).await
            {
                tracing::warn!(message = %message.id, topic = %message.topic, error = %e, "an event is retried");
                continue;
            }
            self.store.outbox_delivered(message.id).await?;
        }
        Ok(messages.len())
    }

    async fn handle(&self, message: &OutboxMessage, plan: &Plan) -> Result<(), StoreError> {
        let Some(operation) = message.operation else {
            return Ok(());
        };
        let mut tenant = self.store.tenant(message.org).await?;
        let Some(ctx) = tenant.operation_context(operation, plan.build).await? else {
            return Ok(());
        };
        let (environment, target) = (
            EnvironmentId::from_uuid(ctx.environment_id),
            TargetId::from_uuid(ctx.target_id),
        );
        let silenced = tenant.silenced(environment, Some(target), now_ms()).await?;
        let payload = self.payload(message, plan, &ctx);
        tenant.enqueue_event(message.id, plan.event, &payload).await?;
        if let Some(open) = plan.incident {
            let key = format!(
                "{}:{}",
                if plan.build { "build" } else { "deployment" },
                ctx.target_id
            );
            let changed = if open {
                let incident = NewIncident {
                    project: Some(ProjectId::from_uuid(ctx.project_id)),
                    environment: Some(environment),
                    target: Some(target),
                    kind: plan.event.to_owned(),
                    severity: if plan.build { "warning" } else { "critical" },
                    dedupe_key: key,
                    title: format!(
                        "{} of {}/{}/{} failed",
                        if plan.build { "The build" } else { "The deployment" },
                        ctx.project,
                        ctx.environment,
                        ctx.app
                    ),
                    detail: plan.code.clone(),
                };
                let (id, new) = tenant.open_incident(&incident).await?;
                new.then_some((id, "incident.opened"))
            } else {
                tenant
                    .resolve_incident_key(&key, SYSTEM)
                    .await?
                    .map(|id| (id, "incident.resolved"))
            };
            if let Some((id, event)) = changed.filter(|_| !silenced) {
                let body = json!({
                    "incident": id,
                    "event": plan.event,
                    "subject": payload["subject"],
                    "runbook": crate::routes::incidents::runbook(plan.event),
                });
                tenant.enqueue_event(Uuid::now_v7(), event, &body).await?;
            }
        }
        tenant.commit().await?;
        if let Some(state) = plan.status {
            self.report_status(&ctx, plan, state).await;
        }
        Ok(())
    }

    fn app_url(&self, ctx: &OperationContext) -> Option<String> {
        self.public_url
            .as_ref()
            .map(|base| format!("{base}/projects/{}/{}/{}", ctx.project, ctx.environment, ctx.app))
    }

    fn payload(&self, message: &OutboxMessage, plan: &Plan, ctx: &OperationContext) -> Value {
        let source: Value = ctx
            .source
            .as_deref()
            .and_then(|s| serde_json::from_str(s).ok())
            .unwrap_or(Value::Null);
        json!({
            "id": message.id,
            "type": plan.event,
            "created_at": now_ms(),
            "subject": {
                "project": ctx.project,
                "environment": ctx.environment,
                "app": ctx.app,
                "url": self.app_url(ctx),
            },
            "operation": message.operation,
            "phase": plan.phase,
            "code": plan.code,
            "reason": ctx.reason,
            "generation": ctx.generation,
            "repository": source.get("repository"),
            "commit": source.get("commit"),
        })
    }

    /// Report a commit status for a Git-sourced build or deployment; a
    /// failure is logged, never retried (the next event reports again).
    async fn report_status(&self, ctx: &OperationContext, plan: &Plan, state: &str) {
        let (Some(github), Some(installation), Some(source)) =
            (&self.github, ctx.installation_id, ctx.source.as_deref())
        else {
            return;
        };
        let source: Value = serde_json::from_str(source).unwrap_or(Value::Null);
        let (Some(repository), Some(commit)) = (
            source
                .get("repository")
                .and_then(Value::as_str)
                .and_then(|r| r.parse::<RepoName>().ok()),
            source.get("commit").and_then(Value::as_str),
        ) else {
            return;
        };
        let context = if plan.build {
            "kuben/build".to_owned()
        } else {
            format!("kuben/{}", ctx.environment)
        };
        let description = format!("{} {}", plan.event, plan.code.as_deref().unwrap_or_default());
        let url = self.app_url(ctx);
        let status = CommitStatus {
            state,
            context: &context,
            description: description.trim(),
            target_url: url.as_deref(),
        };
        let Ok(installation) = u64::try_from(installation) else {
            return;
        };
        if let Err(e) = github
            .commit_status(installation, &repository, commit, &status)
            .await
        {
            tracing::warn!(%repository, commit, error = %e, "a commit status was not reported");
        }
    }

    /// Send the due deliveries. The number sent.
    pub async fn deliver(&self) -> Result<usize, StoreError> {
        let due = self.store.take_deliveries(BATCH, LEASE_MS).await?;
        for delivery in &due {
            let outcome = self.send(delivery).await;
            match outcome {
                Ok(status) => self.store.delivery_succeeded(delivery.id, status).await?,
                Err((status, error)) => {
                    let again = retry_at(delivery.attempts, delivery.created_at, now_ms());
                    self.store
                        .delivery_failed(delivery.id, status, &error, again)
                        .await?;
                }
            }
        }
        Ok(due.len())
    }

    async fn send(&self, d: &WebhookDelivery) -> Result<i32, (Option<i32>, String)> {
        let fail = |e: String| (None, e);
        let found = async {
            let mut tenant = self.store.tenant(d.org).await?;
            tenant.endpoint_secret(d.endpoint).await
        };
        let (url, sealed) = found
            .await
            .map_err(|e| fail(e.to_string()))?
            .ok_or_else(|| fail("the endpoint is disabled".into()))?;
        check_target(&url, &self.cfg).await.map_err(fail)?;
        let secret = open_secret(&self.keyring, d.org, d.endpoint, &sealed).map_err(fail)?;
        let body = Bytes::from(d.payload.to_string());
        let now = now_ms() / 1000;
        let request = Request::builder()
            .method(Method::POST)
            .uri(&url)
            .header(header::CONTENT_TYPE, "application/json")
            .header(header::USER_AGENT, concat!("kuben/", env!("CARGO_PKG_VERSION")))
            .header("kuben-event", &d.event)
            .header("kuben-delivery", d.event_id.to_string())
            .header("kuben-signature", signature(&secret, now, &body))
            .body(Full::new(body))
            .map_err(|e| fail(e.to_string()))?;
        let response = self.transport.send(request).await.map_err(fail)?;
        let status = response.status();
        let code = i32::from(status.as_u16());
        if status.is_success() {
            Ok(code)
        } else {
            Err((Some(code), format!("HTTP {status}")))
        }
    }
}

/// Seal a new endpoint secret for `endpoint` of `org`.
pub fn seal_secret(
    keyring: &Keyring,
    org: OrgId,
    endpoint: Uuid,
    secret: &[u8],
) -> Result<kuben_store::repo::SealedBytes, String> {
    let (org, id) = (org.to_string(), endpoint.to_string());
    keyring
        .seal(
            Identity {
                org: &org,
                secret: &id,
                revision: 0,
            },
            secret,
        )
        .map_err(|e| e.to_string())
}

fn open_secret(
    keyring: &Keyring,
    org: OrgId,
    endpoint: Uuid,
    sealed: &kuben_store::repo::SealedBytes,
) -> Result<Vec<u8>, String> {
    let (org, id) = (org.to_string(), endpoint.to_string());
    keyring
        .open(
            Identity {
                org: &org,
                secret: &id,
                revision: 0,
            },
            sealed,
        )
        .map(|s| s.to_vec())
        .map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn outbox_topics_become_events() {
        let settled = |topic, phase: &str| plan(topic, &json!({ "phase": phase, "code": "RolloutFailed" }));
        let failed = settled("deployment.settled", "failed").expect("plan");
        assert_eq!(
            (
                failed.event,
                failed.incident,
                failed.status,
                failed.code.as_deref()
            ),
            (
                "deployment.failed",
                Some(true),
                Some("failure"),
                Some("RolloutFailed")
            )
        );
        let ok = settled("deployment.settled", "succeeded").expect("plan");
        assert_eq!((ok.event, ok.incident), ("deployment.succeeded", Some(false)));
        assert_eq!(settled("deployment.settled", "superseded"), None);
        assert_eq!(
            plan("deployment.accepted", &json!({})).map(|p| p.event),
            Some("deployment.started")
        );
        let build = settled("build.settled", "failed").expect("plan");
        assert!(build.build && build.incident == Some(true));
        assert_eq!(
            plan("project.apply.settled", &json!({ "phase": "succeeded" })),
            None
        );
    }

    #[test]
    fn signatures_are_hmacs_over_time_and_body() {
        let sig = signature(b"secret", 1_700_000_000, b"{}");
        assert!(sig.starts_with("t=1700000000,v1="));
        assert_eq!(sig.len(), "t=1700000000,v1=".len() + 64);
        assert_ne!(sig, signature(b"secret", 1_700_000_001, b"{}"));
        assert_ne!(sig, signature(b"other", 1_700_000_000, b"{}"));
        let key = hmac::Key::new(hmac::HMAC_SHA256, b"secret");
        let expected = hex(hmac::sign(&key, b"1700000000.{}").as_ref());
        assert!(sig.ends_with(&expected), "receivers can verify it");
    }

    #[test]
    fn deliveries_back_off_and_give_up() {
        let now = 10_000_000;
        assert_eq!(retry_at(1, now, now), Some(now + 60_000));
        assert!(retry_at(8, now, now).expect("retry") <= now + 6 * 3_600_000);
        assert_eq!(retry_at(MAX_ATTEMPTS, now, now), None);
        assert_eq!(retry_at(1, 0, MAX_DELIVERY_AGE_MS + 1), None, "a day old");
    }

    #[test]
    fn private_targets_are_recognized() {
        for ip in [
            "10.0.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "192.168.1.1",
            "100.64.0.1",
            "::1",
            "fd00::1",
            "fe80::1",
            "::ffff:10.0.0.1",
        ] {
            assert!(is_private(ip.parse().expect(ip)), "{ip}");
        }
        for ip in ["1.1.1.1", "8.8.8.8", "2606:4700::1111", "100.128.0.1"] {
            assert!(!is_private(ip.parse().expect(ip)), "{ip}");
        }
    }

    #[tokio::test]
    async fn only_public_https_targets_are_allowed_by_default() {
        let strict = NotifyCfg::default();
        assert!(check_target("http://example.com/hook", &strict).await.is_err());
        assert!(check_target("https://127.0.0.1/hook", &strict).await.is_err());
        assert!(check_target("https://[::1]:8443/hook", &strict).await.is_err());
        assert!(check_target("ftp://example.com", &strict).await.is_err());
        let open = NotifyCfg {
            allow_private_targets: true,
            allow_http: true,
        };
        assert!(check_target("http://127.0.0.1:8080/hook", &open).await.is_ok());
    }

    #[test]
    fn endpoint_secrets_are_sealed_for_their_endpoint() {
        let keyring = Keyring::from_keys([(1, [7; 32])]);
        let (org, endpoint) = (OrgId::new(), Uuid::now_v7());
        let sealed = seal_secret(&keyring, org, endpoint, b"whsec").expect("seal");
        assert_eq!(
            open_secret(&keyring, org, endpoint, &sealed).expect("open"),
            b"whsec"
        );
        assert!(open_secret(&keyring, org, Uuid::now_v7(), &sealed).is_err());
    }
}

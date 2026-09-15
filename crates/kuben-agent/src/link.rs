//! The agent's end of AgentLink (ADR-027): dial the hub, say hello, keep the
//! link alive with heartbeats, and dial again with backoff when it drops.
//!
//! * The agent always dials; the hub never connects to a cluster.
//! * A link that answers no heartbeat for three intervals is dead: the agent
//!   drops it and dials again.
//! * The pause before the next dial doubles from `min_backoff` up to
//!   `max_backoff`, with up to half taken off at random so a hub restart is
//!   not met by every cluster at once. A link that lived longer than
//!   `max_backoff` starts the count again.
//! * A refusal the agent cannot fix by itself (revoked, unknown cluster, no
//!   common protocol version) is retried at the longest pause, never given
//!   up: an operator may fix the hub's side.
//! * Certificates are short-lived. Two thirds into its certificate's life (up
//!   to a tenth earlier at random) the agent asks for a fresh one over the
//!   link, stores it, and dials with it from then on ([`Credentials`]).
//! * Execution envelopes ([`Message::Apply`]) go to the [`Executor`], one
//!   task per target: a newer envelope for a target replaces the one still
//!   running. Its observations go back over the link. Without the
//!   negotiated feature, or without an executor, an envelope is rejected.

use std::{
    collections::{BTreeSet, HashMap},
    fmt, io,
    pin::Pin,
    sync::{Arc, Mutex, MutexGuard, PoisonError},
    time::Duration,
};

use rustls::{
    ClientConfig,
    pki_types::{CertificateDer, ServerName},
};
use time::OffsetDateTime;
use tokio::{
    io::{AsyncRead, AsyncWrite, split},
    net::TcpStream,
    sync::mpsc,
    task::JoinHandle,
    time::{Instant, MissedTickBehavior, interval, sleep},
};
use tokio_rustls::TlsConnector;
use tokio_util::sync::CancellationToken;

use crate::protocol::{
    APPLICATION_RUNTIME, Apply, FrameError, Message, Negotiated, Observation, Refusal, RuntimePhase,
    SUPPORTED_VERSIONS, read_frame, write_frame,
};
use crate::{
    enroll::DeviceKey,
    state::StoredCertificate,
    tls::{Identity, TlsError, agent_config},
};

/// Shortest heartbeat interval the agent accepts from a hub.
const MIN_HEARTBEAT: Duration = Duration::from_millis(10);

/// How the agent reaches the hub: TCP in production, a stream in memory in
/// tests.
pub trait Connector: Send + Sync {
    type Stream: AsyncRead + AsyncWrite + Unpin + Send + 'static;

    fn connect(&self) -> impl Future<Output = io::Result<Self::Stream>> + Send;
}

/// Dials `address` (`host:port`) over TCP.
#[derive(Clone, Debug)]
pub struct TcpConnector {
    pub address: String,
}

impl Connector for TcpConnector {
    type Stream = TcpStream;

    fn connect(&self) -> impl Future<Output = io::Result<TcpStream>> + Send {
        TcpStream::connect(self.address.clone())
    }
}

/// When a certificate is valid.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Lifetime {
    pub not_before: OffsetDateTime,
    pub not_after: OffsetDateTime,
}

/// What the agent authenticates with. A renewal replaces it; the next dial
/// uses the new certificate.
#[derive(Debug)]
pub struct Credentials {
    pinned: Vec<CertificateDer<'static>>,
    current: Mutex<(Arc<ClientConfig>, Option<Lifetime>)>,
}

impl Credentials {
    /// `identity` against the pinned hub CA; `lifetime` of its certificate,
    /// when known, lets the link renew it.
    pub fn new(
        pinned: Vec<CertificateDer<'static>>,
        identity: Identity,
        lifetime: Option<Lifetime>,
    ) -> Result<Self, TlsError> {
        let tls = agent_config(&pinned, Some(identity))?;
        Ok(Self {
            pinned,
            current: Mutex::new((tls, lifetime)),
        })
    }

    fn lock(&self) -> MutexGuard<'_, (Arc<ClientConfig>, Option<Lifetime>)> {
        self.current.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The TLS configuration of the next dial.
    #[must_use]
    pub fn tls(&self) -> Arc<ClientConfig> {
        self.lock().0.clone()
    }

    #[must_use]
    pub fn lifetime(&self) -> Option<Lifetime> {
        self.lock().1
    }

    /// Use `identity` from the next dial on.
    pub fn replace(&self, identity: Identity, lifetime: Lifetime) -> Result<(), TlsError> {
        let tls = agent_config(&self.pinned, Some(identity))?;
        *self.lock() = (tls, Some(lifetime));
        Ok(())
    }
}

/// Keeps a renewed certificate (PEM), e.g. on disk.
pub type StoreCertificate = Arc<dyn Fn(&str) -> Result<(), String> + Send + Sync>;

/// Renewing the certificate over the link.
#[derive(Clone)]
pub struct Renewal {
    /// The device key the fresh certificate is for.
    pub key: Arc<DeviceKey>,
    /// Called with a renewed certificate before the link uses it.
    pub store: StoreCertificate,
}

impl fmt::Debug for Renewal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Renewal")
            .field("device_id", &self.key.device_id())
            .finish_non_exhaustive()
    }
}

/// When to renew: two thirds into the certificate's life, up to a tenth of
/// the life earlier with `jitter` (0 to 1).
#[must_use]
pub fn renew_at(lifetime: &Lifetime, jitter: f64) -> OffsetDateTime {
    let life = lifetime.not_after - lifetime.not_before;
    lifetime.not_before + life * (2.0 / 3.0 - 0.1 * jitter.clamp(0.0, 1.0))
}

/// Where an envelope's observations go, back to the hub.
pub type Reports = mpsc::Sender<Observation>;

/// Carries out the envelopes the hub hands the agent: the cluster side.
pub trait Executor: Send + Sync + fmt::Debug + 'static {
    /// Act on `apply`, sending every observation along the way to `reports`.
    fn apply(&self, apply: Apply, reports: Reports) -> Pin<Box<dyn Future<Output = ()> + Send>>;
}

/// Who the agent is and how it talks to the hub.
#[derive(Clone, Debug)]
pub struct LinkConfig {
    pub cluster_id: String,
    pub agent_version: String,
    pub capabilities: BTreeSet<String>,
    pub kubernetes_version: Option<String>,
    /// The name the hub's certificate must carry.
    pub hub_name: ServerName<'static>,
    /// The pinned hub CA and the agent's identity.
    pub credentials: Arc<Credentials>,
    /// Renew the certificate over the link; `None` never renews.
    pub renewal: Option<Renewal>,
    /// Carries out envelopes; `None` rejects them.
    pub executor: Option<Arc<dyn Executor>>,
    pub min_backoff: Duration,
    pub max_backoff: Duration,
}

impl LinkConfig {
    /// This build's version, no capabilities, no renewal, backoff from 1
    /// second to 5 minutes.
    #[must_use]
    pub fn new(
        cluster_id: impl Into<String>,
        hub_name: ServerName<'static>,
        credentials: Arc<Credentials>,
    ) -> Self {
        Self {
            cluster_id: cluster_id.into(),
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            capabilities: BTreeSet::new(),
            kubernetes_version: None,
            hub_name,
            credentials,
            renewal: None,
            executor: None,
            min_backoff: Duration::from_secs(1),
            max_backoff: Duration::from_mins(5),
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum LinkError {
    #[error("cannot reach the hub: {0}")]
    Connect(io::Error),
    #[error("TLS with the hub failed: {0}")]
    Tls(io::Error),
    #[error(transparent)]
    Frame(#[from] FrameError),
    #[error("the hub refused ({reason:?}): {message}")]
    Refused { reason: Refusal, message: String },
    #[error("the hub closed the link")]
    Closed,
    #[error("the hub sent {0}")]
    Unexpected(&'static str),
    #[error("no heartbeat answer for {0:?}")]
    HeartbeatTimeout(Duration),
}

impl LinkError {
    /// A refusal only the hub's side can lift.
    #[must_use]
    pub const fn is_lasting(&self) -> bool {
        matches!(
            self,
            Self::Refused {
                reason: Refusal::Revoked | Refusal::UnknownCluster | Refusal::UnsupportedProtocol,
                ..
            }
        )
    }
}

/// What the handshake agreed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Session {
    pub negotiated: Negotiated,
    pub hub_version: String,
    pub heartbeat: Duration,
}

/// Say hello on a fresh link and read the hub's answer.
pub async fn handshake<S>(stream: &mut S, config: &LinkConfig) -> Result<Session, LinkError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    write_frame(
        stream,
        &Message::Hello {
            protocol_versions: SUPPORTED_VERSIONS.rev().collect(),
            agent_version: config.agent_version.clone(),
            cluster_id: config.cluster_id.clone(),
            capabilities: config.capabilities.clone(),
            kubernetes_version: config.kubernetes_version.clone(),
        },
    )
    .await?;
    match read_frame(stream).await? {
        Some(Message::Welcome {
            protocol_version,
            hub_version,
            heartbeat_ms,
            features,
        }) => {
            if !SUPPORTED_VERSIONS.contains(&protocol_version) {
                return Err(LinkError::Unexpected(
                    "a protocol version the agent did not offer",
                ));
            }
            Ok(Session {
                negotiated: Negotiated {
                    version: protocol_version,
                    features,
                },
                hub_version,
                heartbeat: Duration::from_millis(heartbeat_ms).max(MIN_HEARTBEAT),
            })
        }
        Some(Message::Refused { reason, message }) => Err(LinkError::Refused { reason, message }),
        Some(_) => Err(LinkError::Unexpected("another message than Welcome")),
        None => Err(LinkError::Closed),
    }
}

type Frames = mpsc::Receiver<Result<Option<Message>, FrameError>>;

/// The envelopes a link is carrying out, and their observations.
struct Work {
    reports: Reports,
    observations: mpsc::Receiver<Observation>,
    running: HashMap<String, JoinHandle<()>>,
}

impl Work {
    fn new() -> Self {
        let (reports, observations) = mpsc::channel(64);
        Self {
            reports,
            observations,
            running: HashMap::new(),
        }
    }

    /// Hand `apply` to the executor in place of an older envelope of the same
    /// target; the observation to send at once when the agent cannot take it.
    fn start(&mut self, config: &LinkConfig, session: &Session, apply: Apply) -> Option<Observation> {
        let rejected = |reason: &str| Observation {
            target: apply.target.clone(),
            generation: 0,
            phase: RuntimePhase::Rejected,
            reason: Some(reason.to_owned()),
            message: None,
        };
        if session.negotiated.require(APPLICATION_RUNTIME).is_err() {
            return Some(rejected("UnsupportedCapability"));
        }
        let Some(executor) = config.executor.clone() else {
            return Some(rejected("NoExecutor"));
        };
        if let Some(older) = self.running.remove(&apply.target) {
            older.abort();
        }
        let target = apply.target.clone();
        self.running
            .insert(target, tokio::spawn(executor.apply(apply, self.reports.clone())));
        None
    }
}

impl Drop for Work {
    fn drop(&mut self) {
        for (_, task) in self.running.drain() {
            task.abort();
        }
    }
}

/// Keep an established link alive until `token` is cancelled (`Ok`) or the
/// link fails. Frames are read by a task of their own, so a frame is never
/// lost halfway through while a heartbeat goes out.
pub async fn keep_alive<S>(
    stream: S,
    session: &Session,
    config: &LinkConfig,
    token: &CancellationToken,
) -> Result<(), LinkError>
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let (mut reader, mut writer) = split(stream);
    let (tx, mut rx) = mpsc::channel(16);
    let reading = tokio::spawn(async move {
        loop {
            let frame = read_frame(&mut reader).await;
            let last = !matches!(frame, Ok(Some(_)));
            if tx.send(frame).await.is_err() || last {
                break;
            }
        }
    });
    let mut work = Work::new();
    let result = heartbeats(&mut writer, &mut rx, session, config, &mut work, token).await;
    reading.abort();
    result
}

async fn heartbeats<W>(
    writer: &mut W,
    frames: &mut Frames,
    session: &Session,
    config: &LinkConfig,
    work: &mut Work,
    token: &CancellationToken,
) -> Result<(), LinkError>
where
    W: AsyncWrite + Unpin,
{
    let dead_after = session.heartbeat * 3;
    let mut tick = interval(session.heartbeat);
    tick.set_missed_tick_behavior(MissedTickBehavior::Delay);
    let mut seq = 0_u64;
    let mut last_answer = Instant::now();
    let jitter: f64 = rand::random();
    let mut renewing = false;
    loop {
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            Some(observation) = work.observations.recv() => {
                write_frame(writer, &Message::Observed(observation)).await?;
            }
            _ = tick.tick() => {
                if last_answer.elapsed() > dead_after {
                    return Err(LinkError::HeartbeatTimeout(dead_after));
                }
                seq += 1;
                write_frame(writer, &Message::Heartbeat { seq }).await?;
                if !renewing && let Some(renewal) = renewal_due(config, jitter) {
                    match renewal.key.csr_pem(&config.cluster_id) {
                        Ok(csr) => {
                            write_frame(writer, &Message::Renew { csr }).await?;
                            renewing = true;
                        }
                        Err(error) => tracing::warn!(%error, "cannot make a renewal request"),
                    }
                }
            }
            frame = frames.recv() => match frame {
                Some(Ok(Some(Message::HeartbeatAck { .. }))) => last_answer = Instant::now(),
                Some(Ok(Some(Message::Enrolled { certificate, .. }))) if renewing => {
                    renewing = false;
                    if let Err(error) = adopt(config, &certificate) {
                        tracing::warn!(%error, "cannot use the renewed certificate");
                    }
                }
                Some(Ok(Some(Message::Apply(apply)))) => {
                    if let Some(rejected) = work.start(config, session, apply) {
                        write_frame(writer, &Message::Observed(rejected)).await?;
                    }
                }
                Some(Ok(Some(Message::Refused { reason, message }))) => {
                    return Err(LinkError::Refused { reason, message });
                }
                // Messages of later steps, and ones this build does not know.
                Some(Ok(Some(_))) => {}
                Some(Ok(None)) | None => return Err(LinkError::Closed),
                Some(Err(e)) => return Err(e.into()),
            },
        }
    }
}

/// The renewal to run now, if one is set up and due.
fn renewal_due(config: &LinkConfig, jitter: f64) -> Option<&Renewal> {
    let renewal = config.renewal.as_ref()?;
    let lifetime = config.credentials.lifetime()?;
    (OffsetDateTime::now_utc() >= renew_at(&lifetime, jitter)).then_some(renewal)
}

/// Store a renewed certificate, then dial with it from now on.
fn adopt(config: &LinkConfig, pem: &str) -> Result<(), String> {
    let renewal = config.renewal.as_ref().ok_or("no renewal is set up")?;
    let stored = StoredCertificate::parse(pem)?;
    (renewal.store)(pem)?;
    let identity = renewal.key.identity(pem).map_err(|e| e.to_string())?;
    config
        .credentials
        .replace(
            identity,
            Lifetime {
                not_before: stored.not_before,
                not_after: stored.not_after,
            },
        )
        .map_err(|e| e.to_string())?;
    tracing::info!(not_after = %stored.not_after, "certificate renewed");
    Ok(())
}

/// The pause before the next dial after `failures` failed ones: doubling
/// from `min`, capped at `max`, with `jitter` (0 to 1) of up to half taken
/// off.
#[must_use]
pub fn backoff(failures: u32, min: Duration, max: Duration, jitter: f64) -> Duration {
    let full = min
        .saturating_mul(2_u32.saturating_pow(failures.min(20)))
        .min(max);
    full.mul_f64(1.0 - 0.5 * jitter.clamp(0.0, 1.0))
}

/// Keep the agent linked to the hub until `token` is cancelled.
pub async fn run<C: Connector>(connector: &C, config: &LinkConfig, token: &CancellationToken) {
    let mut failures = 0_u32;
    while !token.is_cancelled() {
        let dialed = Instant::now();
        let outcome = async {
            let tcp = connector.connect().await.map_err(LinkError::Connect)?;
            let mut stream = TlsConnector::from(config.credentials.tls())
                .connect(config.hub_name.clone(), tcp)
                .await
                .map_err(LinkError::Tls)?;
            let session = handshake(&mut stream, config).await?;
            tracing::info!(
                cluster = %config.cluster_id,
                protocol = session.negotiated.version,
                hub = %session.hub_version,
                features = ?session.negotiated.features,
                "AgentLink up"
            );
            keep_alive(stream, &session, config, token).await
        }
        .await;
        let Err(error) = outcome else {
            return;
        };
        if dialed.elapsed() > config.max_backoff {
            failures = 0;
        }
        let pause = if error.is_lasting() {
            config.max_backoff
        } else {
            backoff(failures, config.min_backoff, config.max_backoff, rand::random())
        };
        failures = failures.saturating_add(1);
        tracing::warn!(
            cluster = %config.cluster_id,
            %error,
            retry_in_ms = u64::try_from(pause.as_millis()).unwrap_or(u64::MAX),
            "AgentLink down"
        );
        tokio::select! {
            () = token.cancelled() => return,
            () = sleep(pause) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_doubles_up_to_the_cap_and_jitter_takes_off_at_most_half() {
        let (min, max) = (Duration::from_secs(1), Duration::from_mins(5));
        assert_eq!(backoff(0, min, max, 0.0), Duration::from_secs(1));
        assert_eq!(backoff(3, min, max, 0.0), Duration::from_secs(8));
        assert_eq!(backoff(30, min, max, 0.0), max);
        assert_eq!(backoff(3, min, max, 1.0), Duration::from_secs(4));
        assert_eq!(
            backoff(3, min, max, 7.0),
            Duration::from_secs(4),
            "jitter is clamped"
        );
    }

    #[test]
    fn renewal_is_due_two_thirds_into_the_life_at_the_latest() {
        let not_before = OffsetDateTime::from_unix_timestamp(1_789_000_000).expect("time");
        let life = Lifetime {
            not_before,
            not_after: not_before + Duration::from_hours(24),
        };
        let close = |a: OffsetDateTime, b: OffsetDateTime| (a - b).abs() < time::Duration::seconds(1);
        assert!(close(renew_at(&life, 0.0), not_before + Duration::from_hours(16)));
        assert!(close(
            renew_at(&life, 1.0),
            not_before + Duration::from_mins(16 * 60 - 144)
        ));
        assert!(
            close(renew_at(&life, 9.0), renew_at(&life, 1.0)),
            "jitter is clamped"
        );
    }

    #[test]
    fn only_refusals_the_hub_must_lift_are_lasting() {
        let refused = |reason| LinkError::Refused {
            reason,
            message: String::new(),
        };
        assert!(refused(Refusal::Revoked).is_lasting());
        assert!(refused(Refusal::UnsupportedProtocol).is_lasting());
        assert!(!refused(Refusal::BadRequest).is_lasting());
        assert!(!LinkError::Closed.is_lasting());
    }
}

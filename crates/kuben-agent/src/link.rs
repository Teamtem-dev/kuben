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

use std::{collections::BTreeSet, io, sync::Arc, time::Duration};

use rustls::{ClientConfig, pki_types::ServerName};
use tokio::{
    io::{AsyncRead, AsyncWrite, split},
    net::TcpStream,
    sync::mpsc,
    time::{Instant, MissedTickBehavior, interval, sleep},
};
use tokio_rustls::TlsConnector;
use tokio_util::sync::CancellationToken;

use crate::protocol::{
    FrameError, Message, Negotiated, Refusal, SUPPORTED_VERSIONS, read_frame, write_frame,
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

/// Who the agent is and how it talks to the hub.
#[derive(Clone, Debug)]
pub struct LinkConfig {
    pub cluster_id: String,
    pub agent_version: String,
    pub capabilities: BTreeSet<String>,
    pub kubernetes_version: Option<String>,
    /// The name the hub's certificate must carry.
    pub hub_name: ServerName<'static>,
    /// TLS with the pinned hub CA and the agent's identity.
    pub tls: Arc<ClientConfig>,
    pub min_backoff: Duration,
    pub max_backoff: Duration,
}

impl LinkConfig {
    /// This build's version, no capabilities, backoff from 1 second to 5
    /// minutes.
    #[must_use]
    pub fn new(cluster_id: impl Into<String>, hub_name: ServerName<'static>, tls: Arc<ClientConfig>) -> Self {
        Self {
            cluster_id: cluster_id.into(),
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            capabilities: BTreeSet::new(),
            kubernetes_version: None,
            hub_name,
            tls,
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

/// Keep an established link alive until `token` is cancelled (`Ok`) or the
/// link fails. Frames are read by a task of their own, so a frame is never
/// lost halfway through while a heartbeat goes out.
pub async fn keep_alive<S>(stream: S, session: &Session, token: &CancellationToken) -> Result<(), LinkError>
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
    let result = heartbeats(&mut writer, &mut rx, session, token).await;
    reading.abort();
    result
}

async fn heartbeats<W>(
    writer: &mut W,
    frames: &mut Frames,
    session: &Session,
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
    loop {
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            _ = tick.tick() => {
                if last_answer.elapsed() > dead_after {
                    return Err(LinkError::HeartbeatTimeout(dead_after));
                }
                seq += 1;
                write_frame(writer, &Message::Heartbeat { seq }).await?;
            }
            frame = frames.recv() => match frame {
                Some(Ok(Some(Message::HeartbeatAck { .. }))) => last_answer = Instant::now(),
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
    let tls = TlsConnector::from(config.tls.clone());
    let mut failures = 0_u32;
    while !token.is_cancelled() {
        let dialed = Instant::now();
        let outcome = async {
            let tcp = connector.connect().await.map_err(LinkError::Connect)?;
            let mut stream = tls
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
            keep_alive(stream, &session, token).await
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

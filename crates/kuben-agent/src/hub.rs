//! The hub's end of AgentLink, hosted by `kuben serve` (M1.9 integration)
//! and tested end to end in this crate.
//!
//! * An anonymous peer may only enroll ([`receive_enrollment`]; the device is recorded before [`answer_enrollment`] hands the certificate over); the issued
//!   certificate is recorded in the [`Registry`].
//! * An authenticated peer is the cluster its certificate names (the URI
//!   SAN `kuben://cluster/<id>` the hub issued). Its Hello must name the same
//!   cluster, and its device must be the cluster's current, unrevoked agent
//!   device ([`Registry::admits`]): a revoked device, or one a newer
//!   enrollment replaced, is refused even while its certificate is valid.
//! * The handshake negotiates the protocol version and the features
//!   ([`negotiate`]); the hub then answers heartbeats and records the link
//!   and when it last heard from each cluster.
//! * A linked agent renews its certificate over the link, for the device
//!   key the link authenticated with ([`Enrollment::renew`]).
//! * [`Hub::send`] hands a linked agent an execution envelope, when both
//!   sides negotiated [`APPLICATION_RUNTIME`]; the agent's observations go to
//!   [`Registry::observed`].

use std::{
    collections::{BTreeSet, HashMap},
    ops::RangeInclusive,
    sync::{Mutex, MutexGuard, PoisonError},
    time::{Duration, Instant},
};

use rustls::pki_types::CertificateDer;
use time::OffsetDateTime;
use tokio::{
    io::{AsyncRead, AsyncWrite, split},
    sync::mpsc,
};
use tokio_rustls::server::TlsStream;
use x509_parser::prelude::{FromDer, GeneralName, ParsedExtension, X509Certificate};

use crate::{
    enroll::{Enrollment, TokenStore, answer_enrollment, device_id_of, receive_enrollment},
    protocol::{
        APPLICATION_RUNTIME, Apply, FrameError, Message, Observation, Refusal, SUPPORTED_VERSIONS, negotiate,
        read_frame, write_frame,
    },
    tls::peer_certificate,
};

/// What the hub offers.
#[derive(Clone, Debug)]
pub struct HubSettings {
    pub hub_version: String,
    pub versions: RangeInclusive<u32>,
    pub features: BTreeSet<String>,
    /// How often agents send a heartbeat.
    pub heartbeat: Duration,
}

impl Default for HubSettings {
    fn default() -> Self {
        Self {
            hub_version: env!("CARGO_PKG_VERSION").to_owned(),
            versions: SUPPORTED_VERSIONS,
            features: BTreeSet::new(),
            heartbeat: Duration::from_secs(10),
        }
    }
}

/// What the hub knows of a cluster's latest link.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SessionInfo {
    pub version: u32,
    pub features: BTreeSet<String>,
    pub agent_version: String,
    pub heartbeats: u64,
    pub last_seen: Instant,
}

/// Where the hub keeps what it knows of each cluster's agent: SQL in `kuben
/// serve`, memory in tests. A registry that cannot answer refuses.
pub trait Registry: Send + Sync {
    /// Whether `device` is the current, unrevoked agent device of `cluster`.
    fn admits(&self, cluster: &str, device: &str) -> impl Future<Output = bool> + Send;

    /// A certificate valid until `not_after` was issued to `device` of
    /// `cluster`: it is the cluster's agent device from now on.
    fn certified(
        &self,
        cluster: &str,
        device: &str,
        not_after: OffsetDateTime,
    ) -> impl Future<Output = ()> + Send;

    /// `device` of `cluster` linked with `session`.
    fn linked(&self, cluster: &str, device: &str, session: &SessionInfo) -> impl Future<Output = ()> + Send;

    /// A heartbeat of `device` of `cluster` arrived.
    fn heard(&self, cluster: &str, device: &str) -> impl Future<Output = ()> + Send;

    /// `device` of `cluster` reported `observation` of an envelope.
    fn observed(
        &self,
        cluster: &str,
        device: &str,
        observation: &Observation,
    ) -> impl Future<Output = ()> + Send;
}

fn lock<V>(m: &Mutex<V>) -> MutexGuard<'_, V> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

/// A registry in memory, for tests.
#[derive(Debug, Default)]
pub struct MemoryRegistry {
    /// Cluster → its agent device and whether that device is revoked.
    devices: Mutex<HashMap<String, (String, bool)>>,
    /// Cluster → the observations its agent reported, oldest first.
    observations: Mutex<HashMap<String, Vec<Observation>>>,
    /// How long recording a certificate takes: none, or a slow database.
    certify_delay: Duration,
}

impl MemoryRegistry {
    /// A registry that takes `delay` to record each certificate, as a
    /// database may.
    #[must_use]
    pub fn with_certify_delay(delay: Duration) -> Self {
        Self {
            certify_delay: delay,
            ..Self::default()
        }
    }

    /// What the agent of `cluster` reported, oldest first.
    #[must_use]
    pub fn observations(&self, cluster: &str) -> Vec<Observation> {
        lock(&self.observations).get(cluster).cloned().unwrap_or_default()
    }

    /// Revoke the current device of `cluster`.
    pub fn revoke(&self, cluster: &str) {
        if let Some(entry) = lock(&self.devices).get_mut(cluster) {
            entry.1 = true;
        }
    }
}

impl Registry for MemoryRegistry {
    fn admits(&self, cluster: &str, device: &str) -> impl Future<Output = bool> + Send {
        let admitted = lock(&self.devices)
            .get(cluster)
            .is_some_and(|(current, revoked)| current == device && !revoked);
        std::future::ready(admitted)
    }

    fn certified(
        &self,
        cluster: &str,
        device: &str,
        _not_after: OffsetDateTime,
    ) -> impl Future<Output = ()> + Send {
        let (cluster, device, delay) = (cluster.to_owned(), device.to_owned(), self.certify_delay);
        async move {
            if !delay.is_zero() {
                tokio::time::sleep(delay).await;
            }
            let mut devices = lock(&self.devices);
            // The same device keeps its revocation; another one replaces it.
            if devices
                .get(&cluster)
                .is_none_or(|(current, _)| *current != device)
            {
                devices.insert(cluster, (device, false));
            }
        }
    }

    fn linked(
        &self,
        _cluster: &str,
        _device: &str,
        _session: &SessionInfo,
    ) -> impl Future<Output = ()> + Send {
        std::future::ready(())
    }

    fn heard(&self, _cluster: &str, _device: &str) -> impl Future<Output = ()> + Send {
        std::future::ready(())
    }

    fn observed(
        &self,
        cluster: &str,
        _device: &str,
        observation: &Observation,
    ) -> impl Future<Output = ()> + Send {
        lock(&self.observations)
            .entry(cluster.to_owned())
            .or_default()
            .push(observation.clone());
        std::future::ready(())
    }
}

/// A cluster's latest link: what it negotiated, and the way to send it
/// envelopes.
#[derive(Debug)]
struct Linked {
    info: SessionInfo,
    outbox: mpsc::Sender<Message>,
}

/// The hub's AgentLink endpoint.
#[derive(Debug)]
pub struct Hub<T, R> {
    enrollment: Enrollment<T>,
    registry: R,
    settings: HubSettings,
    sessions: Mutex<HashMap<String, Linked>>,
}

/// The cluster a certificate the hub issued names.
#[must_use]
pub fn cluster_of(certificate: &CertificateDer<'_>) -> Option<String> {
    let (_, cert) = X509Certificate::from_der(certificate).ok()?;
    cert.extensions()
        .iter()
        .find_map(|ext| match ext.parsed_extension() {
            ParsedExtension::SubjectAlternativeName(san) => {
                san.general_names.iter().find_map(|name| match name {
                    GeneralName::URI(uri) => uri.strip_prefix("kuben://cluster/").map(str::to_owned),
                    _ => None,
                })
            }
            _ => None,
        })
}

/// The device id of the key a certificate is for.
fn device_of(certificate: &CertificateDer<'_>) -> Option<String> {
    let (_, cert) = X509Certificate::from_der(certificate).ok()?;
    Some(device_id_of(cert.public_key().raw))
}

async fn refuse<S: AsyncWrite + Unpin>(
    stream: &mut S,
    reason: Refusal,
    message: &str,
) -> Result<(), FrameError> {
    write_frame(
        stream,
        &Message::Refused {
            reason,
            message: message.to_owned(),
        },
    )
    .await
}

const NOT_THE_AGENT: &str =
    "this device is not the cluster's agent: revoked, or replaced by a newer enrollment";

impl<T: TokenStore, R: Registry> Hub<T, R> {
    #[must_use]
    pub fn new(enrollment: Enrollment<T>, registry: R, settings: HubSettings) -> Self {
        Self {
            enrollment,
            registry,
            settings,
            sessions: Mutex::new(HashMap::new()),
        }
    }

    #[must_use]
    pub const fn enrollment(&self) -> &Enrollment<T> {
        &self.enrollment
    }

    #[must_use]
    pub const fn registry(&self) -> &R {
        &self.registry
    }

    /// The latest link of `cluster` this hub served, if any.
    #[must_use]
    pub fn session(&self, cluster: &str) -> Option<SessionInfo> {
        lock(&self.sessions)
            .get(cluster)
            .map(|linked| linked.info.clone())
    }

    /// Hand the agent of `cluster` the envelope `apply` over its live link.
    /// False when it is not linked, or did not negotiate
    /// [`APPLICATION_RUNTIME`].
    pub async fn send(&self, cluster: &str, apply: Apply) -> bool {
        let outbox = {
            let sessions = lock(&self.sessions);
            match sessions.get(cluster) {
                Some(linked) if linked.info.features.contains(APPLICATION_RUNTIME) => linked.outbox.clone(),
                _ => return false,
            }
        };
        outbox.send(Message::Apply(apply)).await.is_ok()
    }

    /// Serve one accepted TLS connection until it ends.
    pub async fn serve<S>(&self, mut tls: TlsStream<S>) -> Result<(), FrameError>
    where
        S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
    {
        let Some(certificate) = peer_certificate(tls.get_ref().1) else {
            let now = OffsetDateTime::now_utc();
            if let Some(issued) = receive_enrollment(&mut tls, &self.enrollment, now).await? {
                // Recorded before the agent has its certificate: it links at
                // once, and a Hello the registry does not know yet is refused
                // as revoked (seen in CI with the SQL registry).
                self.registry
                    .certified(&issued.cluster_id, &issued.device_id, issued.not_after)
                    .await;
                answer_enrollment(&mut tls, &issued).await?;
            }
            return Ok(());
        };
        let (Some(cluster), Some(device)) = (cluster_of(&certificate), device_of(&certificate)) else {
            return refuse(
                &mut tls,
                Refusal::UnknownCluster,
                "the certificate names no cluster",
            )
            .await;
        };
        let Some(Message::Hello {
            protocol_versions,
            agent_version,
            cluster_id,
            capabilities,
            ..
        }) = read_frame(&mut tls).await?
        else {
            return refuse(&mut tls, Refusal::BadRequest, "a link opens with Hello").await;
        };
        if cluster_id != cluster {
            return refuse(
                &mut tls,
                Refusal::UnknownCluster,
                "Hello names another cluster than the certificate",
            )
            .await;
        }
        if !self.registry.admits(&cluster, &device).await {
            return refuse(&mut tls, Refusal::Revoked, NOT_THE_AGENT).await;
        }
        let agreed = match negotiate(
            &self.settings.versions,
            &self.settings.features,
            &protocol_versions,
            &capabilities,
        ) {
            Ok(agreed) => agreed,
            Err(reason) => return refuse(&mut tls, reason, "no protocol version in common").await,
        };
        write_frame(
            &mut tls,
            &Message::Welcome {
                protocol_version: agreed.version,
                hub_version: self.settings.hub_version.clone(),
                heartbeat_ms: u64::try_from(self.settings.heartbeat.as_millis()).unwrap_or(u64::MAX),
                features: agreed.features.clone(),
            },
        )
        .await?;
        let session = SessionInfo {
            version: agreed.version,
            features: agreed.features,
            agent_version,
            heartbeats: 0,
            last_seen: Instant::now(),
        };
        self.registry.linked(&cluster, &device, &session).await;
        let (outbox, commands) = mpsc::channel(16);
        lock(&self.sessions).insert(
            cluster.clone(),
            Linked {
                info: session,
                outbox,
            },
        );
        self.converse(tls, &cluster, &device, commands).await
    }

    /// The linked part of a connection until the agent hangs up. Frames are
    /// read by a task of their own, so none is lost halfway while an envelope
    /// goes out.
    async fn converse<S>(
        &self,
        tls: TlsStream<S>,
        cluster: &str,
        device: &str,
        mut commands: mpsc::Receiver<Message>,
    ) -> Result<(), FrameError>
    where
        S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
    {
        let (mut reader, mut writer) = split(tls);
        let (tx, mut frames) = mpsc::channel(16);
        let reading = tokio::spawn(async move {
            loop {
                let frame = read_frame(&mut reader).await;
                let last = !matches!(frame, Ok(Some(_)));
                if tx.send(frame).await.is_err() || last {
                    break;
                }
            }
        });
        let result = loop {
            let frame = tokio::select! {
                Some(command) = commands.recv() => {
                    if let Err(e) = write_frame(&mut writer, &command).await {
                        break Err(e);
                    }
                    continue;
                }
                frame = frames.recv() => frame,
            };
            match frame {
                None | Some(Ok(None)) => break Ok(()),
                Some(Err(e)) => break Err(e),
                Some(Ok(Some(message))) => match self.handle(&mut writer, message, cluster, device).await {
                    Ok(true) => {}
                    Ok(false) => break Ok(()),
                    Err(e) => break Err(e),
                },
            }
        };
        reading.abort();
        result
    }

    /// One message of a linked agent; false when the link must close.
    async fn handle<W>(
        &self,
        writer: &mut W,
        message: Message,
        cluster: &str,
        device: &str,
    ) -> Result<bool, FrameError>
    where
        W: AsyncWrite + Unpin,
    {
        match message {
            Message::Heartbeat { seq } => {
                if let Some(linked) = lock(&self.sessions).get_mut(cluster) {
                    linked.info.heartbeats += 1;
                    linked.info.last_seen = Instant::now();
                }
                self.registry.heard(cluster, device).await;
                write_frame(writer, &Message::HeartbeatAck { seq }).await?;
                Ok(true)
            }
            Message::Renew { csr } => {
                if !self.registry.admits(cluster, device).await {
                    refuse(writer, Refusal::Revoked, NOT_THE_AGENT).await?;
                    return Ok(false);
                }
                let issued = match self
                    .enrollment
                    .renew(cluster, &csr, device, OffsetDateTime::now_utc())
                {
                    Ok(issued) => issued,
                    Err(reason) => {
                        refuse(writer, reason, "a renewal is for the device key of this link").await?;
                        return Ok(false);
                    }
                };
                self.registry.certified(cluster, device, issued.not_after).await;
                write_frame(
                    writer,
                    &Message::Enrolled {
                        certificate: issued.certificate_pem,
                        not_after: issued.not_after.unix_timestamp(),
                    },
                )
                .await?;
                Ok(true)
            }
            Message::Observed(observation) => {
                self.registry.observed(cluster, device, &observation).await;
                Ok(true)
            }
            Message::Unknown => Ok(true),
            _ => {
                refuse(writer, Refusal::BadRequest, "unexpected message").await?;
                Ok(false)
            }
        }
    }
}

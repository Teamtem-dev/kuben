//! The hub's end of AgentLink, hosted by `kuben serve` (M1.9 integration)
//! and tested end to end in this crate.
//!
//! * An anonymous peer may only enroll ([`serve_enrollment`]); the issued
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

use std::{
    collections::{BTreeSet, HashMap},
    ops::RangeInclusive,
    sync::{Mutex, MutexGuard, PoisonError},
    time::{Duration, Instant},
};

use rustls::pki_types::CertificateDer;
use time::OffsetDateTime;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio_rustls::server::TlsStream;
use x509_parser::prelude::{FromDer, GeneralName, ParsedExtension, X509Certificate};

use crate::{
    enroll::{Enrollment, TokenStore, device_id_of, serve_enrollment},
    protocol::{FrameError, Message, Refusal, SUPPORTED_VERSIONS, negotiate, read_frame, write_frame},
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
}

fn lock<V>(m: &Mutex<V>) -> MutexGuard<'_, V> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

/// A registry in memory, for tests.
#[derive(Debug, Default)]
pub struct MemoryRegistry {
    /// Cluster → its agent device and whether that device is revoked.
    devices: Mutex<HashMap<String, (String, bool)>>,
}

impl MemoryRegistry {
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
        let mut devices = lock(&self.devices);
        // The same device keeps its revocation; another one replaces it.
        if devices.get(cluster).is_none_or(|(current, _)| current != device) {
            devices.insert(cluster.to_owned(), (device.to_owned(), false));
        }
        std::future::ready(())
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
}

/// The hub's AgentLink endpoint.
#[derive(Debug)]
pub struct Hub<T, R> {
    enrollment: Enrollment<T>,
    registry: R,
    settings: HubSettings,
    sessions: Mutex<HashMap<String, SessionInfo>>,
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
        lock(&self.sessions).get(cluster).cloned()
    }

    /// Serve one accepted TLS connection until it ends.
    pub async fn serve<S>(&self, mut tls: TlsStream<S>) -> Result<(), FrameError>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        let Some(certificate) = peer_certificate(tls.get_ref().1) else {
            let now = OffsetDateTime::now_utc();
            if let Some(issued) = serve_enrollment(&mut tls, &self.enrollment, now).await? {
                self.registry
                    .certified(&issued.cluster_id, &issued.device_id, issued.not_after)
                    .await;
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
        lock(&self.sessions).insert(cluster.clone(), session);
        self.converse(&mut tls, &cluster, &device).await
    }

    /// The linked part of a connection: heartbeats and renewals until the
    /// agent hangs up.
    async fn converse<S>(&self, tls: &mut TlsStream<S>, cluster: &str, device: &str) -> Result<(), FrameError>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        loop {
            match read_frame(tls).await? {
                Some(Message::Heartbeat { seq }) => {
                    if let Some(session) = lock(&self.sessions).get_mut(cluster) {
                        session.heartbeats += 1;
                        session.last_seen = Instant::now();
                    }
                    self.registry.heard(cluster, device).await;
                    write_frame(tls, &Message::HeartbeatAck { seq }).await?;
                }
                Some(Message::Renew { csr }) => {
                    if !self.registry.admits(cluster, device).await {
                        return refuse(tls, Refusal::Revoked, NOT_THE_AGENT).await;
                    }
                    let issued = match self
                        .enrollment
                        .renew(cluster, &csr, device, OffsetDateTime::now_utc())
                    {
                        Ok(issued) => issued,
                        Err(reason) => {
                            return refuse(tls, reason, "a renewal is for the device key of this link").await;
                        }
                    };
                    self.registry.certified(cluster, device, issued.not_after).await;
                    write_frame(
                        tls,
                        &Message::Enrolled {
                            certificate: issued.certificate_pem,
                            not_after: issued.not_after.unix_timestamp(),
                        },
                    )
                    .await?;
                }
                Some(Message::Unknown) => {}
                Some(_) => return refuse(tls, Refusal::BadRequest, "unexpected message").await,
                None => return Ok(()),
            }
        }
    }
}

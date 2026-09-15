//! The hub's end of AgentLink: enough to test the agent end to end, and the
//! piece `kuben serve` will host in the M1.9 integration.
//!
//! * An anonymous peer may only enroll ([`serve_enrollment`]).
//! * An authenticated peer is the cluster its certificate names (the URI
//!   SAN `kuben://cluster/<id>` the hub issued); its Hello must name the same
//!   cluster, and a revoked cluster is refused.
//! * The handshake negotiates the protocol version and the features
//!   ([`negotiate`]); the hub then answers heartbeats and records when it
//!   last heard from each cluster, which is how it tells a stale agent.
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

/// The hub's AgentLink endpoint.
#[derive(Debug)]
pub struct Hub<T> {
    enrollment: Enrollment<T>,
    settings: HubSettings,
    sessions: Mutex<HashMap<String, SessionInfo>>,
    revoked: Mutex<BTreeSet<String>>,
}

fn lock<V>(m: &Mutex<V>) -> MutexGuard<'_, V> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
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

impl<T: TokenStore> Hub<T> {
    #[must_use]
    pub fn new(enrollment: Enrollment<T>, settings: HubSettings) -> Self {
        Self {
            enrollment,
            settings,
            sessions: Mutex::new(HashMap::new()),
            revoked: Mutex::new(BTreeSet::new()),
        }
    }

    #[must_use]
    pub const fn enrollment(&self) -> &Enrollment<T> {
        &self.enrollment
    }

    /// Refuse `cluster`'s links from now on (re-enrollment needed).
    pub fn revoke(&self, cluster: &str) {
        lock(&self.revoked).insert(cluster.to_owned());
    }

    /// The latest link of `cluster`, if it ever linked.
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
            serve_enrollment(&mut tls, &self.enrollment, OffsetDateTime::now_utc()).await?;
            return Ok(());
        };
        let Some(cluster) = cluster_of(&certificate) else {
            return refuse(
                &mut tls,
                Refusal::UnknownCluster,
                "the certificate names no cluster",
            )
            .await;
        };
        let device = device_of(&certificate).unwrap_or_default();
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
        if lock(&self.revoked).contains(&cluster) {
            return refuse(
                &mut tls,
                Refusal::Revoked,
                "this cluster's enrollment was revoked",
            )
            .await;
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
        lock(&self.sessions).insert(
            cluster.clone(),
            SessionInfo {
                version: agreed.version,
                features: agreed.features,
                agent_version,
                heartbeats: 0,
                last_seen: Instant::now(),
            },
        );
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
                    write_frame(tls, &Message::HeartbeatAck { seq }).await?;
                }
                Some(Message::Renew { csr }) => {
                    match self
                        .enrollment
                        .renew(cluster, &csr, device, OffsetDateTime::now_utc())
                    {
                        Ok(issued) => {
                            write_frame(
                                tls,
                                &Message::Enrolled {
                                    certificate: issued.certificate_pem,
                                    not_after: issued.not_after.unix_timestamp(),
                                },
                            )
                            .await?;
                        }
                        Err(reason) => {
                            return refuse(tls, reason, "a renewal is for the device key of this link").await;
                        }
                    }
                }
                Some(Message::Unknown) => {}
                Some(_) => return refuse(tls, Refusal::BadRequest, "unexpected message").await,
                None => return Ok(()),
            }
        }
    }
}

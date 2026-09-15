//! Enrollment (ADR-027, plan §11.4): how a cluster agent gets its identity.
//!
//! 1. The agent generates its device key ([`DeviceKey`]); the key never
//!    leaves the device, and the digest of its public key is the device id.
//! 2. With a bootstrap token (read from a file or stdin, never from the
//!    command line) it connects to the hub anonymously — TLS with the pinned
//!    hub CA, no client certificate — and sends [`Message::Enroll`]: the
//!    token, its cluster and a CSR signed by the device key, which proves it
//!    holds the key ([`request_enrollment`]).
//! 3. The hub ([`Enrollment`], [`serve_enrollment`]) keeps only the token's
//!    hash. A token is bound to one cluster, expires, and is redeemed once;
//!    redeeming it again with the same device key resumes, so a lost answer
//!    never strands the agent, while any other key is refused. Every refusal
//!    looks alike ([`Refusal::InvalidToken`]).
//! 4. The hub issues a short-lived client certificate from the CSR's public
//!    key alone: the subject, names, key usage and lifetime are the hub's,
//!    never the CSR's. The agent then dials in with it (mTLS).

use std::{collections::HashMap, fmt, fmt::Write as _, sync::Mutex, time::Duration};

use rcgen::{
    BasicConstraints, CertificateParams, CertificateSigningRequestParams, DnType, ExtendedKeyUsagePurpose,
    IsCa, Issuer, KeyPair, KeyUsagePurpose, PublicKeyData, SanType, SerialNumber,
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, pem::PemObject};
use sha2::{Digest as _, Sha256};
use time::OffsetDateTime;
use tokio::io::{AsyncRead, AsyncWrite};

use crate::{
    protocol::{FrameError, Message, Refusal, Token, read_frame, write_frame},
    tls::Identity,
};

/// How long after its expiry a redeemed token can still be resumed by the
/// device that redeemed it (an answer lost just before expiry).
pub const RESUME_GRACE: Duration = Duration::from_hours(1);

#[derive(Debug, thiserror::Error)]
pub enum EnrollError {
    #[error("certificate: {0}")]
    Certificate(#[from] rcgen::Error),
    #[error("the certificate from the hub is not valid PEM: {0}")]
    Pem(String),
    #[error(transparent)]
    Frame(#[from] FrameError),
    #[error("the hub refused to enroll this agent ({reason:?}): {message}")]
    Refused { reason: Refusal, message: String },
    #[error("the hub closed the link before answering")]
    Closed,
    #[error("the hub answered with an unexpected message")]
    Unexpected,
}

/// The device id of a public key: `sha256:` of its SubjectPublicKeyInfo.
#[must_use]
pub fn device_id_of(public_key_info: &[u8]) -> String {
    hex_digest(public_key_info)
}

pub(crate) fn hex_digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .fold(String::from("sha256:"), |mut s, b| {
            let _ = write!(s, "{b:02x}");
            s
        })
}

fn cluster_subject(cluster_id: &str) -> String {
    format!("cluster:{cluster_id}")
}

/// The agent's device key, generated where it is used.
pub struct DeviceKey(KeyPair);

impl fmt::Debug for DeviceKey {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("DeviceKey")
            .field("device_id", &self.device_id())
            .finish_non_exhaustive()
    }
}

impl DeviceKey {
    pub fn generate() -> Result<Self, EnrollError> {
        Ok(Self(KeyPair::generate()?))
    }

    pub fn from_pem(pem: &str) -> Result<Self, EnrollError> {
        Ok(Self(KeyPair::from_pem(pem)?))
    }

    #[must_use]
    pub fn to_pem(&self) -> String {
        self.0.serialize_pem()
    }

    /// The public key as a SubjectPublicKeyInfo (DER).
    #[must_use]
    pub fn public_key_info(&self) -> Vec<u8> {
        self.0.subject_public_key_info()
    }

    /// `sha256:` of the public key (its SubjectPublicKeyInfo).
    #[must_use]
    pub fn device_id(&self) -> String {
        hex_digest(&self.0.subject_public_key_info())
    }

    /// A CSR for `cluster_id`, signed by this key.
    pub fn csr_pem(&self, cluster_id: &str) -> Result<String, EnrollError> {
        let mut params = CertificateParams::new(Vec::<String>::new())?;
        params
            .distinguished_name
            .push(DnType::CommonName, cluster_subject(cluster_id));
        Ok(params.serialize_request(&self.0)?.pem()?)
    }

    /// The TLS identity of this key with the certificate the hub issued.
    pub fn identity(&self, certificate_pem: &str) -> Result<Identity, EnrollError> {
        let certificate = CertificateDer::from_pem_slice(certificate_pem.as_bytes())
            .map_err(|e| EnrollError::Pem(e.to_string()))?;
        Ok(Identity {
            chain: vec![certificate],
            key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(self.0.serialize_der())),
        })
    }
}

/// A certificate signing request whose signature was checked: the sender
/// holds the key.
#[derive(Debug)]
pub struct Csr(CertificateSigningRequestParams);

impl Csr {
    pub fn parse(pem: &str) -> Result<Self, EnrollError> {
        Ok(Self(CertificateSigningRequestParams::from_pem(pem)?))
    }

    /// `sha256:` of the requested public key: the device id.
    #[must_use]
    pub fn device_id(&self) -> String {
        hex_digest(&self.0.public_key.subject_public_key_info())
    }
}

/// A certificate the hub issued.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Issued {
    /// The cluster the certificate names.
    pub cluster_id: String,
    pub certificate_pem: String,
    pub device_id: String,
    pub not_after: OffsetDateTime,
}

/// The CA that issues agent identities.
pub struct ClusterCa {
    issuer: Issuer<'static, KeyPair>,
    der: CertificateDer<'static>,
    certificate_pem: String,
    key_pem: String,
}

impl fmt::Debug for ClusterCa {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ClusterCa").finish_non_exhaustive()
    }
}

fn serial() -> SerialNumber {
    let mut bytes: [u8; 16] = rand::random();
    // A positive DER integer.
    bytes[0] &= 0x7f;
    SerialNumber::from(bytes.to_vec())
}

impl ClusterCa {
    /// A new self-signed CA.
    pub fn generate(name: &str) -> Result<Self, EnrollError> {
        let mut params = CertificateParams::new(Vec::<String>::new())?;
        params.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        params.distinguished_name.push(DnType::CommonName, name);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        let key = KeyPair::generate()?;
        let certificate = params.self_signed(&key)?;
        Ok(Self {
            der: certificate.der().clone(),
            certificate_pem: certificate.pem(),
            key_pem: key.serialize_pem(),
            issuer: Issuer::new(params, key),
        })
    }

    /// A CA kept earlier: its certificate and private key in PEM.
    pub fn from_pem(certificate_pem: &str, key_pem: &str) -> Result<Self, EnrollError> {
        let der = CertificateDer::from_pem_slice(certificate_pem.as_bytes())
            .map_err(|e| EnrollError::Pem(e.to_string()))?;
        let issuer = Issuer::from_ca_cert_der(&der, KeyPair::from_pem(key_pem)?)?;
        Ok(Self {
            issuer,
            der,
            certificate_pem: certificate_pem.to_owned(),
            key_pem: key_pem.to_owned(),
        })
    }

    #[must_use]
    pub fn certificate(&self) -> &CertificateDer<'static> {
        &self.der
    }

    #[must_use]
    pub fn certificate_pem(&self) -> &str {
        &self.certificate_pem
    }

    /// The CA's private key; keep it readable by its owner only.
    #[must_use]
    pub fn key_pem(&self) -> &str {
        &self.key_pem
    }

    /// A client certificate for `cluster_id`, bound to the CSR's key and
    /// valid for `lifetime` from `now`. Only the CSR's public key is used.
    pub fn issue(
        &self,
        cluster_id: &str,
        csr: &Csr,
        lifetime: Duration,
        now: OffsetDateTime,
    ) -> Result<Issued, EnrollError> {
        let mut params = CertificateParams::new(Vec::<String>::new())?;
        params
            .distinguished_name
            .push(DnType::CommonName, cluster_subject(cluster_id));
        params.subject_alt_names = vec![SanType::URI(format!("kuben://cluster/{cluster_id}").try_into()?)];
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        params.not_before = now - time::Duration::minutes(5);
        params.not_after = now + lifetime;
        params.serial_number = Some(serial());
        params.use_authority_key_identifier_extension = true;
        let not_after = params.not_after;
        let request = CertificateSigningRequestParams {
            params,
            public_key: csr.0.public_key.clone(),
        };
        Ok(Issued {
            certificate_pem: request.signed_by(&self.issuer)?.pem(),
            cluster_id: cluster_id.to_owned(),
            device_id: csr.device_id(),
            not_after,
        })
    }

    /// A server identity for the hub named `name` (the hub's own key).
    pub fn server_identity(
        &self,
        name: &str,
        lifetime: Duration,
        now: OffsetDateTime,
    ) -> Result<Identity, EnrollError> {
        let mut params = CertificateParams::new(vec![name.to_owned()])?;
        params.distinguished_name.push(DnType::CommonName, "kuben-hub");
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        params.not_before = now - time::Duration::minutes(5);
        params.not_after = now + lifetime;
        params.serial_number = Some(serial());
        let key = KeyPair::generate()?;
        let certificate = params.signed_by(&key, &self.issuer)?;
        Ok(Identity {
            chain: vec![certificate.der().clone()],
            key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        })
    }
}

/// A new bootstrap token: shown to the operator once, stored as its hash.
#[must_use]
pub fn new_token() -> String {
    let bytes: [u8; 32] = rand::random();
    bytes.iter().fold(String::from("kbt_"), |mut s, b| {
        let _ = write!(s, "{b:02x}");
        s
    })
}

/// The hash a token is stored and looked up by.
#[must_use]
pub fn token_hash(token: &str) -> [u8; 32] {
    let mut hash = [0u8; 32];
    hash.copy_from_slice(&Sha256::digest(token.as_bytes()));
    hash
}

/// How a token was redeemed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Redeemed {
    /// For the first time.
    First,
    /// Again, by the device that redeemed it.
    Resumed,
}

/// Why a token was not redeemed; the agent only ever hears
/// [`Refusal::InvalidToken`].
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum TokenRefused {
    #[error("no such token")]
    Unknown,
    #[error("the token has expired")]
    Expired,
    #[error("the token is for another cluster")]
    OtherCluster,
    #[error("the token was redeemed by another device")]
    OtherDevice,
}

/// Where bootstrap tokens live (SQL in the hub; memory in tests).
pub trait TokenStore: Send + Sync {
    /// Redeem the token hashed as `hash` for `device_id` of `cluster_id`.
    fn redeem(
        &self,
        hash: &[u8; 32],
        cluster_id: &str,
        device_id: &str,
        now: OffsetDateTime,
    ) -> impl Future<Output = Result<Redeemed, TokenRefused>> + Send;
}

#[derive(Debug)]
struct TokenRecord {
    cluster_id: String,
    expires_at: OffsetDateTime,
    device_id: Option<String>,
}

/// Tokens in memory, for tests and the hub stub.
#[derive(Debug, Default)]
pub struct MemoryTokens {
    tokens: Mutex<HashMap<[u8; 32], TokenRecord>>,
}

impl MemoryTokens {
    /// A token for `cluster_id`, valid for `ttl` from `now`. Only its hash is
    /// kept; the token itself is returned once.
    pub fn issue(&self, cluster_id: &str, ttl: Duration, now: OffsetDateTime) -> String {
        let token = new_token();
        self.lock().insert(
            token_hash(&token),
            TokenRecord {
                cluster_id: cluster_id.to_owned(),
                expires_at: now + ttl,
                device_id: None,
            },
        );
        token
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<[u8; 32], TokenRecord>> {
        self.tokens
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn redeem_now(
        &self,
        hash: &[u8; 32],
        cluster_id: &str,
        device_id: &str,
        now: OffsetDateTime,
    ) -> Result<Redeemed, TokenRefused> {
        let mut tokens = self.lock();
        let record = tokens.get_mut(hash).ok_or(TokenRefused::Unknown)?;
        if record.cluster_id != cluster_id {
            return Err(TokenRefused::OtherCluster);
        }
        match record.device_id.as_deref() {
            Some(device) if device == device_id => {
                if now >= record.expires_at + RESUME_GRACE {
                    Err(TokenRefused::Expired)
                } else {
                    Ok(Redeemed::Resumed)
                }
            }
            Some(_) => Err(TokenRefused::OtherDevice),
            None if now >= record.expires_at => Err(TokenRefused::Expired),
            None => {
                record.device_id = Some(device_id.to_owned());
                Ok(Redeemed::First)
            }
        }
    }
}

impl TokenStore for MemoryTokens {
    fn redeem(
        &self,
        hash: &[u8; 32],
        cluster_id: &str,
        device_id: &str,
        now: OffsetDateTime,
    ) -> impl Future<Output = Result<Redeemed, TokenRefused>> + Send {
        std::future::ready(self.redeem_now(hash, cluster_id, device_id, now))
    }
}

/// The hub's enrollment service.
#[derive(Debug)]
pub struct Enrollment<T> {
    ca: ClusterCa,
    tokens: T,
    lifetime: Duration,
}

impl<T: TokenStore> Enrollment<T> {
    /// Issues certificates from `ca`, valid for `lifetime`, against `tokens`.
    pub const fn new(ca: ClusterCa, tokens: T, lifetime: Duration) -> Self {
        Self { ca, tokens, lifetime }
    }

    #[must_use]
    pub const fn ca(&self) -> &ClusterCa {
        &self.ca
    }

    #[must_use]
    pub const fn tokens(&self) -> &T {
        &self.tokens
    }

    /// Enroll the device that signed `csr_pem` into `cluster_id`. A CSR that
    /// does not verify never consumes the token.
    pub async fn enroll(
        &self,
        token: &str,
        cluster_id: &str,
        csr_pem: &str,
        now: OffsetDateTime,
    ) -> Result<Issued, Refusal> {
        let csr = Csr::parse(csr_pem).map_err(|_| Refusal::BadRequest)?;
        self.tokens
            .redeem(&token_hash(token), cluster_id, &csr.device_id(), now)
            .await
            .map_err(|_| Refusal::InvalidToken)?;
        self.ca
            .issue(cluster_id, &csr, self.lifetime, now)
            .map_err(|_| Refusal::BadRequest)
    }
}

impl<T> Enrollment<T> {
    /// Renew the certificate of `device_id` in `cluster_id`, asked on the
    /// device's authenticated link: the CSR must be signed by the same
    /// device key.
    pub fn renew(
        &self,
        cluster_id: &str,
        csr_pem: &str,
        device_id: &str,
        now: OffsetDateTime,
    ) -> Result<Issued, Refusal> {
        let csr = Csr::parse(csr_pem).map_err(|_| Refusal::BadRequest)?;
        if csr.device_id() != device_id {
            return Err(Refusal::BadRequest);
        }
        self.ca
            .issue(cluster_id, &csr, self.lifetime, now)
            .map_err(|_| Refusal::BadRequest)
    }
}

/// The hub's side, first half: read an anonymous peer's enrollment request
/// and issue its certificate. A refusal is answered here; an issued
/// certificate is not sent yet, so the hub records the device first and
/// then hands it over ([`answer_enrollment`]): an agent with its
/// certificate links at once, and the hub must already know its device.
pub async fn receive_enrollment<S, T>(
    stream: &mut S,
    enrollment: &Enrollment<T>,
    now: OffsetDateTime,
) -> Result<Option<Issued>, FrameError>
where
    S: AsyncRead + AsyncWrite + Unpin,
    T: TokenStore,
{
    let answer = match read_frame(stream).await? {
        Some(Message::Enroll {
            token,
            cluster_id,
            csr,
        }) => enrollment.enroll(&token.0, &cluster_id, &csr, now).await,
        None => return Ok(None),
        Some(_) => Err(Refusal::BadRequest),
    };
    match answer {
        Ok(issued) => Ok(Some(issued)),
        Err(reason) => {
            let message = match reason {
                Refusal::InvalidToken => "the bootstrap token is not valid for this cluster and device",
                _ => "an anonymous connection may only enroll with a valid CSR",
            };
            write_frame(
                stream,
                &Message::Refused {
                    reason,
                    message: message.into(),
                },
            )
            .await?;
            Ok(None)
        }
    }
}

/// The hub's side, second half: hand the agent its certificate.
pub async fn answer_enrollment<S>(stream: &mut S, issued: &Issued) -> Result<(), FrameError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    write_frame(
        stream,
        &Message::Enrolled {
            certificate: issued.certificate_pem.clone(),
            not_after: issued.not_after.unix_timestamp(),
        },
    )
    .await?;
    Ok(())
}

/// Both halves at once, for a hub that records nothing.
pub async fn serve_enrollment<S, T>(
    stream: &mut S,
    enrollment: &Enrollment<T>,
    now: OffsetDateTime,
) -> Result<Option<Issued>, FrameError>
where
    S: AsyncRead + AsyncWrite + Unpin,
    T: TokenStore,
{
    let Some(issued) = receive_enrollment(stream, enrollment, now).await? else {
        return Ok(None);
    };
    answer_enrollment(stream, &issued).await?;
    Ok(Some(issued))
}

/// What the hub answered an enrollment with.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Enrolled {
    pub certificate_pem: String,
    /// Unix seconds.
    pub not_after: i64,
}

/// The agent's side: enroll `key` into `cluster_id` with `token` over
/// `stream`, a TLS connection to the pinned hub without a client
/// certificate.
pub async fn request_enrollment<S>(
    stream: &mut S,
    token: &str,
    cluster_id: &str,
    key: &DeviceKey,
) -> Result<Enrolled, EnrollError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    write_frame(
        stream,
        &Message::Enroll {
            token: Token(token.to_owned()),
            cluster_id: cluster_id.to_owned(),
            csr: key.csr_pem(cluster_id)?,
        },
    )
    .await?;
    match read_frame(stream).await? {
        Some(Message::Enrolled {
            certificate,
            not_after,
        }) => Ok(Enrolled {
            certificate_pem: certificate,
            not_after,
        }),
        Some(Message::Refused { reason, message }) => Err(EnrollError::Refused { reason, message }),
        Some(_) => Err(EnrollError::Unexpected),
        None => Err(EnrollError::Closed),
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use rustls::{ClientConfig, ServerConfig};
    use tokio::io::{AsyncReadExt, AsyncWriteExt, duplex};
    use tokio_rustls::{TlsAcceptor, TlsConnector};
    use x509_parser::prelude::{FromDer, GeneralName, ParsedExtension, X509Certificate};

    use super::*;
    use crate::tls::{ClientAuth, HUB_NAME, agent_config, hub_config, peer_certificate, server_name};

    const DAY: Duration = Duration::from_hours(24);

    fn now() -> OffsetDateTime {
        OffsetDateTime::from_unix_timestamp(1_789_000_000).expect("time")
    }

    fn enrollment() -> (Enrollment<MemoryTokens>, String) {
        let tokens = MemoryTokens::default();
        let token = tokens.issue("primary", Duration::from_mins(30), now());
        let ca = ClusterCa::generate("kuben cluster CA").expect("ca");
        (Enrollment::new(ca, tokens, DAY), token)
    }

    #[tokio::test]
    async fn a_certificate_is_issued_from_the_key_alone() {
        let (service, token) = enrollment();
        // A CSR that asks for far more than a cluster identity.
        let key = KeyPair::generate().expect("key");
        let mut asked = CertificateParams::new(vec![HUB_NAME.to_owned()]).expect("params");
        asked
            .distinguished_name
            .push(DnType::CommonName, "cluster:someone-else");
        asked.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let csr = asked.serialize_request(&key).expect("csr").pem().expect("pem");

        let issued = service
            .enroll(&token, "primary", &csr, now())
            .await
            .expect("issued");
        let der = CertificateDer::from_pem_slice(issued.certificate_pem.as_bytes()).expect("pem");
        let (_, cert) = X509Certificate::from_der(&der).expect("x509");
        let cn = cert
            .subject()
            .iter_common_name()
            .next()
            .and_then(|c| c.as_str().ok())
            .expect("cn");
        assert_eq!(cn, "cluster:primary");
        let mut uris = Vec::new();
        let mut client_auth_only = false;
        for ext in cert.extensions() {
            match ext.parsed_extension() {
                ParsedExtension::SubjectAlternativeName(san) => {
                    for name in &san.general_names {
                        match name {
                            GeneralName::URI(uri) => uris.push((*uri).to_owned()),
                            other => panic!("unexpected name {other:?}"),
                        }
                    }
                }
                ParsedExtension::ExtendedKeyUsage(eku) => {
                    client_auth_only = eku.client_auth && !eku.server_auth && !eku.any;
                }
                _ => {}
            }
        }
        assert_eq!(uris, ["kuben://cluster/primary"]);
        assert!(client_auth_only, "client authentication only");
        assert_eq!(
            cert.validity().not_after.timestamp(),
            (now() + DAY).unix_timestamp()
        );
        assert_eq!(issued.device_id, hex_digest(&key.subject_public_key_info()));
    }

    #[tokio::test]
    async fn a_token_is_redeemed_once_and_resumed_only_by_its_device() {
        let (service, token) = enrollment();
        let device = DeviceKey::generate().expect("key");
        let csr = device.csr_pem("primary").expect("csr");
        let first = service
            .enroll(&token, "primary", &csr, now())
            .await
            .expect("first");
        let again = service
            .enroll(&token, "primary", &csr, now() + Duration::from_mins(5))
            .await
            .expect("the same device resumes");
        assert_eq!(first.device_id, again.device_id);
        assert_ne!(
            first.certificate_pem, again.certificate_pem,
            "a fresh certificate"
        );

        let other = DeviceKey::generate().expect("key");
        let refused = service
            .enroll(&token, "primary", &other.csr_pem("primary").expect("csr"), now())
            .await;
        assert_eq!(refused, Err(Refusal::InvalidToken), "another device");
        assert_eq!(
            service
                .enroll(
                    &token,
                    "primary",
                    &csr,
                    now() + Duration::from_mins(30) + RESUME_GRACE
                )
                .await,
            Err(Refusal::InvalidToken),
            "resuming ends with the grace period"
        );
    }

    #[tokio::test]
    async fn every_bad_token_gets_the_same_refusal() {
        let (service, token) = enrollment();
        let device = DeviceKey::generate().expect("key");
        let csr = device.csr_pem("primary").expect("csr");
        assert_eq!(
            service.enroll("kbt_unknown", "primary", &csr, now()).await,
            Err(Refusal::InvalidToken)
        );
        assert_eq!(
            service.enroll(&token, "secondary", &csr, now()).await,
            Err(Refusal::InvalidToken),
            "another cluster"
        );
        assert_eq!(
            service
                .enroll(&token, "primary", &csr, now() + Duration::from_mins(31))
                .await,
            Err(Refusal::InvalidToken),
            "expired"
        );
    }

    #[tokio::test]
    async fn a_csr_that_does_not_verify_never_consumes_the_token() {
        let (service, token) = enrollment();
        assert_eq!(
            service.enroll(&token, "primary", "not a csr", now()).await,
            Err(Refusal::BadRequest)
        );
        let device = DeviceKey::generate().expect("key");
        service
            .enroll(&token, "primary", &device.csr_pem("primary").expect("csr"), now())
            .await
            .expect("the token is still unused");
    }

    #[test]
    fn a_renewal_is_only_for_the_device_key_of_the_link() {
        let (service, _token) = enrollment();
        let device = DeviceKey::generate().expect("key");
        let renewed = service
            .renew(
                "primary",
                &device.csr_pem("primary").expect("csr"),
                &device.device_id(),
                now(),
            )
            .expect("renewed");
        assert_eq!(renewed.device_id, device.device_id());
        assert_eq!(renewed.not_after, now() + DAY);
        let other = DeviceKey::generate().expect("key");
        assert_eq!(
            service.renew(
                "primary",
                &other.csr_pem("primary").expect("csr"),
                &device.device_id(),
                now()
            ),
            Err(Refusal::BadRequest),
            "another key"
        );
        assert_eq!(device_id_of(&device.public_key_info()), device.device_id());
    }

    #[test]
    fn a_cluster_ca_survives_its_pem_and_keeps_issuing_for_the_same_trust() {
        let ca = ClusterCa::generate("kuben cluster CA").expect("ca");
        let kept = ClusterCa::from_pem(ca.certificate_pem(), ca.key_pem()).expect("reloaded");
        assert_eq!(kept.certificate(), ca.certificate());
        let device = DeviceKey::generate().expect("key");
        let csr = Csr::parse(&device.csr_pem("primary").expect("csr")).expect("parse");
        let issued = kept.issue("primary", &csr, DAY, now()).expect("issue");
        assert_eq!(issued.cluster_id, "primary");
        let der = CertificateDer::from_pem_slice(issued.certificate_pem.as_bytes()).expect("pem");
        let (_, cert) = X509Certificate::from_der(&der).expect("x509");
        let (_, root) = X509Certificate::from_der(ca.certificate()).expect("root");
        cert.verify_signature(Some(root.public_key()))
            .expect("signed by the original CA's key");
    }

    #[test]
    fn a_device_key_survives_its_pem_and_keeps_its_id() {
        let key = DeviceKey::generate().expect("key");
        let back = DeviceKey::from_pem(&key.to_pem()).expect("pem");
        assert_eq!(back.device_id(), key.device_id());
        assert!(key.device_id().starts_with("sha256:"));
        assert!(!format!("{key:?}").contains("PRIVATE"));
        let token = Message::Enroll {
            token: Token("kbt_secret".into()),
            cluster_id: "primary".into(),
            csr: String::new(),
        };
        assert!(
            !format!("{token:?}").contains("kbt_secret"),
            "a token never shows in Debug"
        );
    }

    /// The hub config for `service`: its CA signs the hub's own certificate
    /// too, so the agent pins that one CA.
    fn hub(service: &Enrollment<MemoryTokens>, auth: ClientAuth) -> Arc<ServerConfig> {
        let identity = service
            .ca()
            .server_identity(HUB_NAME, DAY, OffsetDateTime::now_utc())
            .expect("hub identity");
        hub_config(std::slice::from_ref(service.ca().certificate()), identity, auth).expect("hub config")
    }

    fn agent(service: &Enrollment<MemoryTokens>, identity: Option<Identity>) -> Arc<ClientConfig> {
        agent_config(std::slice::from_ref(service.ca().certificate()), identity).expect("agent config")
    }

    #[tokio::test]
    async fn an_agent_enrolls_anonymously_then_dials_in_with_its_certificate() {
        let tokens = MemoryTokens::default();
        let token = tokens.issue("primary", Duration::from_mins(30), OffsetDateTime::now_utc());
        let service = Arc::new(Enrollment::new(
            ClusterCa::generate("kuben cluster CA").expect("ca"),
            tokens,
            DAY,
        ));
        let device = DeviceKey::generate().expect("key");

        // Anonymous: enroll.
        let (agent_io, hub_io) = duplex(64 * 1024);
        let acceptor = TlsAcceptor::from(hub(&service, ClientAuth::EnrollmentAllowed));
        let hub_service = service.clone();
        let hub_side = tokio::spawn(async move {
            let mut tls = acceptor.accept(hub_io).await.expect("accept");
            assert!(peer_certificate(tls.get_ref().1).is_none(), "anonymous");
            serve_enrollment(&mut tls, &hub_service, OffsetDateTime::now_utc())
                .await
                .expect("serve")
        });
        let mut tls = TlsConnector::from(agent(&service, None))
            .connect(server_name(HUB_NAME).expect("name"), agent_io)
            .await
            .expect("connect");
        let enrolled = request_enrollment(&mut tls, &token, "primary", &device)
            .await
            .expect("enrolled");
        let issued = hub_side.await.expect("join").expect("issued");
        assert_eq!(issued.device_id, device.device_id());
        assert_eq!(enrolled.certificate_pem, issued.certificate_pem);

        // Enrolled: mTLS with the issued certificate.
        let identity = device.identity(&enrolled.certificate_pem).expect("identity");
        let (agent_io, hub_io) = duplex(64 * 1024);
        let acceptor = TlsAcceptor::from(hub(&service, ClientAuth::Required));
        let hub_side = tokio::spawn(async move {
            let mut tls = acceptor.accept(hub_io).await.expect("mTLS accept");
            let peer = peer_certificate(tls.get_ref().1).expect("a client certificate");
            let mut byte = [0u8; 1];
            tls.read_exact(&mut byte).await.expect("read");
            peer
        });
        let mut tls = TlsConnector::from(agent(&service, Some(identity)))
            .connect(server_name(HUB_NAME).expect("name"), agent_io)
            .await
            .expect("mTLS connect");
        tls.write_all(b"x").await.expect("write");
        tls.flush().await.expect("flush");
        let peer = hub_side.await.expect("join");
        let (_, cert) = X509Certificate::from_der(&peer).expect("x509");
        assert_eq!(
            cert.subject()
                .iter_common_name()
                .next()
                .and_then(|c| c.as_str().ok()),
            Some("cluster:primary")
        );
    }

    #[tokio::test]
    async fn a_refused_enrollment_reaches_the_agent_as_a_refusal() {
        let (service, _token) = enrollment();
        let device = DeviceKey::generate().expect("key");
        let (mut agent_io, mut hub_io) = duplex(64 * 1024);
        let hub_side = tokio::spawn(async move { serve_enrollment(&mut hub_io, &service, now()).await });
        let refused = request_enrollment(&mut agent_io, "kbt_wrong", "primary", &device).await;
        assert!(
            matches!(
                refused,
                Err(EnrollError::Refused {
                    reason: Refusal::InvalidToken,
                    ..
                })
            ),
            "{refused:?}"
        );
        assert_eq!(hub_side.await.expect("join").expect("serve"), None);
    }
}

//! TLS for AgentLink (ADR-027, from the M0 spike): TLS 1.3 only, the `ring`
//! provider, mutual authentication.
//!
//! * The agent trusts only the pinned hub CA and checks the hub's name.
//! * The hub accepts client certificates issued by the cluster CA. With
//!   [`ClientAuth::EnrollmentAllowed`] it also accepts a connection without
//!   one: such a peer is anonymous and may only enroll.
//! * Extended key usage keeps the roles apart: a server certificate is never
//!   an agent identity.

use std::sync::Arc;

use rustls::{
    ClientConfig, RootCertStore, ServerConfig, ServerConnection,
    crypto::{CryptoProvider, ring},
    pki_types::{CertificateDer, PrivateKeyDer, ServerName},
    server::WebPkiClientVerifier,
    version::TLS13,
};

/// The name the hub's certificate carries unless configured otherwise.
pub const HUB_NAME: &str = "hub.kuben.internal";

/// A certificate chain and its private key.
#[derive(Debug)]
pub struct Identity {
    pub chain: Vec<CertificateDer<'static>>,
    pub key: PrivateKeyDer<'static>,
}

impl Clone for Identity {
    fn clone(&self) -> Self {
        Self {
            chain: self.chain.clone(),
            key: self.key.clone_key(),
        }
    }
}

/// Whether the hub lets a peer in without a client certificate.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ClientAuth {
    /// Every peer presents a certificate issued by the cluster CA.
    Required,
    /// A peer without a certificate gets in too, anonymous: it may only
    /// enroll.
    EnrollmentAllowed,
}

#[derive(Debug, thiserror::Error)]
pub enum TlsError {
    #[error("TLS: {0}")]
    Rustls(#[from] rustls::Error),
    #[error("client certificate verifier: {0}")]
    Verifier(#[from] rustls::server::VerifierBuilderError),
    #[error("no trusted CA certificate was given")]
    NoTrust,
    #[error("`{0}` is not a valid server name")]
    ServerName(String),
}

/// The crypto provider of both ends.
#[must_use]
pub fn provider() -> Arc<CryptoProvider> {
    Arc::new(ring::default_provider())
}

fn roots(cas: &[CertificateDer<'static>]) -> Result<Arc<RootCertStore>, TlsError> {
    if cas.is_empty() {
        return Err(TlsError::NoTrust);
    }
    let mut store = RootCertStore::empty();
    for ca in cas {
        store.add(ca.clone())?;
    }
    Ok(Arc::new(store))
}

/// The agent's end: trusts `pinned` alone for the hub, and authenticates
/// with `identity` once enrolled (without one, it can only enroll).
pub fn agent_config(
    pinned: &[CertificateDer<'static>],
    identity: Option<Identity>,
) -> Result<Arc<ClientConfig>, TlsError> {
    let builder = ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&TLS13])?
        .with_root_certificates(roots(pinned)?);
    Ok(Arc::new(match identity {
        Some(id) => builder.with_client_auth_cert(id.chain, id.key)?,
        None => builder.with_no_client_auth(),
    }))
}

/// The hub's end: presents `identity`, and accepts client certificates
/// issued by `cluster_cas` (or, with [`ClientAuth::EnrollmentAllowed`], no
/// certificate at all).
pub fn hub_config(
    cluster_cas: &[CertificateDer<'static>],
    identity: Identity,
    auth: ClientAuth,
) -> Result<Arc<ServerConfig>, TlsError> {
    let verifier = WebPkiClientVerifier::builder_with_provider(roots(cluster_cas)?, provider());
    let verifier = match auth {
        ClientAuth::Required => verifier,
        ClientAuth::EnrollmentAllowed => verifier.allow_unauthenticated(),
    }
    .build()?;
    Ok(Arc::new(
        ServerConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&TLS13])?
            .with_client_cert_verifier(verifier)
            .with_single_cert(identity.chain, identity.key)?,
    ))
}

/// The hub's name as the agent checks it.
pub fn server_name(name: &str) -> Result<ServerName<'static>, TlsError> {
    ServerName::try_from(name.to_owned()).map_err(|_| TlsError::ServerName(name.to_owned()))
}

/// The certificate the peer authenticated with; `None` for an anonymous
/// peer (hub side).
#[must_use]
pub fn peer_certificate(conn: &ServerConnection) -> Option<CertificateDer<'static>> {
    conn.peer_certificates()
        .and_then(|chain| chain.first())
        .map(|c| c.clone().into_owned())
}

#[cfg(test)]
mod tests {
    use rcgen::{
        BasicConstraints, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair,
        KeyUsagePurpose,
    };
    use rustls::pki_types::PrivatePkcs8KeyDer;
    use tokio::io::{AsyncReadExt, AsyncWriteExt, duplex};
    use tokio_rustls::{TlsAcceptor, TlsConnector};

    use super::*;

    struct Ca {
        issuer: Issuer<'static, KeyPair>,
        der: CertificateDer<'static>,
    }

    fn ca(name: &str) -> Ca {
        let mut params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.distinguished_name.push(DnType::CommonName, name);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        let key = KeyPair::generate().expect("ca key");
        let der = params.self_signed(&key).expect("self-signed").der().clone();
        Ca {
            issuer: Issuer::new(params, key),
            der,
        }
    }

    fn leaf(ca: &Ca, common_name: &str, sans: &[&str], purpose: ExtendedKeyUsagePurpose) -> Identity {
        let sans = sans.iter().map(|s| (*s).to_owned()).collect::<Vec<_>>();
        let mut params = CertificateParams::new(sans).expect("leaf params");
        params.distinguished_name.push(DnType::CommonName, common_name);
        params.extended_key_usages = vec![purpose];
        let key = KeyPair::generate().expect("leaf key");
        let cert = params.signed_by(&key, &ca.issuer).expect("sign");
        Identity {
            chain: vec![cert.der().clone()],
            key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        }
    }

    fn hub_identity(ca: &Ca) -> Identity {
        leaf(ca, "kuben-hub", &[HUB_NAME], ExtendedKeyUsagePurpose::ServerAuth)
    }

    fn agent_identity(ca: &Ca) -> Identity {
        leaf(ca, "cluster:primary", &[], ExtendedKeyUsagePurpose::ClientAuth)
    }

    /// One round trip; the certificate the hub saw (if any) when both ends
    /// accepted each other.
    async fn exchange(
        hub: Arc<ServerConfig>,
        agent: Arc<ClientConfig>,
    ) -> Result<Option<CertificateDer<'static>>, String> {
        let (agent_io, hub_io) = duplex(64 * 1024);
        let hub_side = tokio::spawn(async move {
            let mut tls = TlsAcceptor::from(hub)
                .accept(hub_io)
                .await
                .map_err(|e| format!("hub: {e}"))?;
            let peer = peer_certificate(tls.get_ref().1);
            let mut hello = [0u8; 5];
            tls.read_exact(&mut hello)
                .await
                .map_err(|e| format!("hub read: {e}"))?;
            tls.write_all(b"pong")
                .await
                .map_err(|e| format!("hub write: {e}"))?;
            tls.shutdown().await.map_err(|e| format!("hub shutdown: {e}"))?;
            Ok::<_, String>(peer)
        });
        let agent_side = async {
            let mut tls = TlsConnector::from(agent)
                .connect(server_name(HUB_NAME).map_err(|e| e.to_string())?, agent_io)
                .await
                .map_err(|e| format!("agent: {e}"))?;
            tls.write_all(b"hello")
                .await
                .map_err(|e| format!("agent write: {e}"))?;
            let mut reply = Vec::new();
            tls.read_to_end(&mut reply)
                .await
                .map_err(|e| format!("agent read: {e}"))?;
            Ok::<_, String>(reply)
        }
        .await;
        let hub_result = hub_side.await.map_err(|e| e.to_string())?;
        match (agent_side, hub_result) {
            (Ok(reply), Ok(peer)) if reply == b"pong" => Ok(peer),
            (Ok(reply), Ok(_)) => Err(format!("unexpected reply {reply:?}")),
            (Err(e), _) | (_, Err(e)) => Err(e),
        }
    }

    fn hub(ca: &Ca, identity: Identity, auth: ClientAuth) -> Arc<ServerConfig> {
        hub_config(std::slice::from_ref(&ca.der), identity, auth).expect("hub config")
    }

    fn agent(pinned: &Ca, identity: Option<Identity>) -> Arc<ClientConfig> {
        agent_config(std::slice::from_ref(&pinned.der), identity).expect("agent config")
    }

    #[tokio::test]
    async fn an_enrolled_agent_and_the_pinned_hub_authenticate_each_other() {
        let root = ca("kuben CA");
        let identity = agent_identity(&root);
        let cert = identity.chain[0].clone();
        let seen = exchange(
            hub(&root, hub_identity(&root), ClientAuth::Required),
            agent(&root, Some(identity)),
        )
        .await
        .expect("mutual TLS");
        assert_eq!(
            seen,
            Some(cert),
            "the hub identifies the agent by its certificate"
        );
    }

    #[tokio::test]
    async fn a_client_certificate_from_another_ca_is_rejected() {
        let root = ca("kuben CA");
        let rogue = ca("rogue CA");
        let result = exchange(
            hub(&root, hub_identity(&root), ClientAuth::EnrollmentAllowed),
            agent(&root, Some(agent_identity(&rogue))),
        )
        .await;
        assert!(result.is_err(), "{result:?}");
    }

    #[tokio::test]
    async fn an_anonymous_peer_gets_in_only_where_enrollment_is_allowed() {
        let root = ca("kuben CA");
        let refused = exchange(
            hub(&root, hub_identity(&root), ClientAuth::Required),
            agent(&root, None),
        )
        .await;
        assert!(refused.is_err(), "{refused:?}");
        let anonymous = exchange(
            hub(&root, hub_identity(&root), ClientAuth::EnrollmentAllowed),
            agent(&root, None),
        )
        .await
        .expect("an anonymous peer may enroll");
        assert_eq!(anonymous, None, "and is known to be anonymous");
    }

    #[tokio::test]
    async fn the_agent_refuses_a_hub_it_has_not_pinned_or_of_another_name() {
        let root = ca("kuben CA");
        let impostor = ca("impostor CA");
        let unpinned = exchange(
            hub(&root, hub_identity(&impostor), ClientAuth::Required),
            agent(&root, Some(agent_identity(&root))),
        )
        .await;
        assert!(unpinned.is_err(), "{unpinned:?}");
        let other_name = leaf(
            &root,
            "kuben-hub",
            &["other.example.com"],
            ExtendedKeyUsagePurpose::ServerAuth,
        );
        let renamed = exchange(
            hub(&root, other_name, ClientAuth::Required),
            agent(&root, Some(agent_identity(&root))),
        )
        .await;
        assert!(renamed.is_err(), "{renamed:?}");
    }

    #[tokio::test]
    async fn a_server_certificate_is_not_an_agent_identity() {
        let root = ca("kuben CA");
        let result = exchange(
            hub(&root, hub_identity(&root), ClientAuth::Required),
            agent(&root, Some(hub_identity(&root))),
        )
        .await;
        assert!(result.is_err(), "{result:?}");
    }

    #[test]
    fn a_configuration_without_a_trusted_ca_is_refused() {
        assert!(matches!(agent_config(&[], None), Err(TlsError::NoTrust)));
        assert!(matches!(server_name("not a name"), Err(TlsError::ServerName(_))));
    }
}

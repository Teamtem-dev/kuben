//! M0 spike (ADR-027): the AgentLink transport.
//!
//! The agent dials the hub over TLS 1.3 with mutual authentication. The agent
//! pins the hub's bootstrap CA; the hub accepts only client certificates
//! issued by that CA and identifies the agent by its certificate. The test
//! runs over an in-memory duplex stream, so it needs no network.

use std::sync::Arc;

use rcgen::{
    BasicConstraints, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair,
    KeyUsagePurpose,
};
use rustls::{
    ClientConfig, RootCertStore, ServerConfig,
    crypto::{CryptoProvider, ring},
    pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName},
    server::WebPkiClientVerifier,
    version::TLS13,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt, duplex};
use tokio_rustls::{TlsAcceptor, TlsConnector};

const HUB_NAME: &str = "hub.kuben.internal";

struct Ca {
    issuer: Issuer<'static, KeyPair>,
    der: CertificateDer<'static>,
}

type Identity = (Vec<CertificateDer<'static>>, PrivateKeyDer<'static>);

fn provider() -> Arc<CryptoProvider> {
    Arc::new(ring::default_provider())
}

fn ca(name: &str) -> Ca {
    let mut params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params.distinguished_name.push(DnType::CommonName, name);
    params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    let key = KeyPair::generate().expect("ca key");
    let der = params.self_signed(&key).expect("self-signed ca").der().clone();
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
    let cert = params.signed_by(&key, &ca.issuer).expect("sign leaf");
    (
        vec![cert.der().clone()],
        PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
    )
}

fn roots(ca: &Ca) -> Arc<RootCertStore> {
    let mut store = RootCertStore::empty();
    store.add(ca.der.clone()).expect("add root");
    Arc::new(store)
}

/// The hub: TLS 1.3 only, client certificate required and issued by `trust`.
fn hub_config(trust: &Ca, identity: Identity) -> Arc<ServerConfig> {
    let verifier = WebPkiClientVerifier::builder_with_provider(roots(trust), provider())
        .build()
        .expect("client verifier");
    Arc::new(
        ServerConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&TLS13])
            .expect("tls 1.3")
            .with_client_cert_verifier(verifier)
            .with_single_cert(identity.0, identity.1)
            .expect("hub certificate"),
    )
}

/// The agent: pins `pinned` as the only trusted CA for the hub.
fn agent_config(pinned: &Ca, identity: Option<Identity>) -> Arc<ClientConfig> {
    let builder = ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&TLS13])
        .expect("tls 1.3")
        .with_root_certificates(roots(pinned));
    Arc::new(match identity {
        Some((chain, key)) => builder
            .with_client_auth_cert(chain, key)
            .expect("agent certificate"),
        None => builder.with_no_client_auth(),
    })
}

/// One request/response over mTLS. `Ok((reply, agent_cert))` only when both
/// sides authenticated each other.
async fn exchange(
    hub: Arc<ServerConfig>,
    agent: Arc<ClientConfig>,
) -> Result<(String, CertificateDer<'static>), String> {
    let (agent_io, hub_io) = duplex(64 * 1024);
    let acceptor = TlsAcceptor::from(hub);
    let connector = TlsConnector::from(agent);

    let hub_side = tokio::spawn(async move {
        let mut tls = acceptor.accept(hub_io).await.map_err(|e| format!("hub: {e}"))?;
        let peer = tls
            .get_ref()
            .1
            .peer_certificates()
            .and_then(|chain| chain.first())
            .cloned()
            .ok_or("hub: no client certificate")?;
        let mut hello = [0u8; 5];
        tls.read_exact(&mut hello)
            .await
            .map_err(|e| format!("hub read: {e}"))?;
        tls.write_all(b"pong")
            .await
            .map_err(|e| format!("hub write: {e}"))?;
        tls.shutdown().await.map_err(|e| format!("hub shutdown: {e}"))?;
        Ok::<_, String>(peer.into_owned())
    });

    let agent_side = async {
        let name = ServerName::try_from(HUB_NAME).map_err(|e| e.to_string())?;
        let mut tls = connector
            .connect(name, agent_io)
            .await
            .map_err(|e| format!("agent: {e}"))?;
        tls.write_all(b"hello")
            .await
            .map_err(|e| format!("agent write: {e}"))?;
        let mut reply = Vec::new();
        tls.read_to_end(&mut reply)
            .await
            .map_err(|e| format!("agent read: {e}"))?;
        Ok::<_, String>(String::from_utf8_lossy(&reply).into_owned())
    }
    .await;

    let hub_result = hub_side.await.map_err(|e| e.to_string())?;
    match (agent_side, hub_result) {
        (Ok(reply), Ok(peer)) => Ok((reply, peer)),
        (Err(e), _) | (_, Err(e)) => Err(e),
    }
}

fn hub_identity(ca: &Ca) -> Identity {
    leaf(ca, "kuben-hub", &[HUB_NAME], ExtendedKeyUsagePurpose::ServerAuth)
}

fn agent_identity(ca: &Ca, cluster: &str) -> Identity {
    leaf(
        ca,
        &format!("cluster:{cluster}"),
        &[],
        ExtendedKeyUsagePurpose::ClientAuth,
    )
}

#[tokio::test]
async fn an_enrolled_agent_and_the_pinned_hub_authenticate_each_other() {
    let bootstrap = ca("kuben bootstrap CA");
    let agent = agent_identity(&bootstrap, "primary");
    let agent_cert = agent.0[0].clone();

    let (reply, seen) = exchange(
        hub_config(&bootstrap, hub_identity(&bootstrap)),
        agent_config(&bootstrap, Some(agent)),
    )
    .await
    .expect("mutual TLS succeeds");

    assert_eq!(reply, "pong");
    assert_eq!(
        seen, agent_cert,
        "the hub identifies the agent by its certificate"
    );
}

#[tokio::test]
async fn a_client_certificate_from_another_ca_is_rejected() {
    let bootstrap = ca("kuben bootstrap CA");
    let rogue = ca("rogue CA");
    let result = exchange(
        hub_config(&bootstrap, hub_identity(&bootstrap)),
        agent_config(&bootstrap, Some(agent_identity(&rogue, "primary"))),
    )
    .await;
    assert!(result.is_err(), "unknown client CA must fail: {result:?}");
}

#[tokio::test]
async fn an_agent_without_a_certificate_is_rejected() {
    let bootstrap = ca("kuben bootstrap CA");
    let result = exchange(
        hub_config(&bootstrap, hub_identity(&bootstrap)),
        agent_config(&bootstrap, None),
    )
    .await;
    assert!(result.is_err(), "anonymous agents must fail: {result:?}");
}

#[tokio::test]
async fn the_agent_refuses_a_hub_not_signed_by_the_pinned_ca() {
    let bootstrap = ca("kuben bootstrap CA");
    let impostor = ca("impostor CA");
    let result = exchange(
        hub_config(&bootstrap, hub_identity(&impostor)),
        agent_config(&bootstrap, Some(agent_identity(&bootstrap, "primary"))),
    )
    .await;
    assert!(
        result.is_err(),
        "the agent must not talk to an unpinned hub: {result:?}"
    );
}

#[tokio::test]
async fn the_agent_refuses_a_hub_certificate_for_another_name() {
    let bootstrap = ca("kuben bootstrap CA");
    let wrong_name = leaf(
        &bootstrap,
        "kuben-hub",
        &["other.example.com"],
        ExtendedKeyUsagePurpose::ServerAuth,
    );
    let result = exchange(
        hub_config(&bootstrap, wrong_name),
        agent_config(&bootstrap, Some(agent_identity(&bootstrap, "primary"))),
    )
    .await;
    assert!(result.is_err(), "hostname verification must hold: {result:?}");
}

#[tokio::test]
async fn a_server_certificate_cannot_be_used_as_an_agent_identity() {
    // Extended key usage keeps roles apart: a stolen hub certificate is not
    // a cluster identity.
    let bootstrap = ca("kuben bootstrap CA");
    let as_agent = leaf(
        &bootstrap,
        "kuben-hub",
        &[HUB_NAME],
        ExtendedKeyUsagePurpose::ServerAuth,
    );
    let result = exchange(
        hub_config(&bootstrap, hub_identity(&bootstrap)),
        agent_config(&bootstrap, Some(as_agent)),
    )
    .await;
    assert!(
        result.is_err(),
        "serverAuth-only certificates must not authenticate agents: {result:?}"
    );
}

//! AgentLink end to end, in memory: the agent's link loop against the hub
//! over TLS on duplex streams (no sockets, so it runs anywhere).

use std::{
    collections::BTreeSet,
    io,
    sync::{
        Arc,
        atomic::{AtomicU32, Ordering},
    },
    time::Duration,
};

use kuben_agent::{
    enroll::{ClusterCa, Csr, DeviceKey, Enrollment, MemoryTokens, request_enrollment},
    hub::{Hub, HubSettings, MemoryRegistry, Registry, SessionInfo},
    link::{Connector, Credentials, Executor, Lifetime, LinkConfig, Renewal, Reports, run},
    protocol::{APPLICATION_RUNTIME, Apply, Message, Observation, RuntimePhase, read_frame, write_frame},
    state::{HubAddress, State, StateError, ensure_identity},
    tls::{ClientAuth, HUB_NAME, Identity, agent_config, hub_config, server_name},
};
use time::OffsetDateTime;
use tokio::io::{DuplexStream, duplex};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tokio_util::sync::CancellationToken;

const DAY: Duration = Duration::from_hours(24);

type TestHub = Hub<MemoryTokens, MemoryRegistry>;

struct World {
    hub: Arc<TestHub>,
    acceptor: TlsAcceptor,
    pinned: rustls::pki_types::CertificateDer<'static>,
}

fn world(settings: HubSettings) -> Arc<World> {
    let ca = ClusterCa::generate("kuben cluster CA").expect("ca");
    let identity = ca
        .server_identity(HUB_NAME, DAY, OffsetDateTime::now_utc())
        .expect("hub identity");
    let pinned = ca.certificate().clone();
    let acceptor = TlsAcceptor::from(
        hub_config(
            std::slice::from_ref(&pinned),
            identity,
            ClientAuth::EnrollmentAllowed,
        )
        .expect("hub config"),
    );
    let hub = Arc::new(Hub::new(
        Enrollment::new(ca, MemoryTokens::default(), DAY),
        MemoryRegistry::default(),
        settings,
    ));
    Arc::new(World {
        hub,
        acceptor,
        pinned,
    })
}

/// An identity the hub's CA issued for `cluster`, recorded as the cluster's
/// agent device (as enrollment records it).
async fn issued_identity(world: &World, cluster: &str) -> Identity {
    let device = DeviceKey::generate().expect("key");
    let csr = Csr::parse(&device.csr_pem(cluster).expect("csr")).expect("parse");
    let issued = world
        .hub
        .enrollment()
        .ca()
        .issue(cluster, &csr, DAY, OffsetDateTime::now_utc())
        .expect("issue");
    world
        .hub
        .registry()
        .certified(cluster, &issued.device_id, issued.not_after)
        .await;
    device.identity(&issued.certificate_pem).expect("identity")
}

fn config(world: &World, cluster: &str, identity: Identity) -> LinkConfig {
    let credentials = Credentials::new(vec![world.pinned.clone()], identity, None).expect("credentials");
    let mut config = LinkConfig::new(
        cluster,
        server_name(HUB_NAME).expect("name"),
        Arc::new(credentials),
    );
    config.min_backoff = Duration::from_millis(10);
    config.max_backoff = Duration::from_millis(50);
    config
}

/// Every dial is a duplex pair whose other end the hub serves; the first
/// `fail_first` dials fail as refused connections.
struct Dialer {
    world: Arc<World>,
    dials: AtomicU32,
    fail_first: u32,
}

impl Dialer {
    fn new(world: &Arc<World>, fail_first: u32) -> Arc<Self> {
        Arc::new(Self {
            world: world.clone(),
            dials: AtomicU32::new(0),
            fail_first,
        })
    }

    fn dials(&self) -> u32 {
        self.dials.load(Ordering::SeqCst)
    }
}

impl Connector for Dialer {
    type Stream = DuplexStream;

    fn connect(&self) -> impl Future<Output = io::Result<DuplexStream>> + Send {
        let n = self.dials.fetch_add(1, Ordering::SeqCst);
        let world = self.world.clone();
        let fail = n < self.fail_first;
        async move {
            if fail {
                return Err(io::Error::from(io::ErrorKind::ConnectionRefused));
            }
            let (agent, hub) = duplex(64 * 1024);
            tokio::spawn(async move {
                if let Ok(tls) = world.acceptor.accept(hub).await {
                    let _ = world.hub.serve(tls).await;
                }
            });
            Ok(agent)
        }
    }
}

/// Wait up to ten seconds for `check`.
async fn eventually(what: &str, mut check: impl FnMut() -> bool) {
    for _ in 0..1000 {
        if check() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("timed out waiting for {what}");
}

fn start<C: Connector + 'static>(
    dialer: Arc<C>,
    config: LinkConfig,
) -> (CancellationToken, tokio::task::JoinHandle<()>) {
    let token = CancellationToken::new();
    let stop = token.clone();
    let task = tokio::spawn(async move { run(dialer.as_ref(), &config, &stop).await });
    (token, task)
}

fn features(items: &[&str]) -> BTreeSet<String> {
    items.iter().map(|s| (*s).to_owned()).collect()
}

fn heartbeats(world: &World, cluster: &str) -> u64 {
    world
        .hub
        .session(cluster)
        .map_or(0, |s: SessionInfo| s.heartbeats)
}

fn quick() -> HubSettings {
    HubSettings {
        heartbeat: Duration::from_millis(20),
        ..HubSettings::default()
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_enrolled_agent_links_and_keeps_the_link_alive() {
    let world = world(HubSettings {
        features: features(&["applicationRuntime", "executionTask"]),
        ..quick()
    });
    let mut config = config(&world, "primary", issued_identity(&world, "primary").await);
    config.capabilities = features(&["executionTask", "logs"]);
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(dialer.clone(), config);

    eventually("three heartbeats", || heartbeats(&world, "primary") >= 3).await;
    let session = world.hub.session("primary").expect("session");
    assert_eq!(
        session.features,
        features(&["executionTask"]),
        "features both know"
    );
    assert_eq!(session.agent_version, env!("CARGO_PKG_VERSION"));
    token.cancel();
    task.await.expect("the link loop ends when cancelled");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_agent_dials_again_after_failed_dials() {
    let world = world(quick());
    let dialer = Dialer::new(&world, 2);
    let identity = issued_identity(&world, "primary").await;
    let (token, task) = start(dialer.clone(), config(&world, "primary", identity));
    eventually("a link after two refused dials", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    assert!(dialer.dials() >= 3, "{} dials", dialer.dials());
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_revoked_device_is_refused_and_the_agent_keeps_trying() {
    let world = world(HubSettings::default());
    let identity = issued_identity(&world, "primary").await;
    world.hub.registry().revoke("primary");
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(dialer.clone(), config(&world, "primary", identity));
    eventually("three dials", || dialer.dials() >= 3).await;
    assert!(world.hub.session("primary").is_none(), "never linked");
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_device_a_newer_enrollment_replaced_is_refused() {
    let world = world(HubSettings::default());
    let old = issued_identity(&world, "primary").await;
    let _new = issued_identity(&world, "primary").await;
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(dialer.clone(), config(&world, "primary", old));
    eventually("two dials", || dialer.dials() >= 2).await;
    assert!(
        world.hub.session("primary").is_none(),
        "a still valid certificate of a replaced device"
    );
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_hello_for_another_cluster_than_the_certificate_is_refused() {
    let world = world(HubSettings::default());
    let dialer = Dialer::new(&world, 0);
    let identity = issued_identity(&world, "primary").await;
    let (token, task) = start(dialer.clone(), config(&world, "other", identity));
    eventually("two dials", || dialer.dials() >= 2).await;
    assert!(world.hub.session("other").is_none());
    assert!(world.hub.session("primary").is_none());
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_agent_without_a_common_protocol_version_is_refused() {
    let world = world(HubSettings {
        versions: 90..=91,
        ..HubSettings::default()
    });
    let dialer = Dialer::new(&world, 0);
    let identity = issued_identity(&world, "primary").await;
    let (token, task) = start(dialer.clone(), config(&world, "primary", identity));
    eventually("two dials", || dialer.dials() >= 2).await;
    assert!(world.hub.session("primary").is_none());
    token.cancel();
    task.await.expect("join");
}

/// A hub that welcomes the agent and then never answers.
struct Silent {
    world: Arc<World>,
    dials: AtomicU32,
}

impl Connector for Silent {
    type Stream = DuplexStream;

    fn connect(&self) -> impl Future<Output = io::Result<DuplexStream>> + Send {
        self.dials.fetch_add(1, Ordering::SeqCst);
        let acceptor = self.world.acceptor.clone();
        async move {
            let (agent, hub) = duplex(64 * 1024);
            tokio::spawn(async move {
                let Ok(mut tls) = acceptor.accept(hub).await else {
                    return;
                };
                let _hello = read_frame(&mut tls).await;
                let _ = write_frame(
                    &mut tls,
                    &Message::Welcome {
                        protocol_version: 1,
                        hub_version: "silent".into(),
                        heartbeat_ms: 20,
                        features: BTreeSet::new(),
                    },
                )
                .await;
                while let Ok(Some(_)) = read_frame(&mut tls).await {}
            });
            Ok(agent)
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_hub_that_stops_answering_is_left_and_dialed_again() {
    let world = world(HubSettings::default());
    let silent = Arc::new(Silent {
        world: world.clone(),
        dials: AtomicU32::new(0),
    });
    let identity = issued_identity(&world, "primary").await;
    let (token, task) = start(silent.clone(), config(&world, "primary", identity));
    eventually("a second dial after the heartbeat timeout", || {
        silent.dials.load(Ordering::SeqCst) >= 2
    })
    .await;
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_agent_enrolls_through_the_hub_and_then_links() {
    let world = world(quick());
    let token =
        world
            .hub
            .enrollment()
            .tokens()
            .issue("primary", Duration::from_mins(30), OffsetDateTime::now_utc());
    let dialer = Dialer::new(&world, 0);
    let device = DeviceKey::generate().expect("key");

    let raw = dialer.connect().await.expect("dial");
    let anonymous = agent_config(std::slice::from_ref(&world.pinned), None).expect("config");
    let mut tls = TlsConnector::from(anonymous)
        .connect(server_name(HUB_NAME).expect("name"), raw)
        .await
        .expect("TLS without a client certificate");
    let enrolled = request_enrollment(&mut tls, &token, "primary", &device)
        .await
        .expect("enrolled through the hub");

    let identity = device.identity(&enrolled.certificate_pem).expect("identity");
    let (stop, task) = start(dialer.clone(), config(&world, "primary", identity));
    eventually("a link with the enrolled identity", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    stop.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_agent_enrolls_once_and_then_uses_its_stored_identity() {
    let world = world(quick());
    let now = OffsetDateTime::now_utc();
    let token = world
        .hub
        .enrollment()
        .tokens()
        .issue("primary", Duration::from_mins(30), now);
    let dir = std::env::temp_dir().join(format!(
        "kuben-agent-link-{}-{}",
        std::process::id(),
        now.unix_timestamp_nanos()
    ));
    let state = State::open(&dir).expect("state");
    let key = state.device_key().expect("key");
    let dialer = Dialer::new(&world, 0);
    let pinned = [world.pinned.clone()];
    let name = server_name(HUB_NAME).expect("name");
    let hub = HubAddress {
        connector: dialer.as_ref(),
        pinned: &pinned,
        name: &name,
    };

    let without = ensure_identity(&state, &key, &hub, "primary", || Ok(None), now).await;
    assert!(matches!(without, Err(StateError::NeedsEnrollment)), "{without:?}");
    ensure_identity(&state, &key, &hub, "primary", || Ok(Some(token.clone())), now)
        .await
        .expect("enrolled");
    let stored = ensure_identity(
        &state,
        &key,
        &hub,
        "primary",
        || panic!("an enrolled agent never reads a token"),
        now,
    )
    .await
    .expect("the stored identity");
    let later = ensure_identity(&state, &key, &hub, "primary", || Ok(None), now + DAY + DAY).await;
    assert!(
        matches!(later, Err(StateError::NeedsEnrollment)),
        "an expired certificate"
    );

    let (stop, task) = start(dialer.clone(), config(&world, "primary", stored));
    eventually("a link with the stored identity", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    stop.cancel();
    task.await.expect("join");
    std::fs::remove_dir_all(&dir).ok();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_agent_renews_its_certificate_over_the_link_once() {
    let world = world(quick());
    let key = Arc::new(DeviceKey::generate().expect("key"));
    let now = OffsetDateTime::now_utc();
    let csr = Csr::parse(&key.csr_pem("primary").expect("csr")).expect("parse");
    // A minute of life left: the renewal is due at once.
    let issued = world
        .hub
        .enrollment()
        .ca()
        .issue("primary", &csr, Duration::from_mins(1), now)
        .expect("issue");
    world
        .hub
        .registry()
        .certified("primary", &issued.device_id, issued.not_after)
        .await;
    let first = Lifetime {
        not_before: now - time::Duration::minutes(5),
        not_after: issued.not_after,
    };
    let credentials = Arc::new(
        Credentials::new(
            vec![world.pinned.clone()],
            key.identity(&issued.certificate_pem).expect("identity"),
            Some(first),
        )
        .expect("credentials"),
    );
    let stored = Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
    let keep = stored.clone();
    let mut config = LinkConfig::new(
        "primary",
        server_name(HUB_NAME).expect("name"),
        credentials.clone(),
    );
    config.min_backoff = Duration::from_millis(10);
    config.max_backoff = Duration::from_millis(50);
    config.renewal = Some(Renewal {
        key: key.clone(),
        store: Arc::new(move |pem: &str| {
            keep.lock().expect("lock").push(pem.to_owned());
            Ok(())
        }),
    });
    let dialer = Dialer::new(&world, 0);
    let (stop, task) = start(dialer.clone(), config);

    eventually("a renewed certificate", || {
        !stored.lock().expect("lock").is_empty()
    })
    .await;
    let renewed = credentials.lifetime().expect("lifetime");
    assert!(
        renewed.not_after > first.not_after + Duration::from_hours(1),
        "the hub's day-long certificate replaced the old one"
    );
    let at_renewal = heartbeats(&world, "primary");
    eventually("more heartbeats", || {
        heartbeats(&world, "primary") >= at_renewal + 5
    })
    .await;
    assert_eq!(stored.lock().expect("lock").len(), 1, "renewed once, not again");
    stop.cancel();
    task.await.expect("join");
}

/// An executor that records envelopes and reports them ready.
#[derive(Debug, Default)]
struct Fake {
    applied: std::sync::Mutex<Vec<Apply>>,
}

impl Executor for Fake {
    fn apply(&self, apply: Apply, reports: Reports) -> std::pin::Pin<Box<dyn Future<Output = ()> + Send>> {
        self.applied.lock().expect("lock").push(apply.clone());
        Box::pin(async move {
            let generation = serde_json::from_str::<serde_json::Value>(&apply.spec)
                .ok()
                .and_then(|v| v["generation"].as_i64())
                .unwrap_or(0);
            for phase in [RuntimePhase::Accepted, RuntimePhase::Ready] {
                let _ = reports
                    .send(Observation {
                        target: apply.target.clone(),
                        generation,
                        phase,
                        reason: None,
                        message: None,
                    })
                    .await;
            }
        })
    }
}

fn envelope(generation: i64) -> Apply {
    Apply {
        target: "0199a0c0-0000-7000-8000-000000000001".into(),
        namespace: "kb-shop-prod".into(),
        name: "web".into(),
        spec: format!(r#"{{"generation":{generation}}}"#),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_hub_hands_an_agent_an_envelope_and_hears_how_it_went() {
    let world = world(HubSettings {
        features: features(&[APPLICATION_RUNTIME]),
        ..quick()
    });
    let fake = Arc::new(Fake::default());
    let mut config = config(&world, "primary", issued_identity(&world, "primary").await);
    config.capabilities = features(&[APPLICATION_RUNTIME]);
    config.executor = Some(fake.clone());
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(dialer.clone(), config);
    eventually("a link", || heartbeats(&world, "primary") >= 1).await;

    assert!(
        world.hub.send("primary", envelope(5)).await,
        "sent over the live link"
    );
    eventually("a ready observation", || {
        world
            .hub
            .registry()
            .observations("primary")
            .iter()
            .any(|o| o.phase == RuntimePhase::Ready && o.generation == 5)
    })
    .await;
    assert_eq!(fake.applied.lock().expect("lock").len(), 1);
    assert!(
        !world.hub.send("elsewhere", envelope(5)).await,
        "no link to that cluster"
    );
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn envelopes_need_the_negotiated_feature() {
    let world = world(quick());
    let mut config = config(&world, "primary", issued_identity(&world, "primary").await);
    config.capabilities = features(&[APPLICATION_RUNTIME]);
    config.executor = Some(Arc::new(Fake::default()));
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(dialer.clone(), config);
    eventually("a link", || heartbeats(&world, "primary") >= 1).await;
    assert!(
        !world.hub.send("primary", envelope(5)).await,
        "the hub did not offer the feature"
    );
    token.cancel();
    task.await.expect("join");
}

/// A hub that sends an envelope right after a Welcome without features and
/// keeps what the agent answers.
struct Pushy {
    world: Arc<World>,
    answer: Arc<std::sync::Mutex<Option<Observation>>>,
}

impl Connector for Pushy {
    type Stream = DuplexStream;

    fn connect(&self) -> impl Future<Output = io::Result<DuplexStream>> + Send {
        let (acceptor, answer) = (self.world.acceptor.clone(), self.answer.clone());
        async move {
            let (agent, hub) = duplex(64 * 1024);
            tokio::spawn(async move {
                let Ok(mut tls) = acceptor.accept(hub).await else {
                    return;
                };
                let _hello = read_frame(&mut tls).await;
                let welcome = Message::Welcome {
                    protocol_version: 1,
                    hub_version: "pushy".into(),
                    heartbeat_ms: 20,
                    features: BTreeSet::new(),
                };
                if write_frame(&mut tls, &welcome).await.is_err()
                    || write_frame(&mut tls, &Message::Apply(envelope(5))).await.is_err()
                {
                    return;
                }
                while let Ok(Some(message)) = read_frame(&mut tls).await {
                    match message {
                        Message::Heartbeat { seq } => {
                            let _ = write_frame(&mut tls, &Message::HeartbeatAck { seq }).await;
                        }
                        Message::Observed(observation) => {
                            *answer.lock().expect("lock") = Some(observation);
                        }
                        _ => {}
                    }
                }
            });
            Ok(agent)
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_agent_rejects_an_envelope_outside_the_negotiated_features() {
    let world = world(HubSettings::default());
    let pushy = Arc::new(Pushy {
        world: world.clone(),
        answer: Arc::default(),
    });
    let fake = Arc::new(Fake::default());
    let mut config = config(&world, "primary", issued_identity(&world, "primary").await);
    config.executor = Some(fake.clone());
    let (token, task) = start(pushy.clone(), config);
    eventually("an answer", || pushy.answer.lock().expect("lock").is_some()).await;
    let answer = pushy.answer.lock().expect("lock").clone().expect("answer");
    assert_eq!(answer.phase, RuntimePhase::Rejected);
    assert_eq!(answer.reason.as_deref(), Some("UnsupportedCapability"));
    assert!(fake.applied.lock().expect("lock").is_empty(), "never applied");
    token.cancel();
    task.await.expect("join");
}

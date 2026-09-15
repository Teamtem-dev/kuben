//! AgentLink end to end, in memory: the agent's link loop against the hub
//! stub over TLS on duplex streams (no sockets, so it runs anywhere).

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
    hub::{Hub, HubSettings, SessionInfo},
    link::{Connector, LinkConfig, run},
    protocol::{Message, read_frame, write_frame},
    state::{HubAddress, State, StateError, ensure_identity},
    tls::{ClientAuth, HUB_NAME, Identity, agent_config, hub_config, server_name},
};
use time::OffsetDateTime;
use tokio::io::{DuplexStream, duplex};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tokio_util::sync::CancellationToken;

const DAY: Duration = Duration::from_hours(24);

struct World {
    hub: Arc<Hub<MemoryTokens>>,
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
        settings,
    ));
    Arc::new(World {
        hub,
        acceptor,
        pinned,
    })
}

fn issued_identity(world: &World, cluster: &str) -> Identity {
    let device = DeviceKey::generate().expect("key");
    let csr = Csr::parse(&device.csr_pem(cluster).expect("csr")).expect("parse");
    let issued = world
        .hub
        .enrollment()
        .ca()
        .issue(cluster, &csr, DAY, OffsetDateTime::now_utc())
        .expect("issue");
    device.identity(&issued.certificate_pem).expect("identity")
}

fn config(world: &World, cluster: &str, identity: Option<Identity>) -> LinkConfig {
    let tls = agent_config(std::slice::from_ref(&world.pinned), identity).expect("agent config");
    let mut config = LinkConfig::new(cluster, server_name(HUB_NAME).expect("name"), tls);
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

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_enrolled_agent_links_and_keeps_the_link_alive() {
    let world = world(HubSettings {
        features: features(&["applicationRuntime", "executionTask"]),
        heartbeat: Duration::from_millis(20),
        ..HubSettings::default()
    });
    let mut config = config(&world, "primary", Some(issued_identity(&world, "primary")));
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
    let world = world(HubSettings {
        heartbeat: Duration::from_millis(20),
        ..HubSettings::default()
    });
    let dialer = Dialer::new(&world, 2);
    let (token, task) = start(
        dialer.clone(),
        config(&world, "primary", Some(issued_identity(&world, "primary"))),
    );
    eventually("a link after two refused dials", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    assert!(dialer.dials() >= 3, "{} dials", dialer.dials());
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_revoked_cluster_is_refused_and_the_agent_keeps_trying() {
    let world = world(HubSettings::default());
    world.hub.revoke("primary");
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(
        dialer.clone(),
        config(&world, "primary", Some(issued_identity(&world, "primary"))),
    );
    eventually("three dials", || dialer.dials() >= 3).await;
    assert!(world.hub.session("primary").is_none(), "never linked");
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_hello_for_another_cluster_than_the_certificate_is_refused() {
    let world = world(HubSettings::default());
    let dialer = Dialer::new(&world, 0);
    let (token, task) = start(
        dialer.clone(),
        config(&world, "other", Some(issued_identity(&world, "primary"))),
    );
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
    let (token, task) = start(
        dialer.clone(),
        config(&world, "primary", Some(issued_identity(&world, "primary"))),
    );
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
    let (token, task) = start(
        silent.clone(),
        config(&world, "primary", Some(issued_identity(&world, "primary"))),
    );
    eventually("a second dial after the heartbeat timeout", || {
        silent.dials.load(Ordering::SeqCst) >= 2
    })
    .await;
    token.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_agent_enrolls_through_the_hub_and_then_links() {
    let world = world(HubSettings {
        heartbeat: Duration::from_millis(20),
        ..HubSettings::default()
    });
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
    let (stop, task) = start(dialer.clone(), config(&world, "primary", Some(identity)));
    eventually("a link with the enrolled identity", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    stop.cancel();
    task.await.expect("join");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_agent_enrolls_once_and_then_uses_its_stored_identity() {
    let world = world(HubSettings {
        heartbeat: Duration::from_millis(20),
        ..HubSettings::default()
    });
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

    let (stop, task) = start(dialer.clone(), config(&world, "primary", Some(stored)));
    eventually("a link with the stored identity", || {
        heartbeats(&world, "primary") >= 1
    })
    .await;
    stop.cancel();
    task.await.expect("join");
    std::fs::remove_dir_all(&dir).ok();
}

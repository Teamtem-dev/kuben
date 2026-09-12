//! Shared API state.

use std::{sync::Arc, time::Duration};

use kuben_core::{config::Config, ids::UserId, traits::PolicyEngine};
use kuben_platform::{health::Health, projection::Projections, registry::ClusterRegistry};
use kuben_store::Store;
use tokio::sync::Semaphore;

use crate::auth::{password::Hasher, throttle::LoginThrottle};

#[derive(Clone)]
pub struct ApiState {
    pub cfg: Arc<Config>,
    pub store: Store,
    /// `None` when no cluster is configured (setup / dev mode).
    pub cluster: Option<ClusterRegistry>,
    pub projections: Arc<Projections>,
    pub health: Health,
    pub policy: Arc<dyn PolicyEngine>,
    pub hasher: Arc<Hasher>,
    /// Argon2id is memory-hard; bound concurrent verifications.
    pub login_permits: Arc<Semaphore>,
    /// Short-TTL session cache: `sha256(session_id)` → user id.
    pub session_cache: moka::future::Cache<Vec<u8>, UserId>,
    /// Brute-force protection for `/auth/login` (scenario 1).
    pub throttle: LoginThrottle,
    /// `POST /setup` runs one at a time: exactly one first admin.
    pub setup_lock: Arc<tokio::sync::Mutex<()>>,
}

impl std::fmt::Debug for ApiState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ApiState")
            .field("cluster", &self.cluster.is_some())
            .finish_non_exhaustive()
    }
}

impl ApiState {
    #[must_use]
    pub fn new(
        cfg: Config,
        store: Store,
        cluster: Option<ClusterRegistry>,
        projections: Arc<Projections>,
        health: Health,
        policy: Arc<dyn PolicyEngine>,
    ) -> Self {
        let session_cache = moka::future::Cache::builder()
            .time_to_live(Duration::from_secs(cfg.security.session_cache_ttl_secs))
            .max_capacity(10_000)
            .build();
        let hasher = Arc::new(Hasher::from_config(&cfg.security));
        let login_permits = Arc::new(Semaphore::new(cfg.security.login_concurrency.max(1)));
        let throttle = LoginThrottle::new(&cfg.security, store.clone());
        Self {
            cfg: Arc::new(cfg),
            store,
            cluster,
            projections,
            health,
            policy,
            hasher,
            login_permits,
            session_cache,
            throttle,
            setup_lock: Arc::default(),
        }
    }
}

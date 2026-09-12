//! Read models ("projections"). Informers translate Kubernetes objects into
//! small UI-shaped views (a `PodView` is ~200 bytes vs several KiB for a raw
//! Pod), keep them in memory, and publish deltas on a bounded broadcast
//! channel that SSE clients consume (ADR-004).

pub mod informer;
mod views;

use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};

use dashmap::DashMap;
use serde::Serialize;
use tokio::sync::{broadcast, watch};
pub use views::{
    AppView, EnvVarRef, EnvironmentView, PodPhase, PodView, ProcessView, ProjectView, VolumeView,
};

/// A change notification. Every delta carries the global sequence number so
/// clients can resume with `Last-Event-ID`.
#[derive(Clone, Debug, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Delta {
    PodUpsert {
        seq: u64,
        pod: Arc<PodView>,
    },
    PodDelete {
        seq: u64,
        key: String,
    },
    ProjectUpsert {
        seq: u64,
        project: Arc<ProjectView>,
    },
    ProjectDelete {
        seq: u64,
        key: String,
    },
    EnvironmentUpsert {
        seq: u64,
        environment: Arc<EnvironmentView>,
    },
    EnvironmentDelete {
        seq: u64,
        key: String,
    },
    AppUpsert {
        seq: u64,
        app: Arc<AppView>,
    },
    AppDelete {
        seq: u64,
        key: String,
    },
    /// A watch was re-listed; clients must refetch the snapshot.
    Resync {
        seq: u64,
    },
}

impl Delta {
    #[must_use]
    pub fn seq(&self) -> u64 {
        match self {
            Self::PodUpsert { seq, .. }
            | Self::PodDelete { seq, .. }
            | Self::ProjectUpsert { seq, .. }
            | Self::ProjectDelete { seq, .. }
            | Self::EnvironmentUpsert { seq, .. }
            | Self::EnvironmentDelete { seq, .. }
            | Self::AppUpsert { seq, .. }
            | Self::AppDelete { seq, .. }
            | Self::Resync { seq } => *seq,
        }
    }
}

/// Point-in-time view of everything, tagged with the sequence it reflects.
#[derive(Clone, Debug, Serialize)]
pub struct Snapshot {
    pub seq: u64,
    pub pods: Vec<Arc<PodView>>,
    pub projects: Vec<Arc<ProjectView>>,
    pub environments: Vec<Arc<EnvironmentView>>,
    pub apps: Vec<Arc<AppView>>,
}

/// Bounded fan-out capacity. Slow SSE consumers observe `Lagged` and are
/// told to resync rather than growing memory.
pub const DELTA_CAPACITY: usize = 1024;

/// One bit per informer, set when its initial LIST has been swapped in.
const SYNCED_PODS: u8 = 1;
const SYNCED_PROJECTS: u8 = 1 << 1;
const SYNCED_ENVIRONMENTS: u8 = 1 << 2;
const SYNCED_APPS: u8 = 1 << 3;
const SYNCED_ALL: u8 = SYNCED_PODS | SYNCED_PROJECTS | SYNCED_ENVIRONMENTS | SYNCED_APPS;

trait Keyed {
    fn key(&self) -> &str;
}

impl Keyed for PodView {
    fn key(&self) -> &str {
        &self.key
    }
}

impl Keyed for ProjectView {
    fn key(&self) -> &str {
        &self.name
    }
}

impl Keyed for EnvironmentView {
    fn key(&self) -> &str {
        &self.name
    }
}

impl Keyed for AppView {
    fn key(&self) -> &str {
        &self.key
    }
}

/// One kind's views, keyed by [`Keyed::key`].
#[derive(Debug)]
struct Table<V>(DashMap<String, Arc<V>>);

impl<V: Keyed + PartialEq> Table<V> {
    fn new() -> Self {
        Self(DashMap::new())
    }

    /// Insert; returns the stored value only if it actually changed.
    fn upsert(&self, value: V) -> Option<Arc<V>> {
        let value = Arc::new(value);
        let changed = self.0.get(value.key()).is_none_or(|old| **old != *value);
        if !changed {
            return None;
        }
        self.0.insert(value.key().to_owned(), value.clone());
        Some(value)
    }

    fn remove(&self, key: &str) -> bool {
        self.0.remove(key).is_some()
    }

    fn replace(&self, items: Vec<V>) {
        self.0.clear();
        for v in items {
            self.0.insert(v.key().to_owned(), Arc::new(v));
        }
    }

    fn get(&self, key: &str) -> Option<Arc<V>> {
        self.0.get(key).map(|e| e.value().clone())
    }

    fn sorted(&self) -> Vec<Arc<V>> {
        let mut v: Vec<_> = self.0.iter().map(|e| e.value().clone()).collect();
        v.sort_by(|a, b| a.key().cmp(b.key()));
        v
    }
}

#[derive(Debug)]
pub struct Projections {
    pods: Table<PodView>,
    projects: Table<ProjectView>,
    environments: Table<EnvironmentView>,
    apps: Table<AppView>,
    seq: AtomicU64,
    tx: broadcast::Sender<Arc<Delta>>,
    /// `SYNCED_*` bits. Until every informer has listed once, the tables are
    /// incomplete and lookups would answer `404` for objects that exist.
    synced: watch::Sender<u8>,
}

impl Default for Projections {
    fn default() -> Self {
        Self::new()
    }
}

impl Projections {
    #[must_use]
    pub fn new() -> Self {
        let (tx, _rx) = broadcast::channel(DELTA_CAPACITY);
        Self {
            pods: Table::new(),
            projects: Table::new(),
            environments: Table::new(),
            apps: Table::new(),
            seq: AtomicU64::new(0),
            tx,
            synced: watch::Sender::new(0),
        }
    }

    fn mark_synced(&self, bit: u8) {
        self.synced.send_modify(|s| *s |= bit);
    }

    /// Every informer has completed its initial LIST.
    #[must_use]
    pub fn is_synced(&self) -> bool {
        *self.synced.borrow() == SYNCED_ALL
    }

    /// Resolve once every informer has completed its initial LIST.
    pub async fn wait_synced(&self) {
        let mut rx = self.synced.subscribe();
        // `Err` only if the sender is dropped, which `&self` rules out.
        let _ = rx.wait_for(|s| *s == SYNCED_ALL).await;
    }

    /// The informers that have not completed their initial LIST yet.
    #[must_use]
    pub fn pending_kinds(&self) -> Vec<&'static str> {
        let synced = *self.synced.borrow();
        [
            (SYNCED_PODS, "pods"),
            (SYNCED_PROJECTS, "projects"),
            (SYNCED_ENVIRONMENTS, "environments"),
            (SYNCED_APPS, "apps"),
        ]
        .into_iter()
        .filter(|(bit, _)| synced & bit == 0)
        .map(|(_, kind)| kind)
        .collect()
    }

    /// Current global sequence (also used as `ETag`).
    #[must_use]
    pub fn seq(&self) -> u64 {
        self.seq.load(Ordering::Acquire)
    }

    fn next_seq(&self) -> u64 {
        self.seq.fetch_add(1, Ordering::AcqRel) + 1
    }

    fn publish(&self, make: impl FnOnce(u64) -> Delta) {
        let seq = self.next_seq();
        // Send errors only mean "no subscribers"; that is fine.
        let _ = self.tx.send(Arc::new(make(seq)));
    }

    fn resync(&self) {
        self.publish(|seq| Delta::Resync { seq });
    }

    pub fn subscribe(&self) -> broadcast::Receiver<Arc<Delta>> {
        self.tx.subscribe()
    }

    #[must_use]
    pub fn snapshot(&self) -> Snapshot {
        Snapshot {
            seq: self.seq(),
            pods: self.pods.sorted(),
            projects: self.projects.sorted(),
            environments: self.environments.sorted(),
            apps: self.apps.sorted(),
        }
    }

    // ---- pods ----

    pub fn upsert_pod(&self, pod: PodView) {
        if let Some(pod) = self.pods.upsert(pod) {
            self.publish(|seq| Delta::PodUpsert { seq, pod });
        }
    }

    pub fn remove_pod(&self, key: &str) {
        if self.pods.remove(key) {
            self.publish(|seq| Delta::PodDelete {
                seq,
                key: key.to_owned(),
            });
        }
    }

    #[must_use]
    pub fn pod(&self, key: &str) -> Option<Arc<PodView>> {
        self.pods.get(key)
    }

    #[must_use]
    pub fn pod_count(&self) -> usize {
        self.pods.0.len()
    }

    /// Pods of one app, sorted by name.
    #[must_use]
    pub fn pods_of_app(&self, namespace: &str, app: &str) -> Vec<Arc<PodView>> {
        self.pods
            .sorted()
            .into_iter()
            .filter(|p| p.namespace == namespace && p.app.as_deref() == Some(app))
            .collect()
    }

    /// Replace the whole pod set after a re-list.
    pub fn replace_pods(&self, pods: Vec<PodView>) {
        self.pods.replace(pods);
        self.mark_synced(SYNCED_PODS);
        self.resync();
    }

    // ---- projects ----

    pub fn upsert_project(&self, project: ProjectView) {
        if let Some(project) = self.projects.upsert(project) {
            self.publish(|seq| Delta::ProjectUpsert { seq, project });
        }
    }

    pub fn remove_project(&self, name: &str) {
        if self.projects.remove(name) {
            self.publish(|seq| Delta::ProjectDelete {
                seq,
                key: name.to_owned(),
            });
        }
    }

    #[must_use]
    pub fn projects(&self) -> Vec<Arc<ProjectView>> {
        self.projects.sorted()
    }

    #[must_use]
    pub fn project(&self, name: &str) -> Option<Arc<ProjectView>> {
        self.projects.get(name)
    }

    pub fn replace_projects(&self, projects: Vec<ProjectView>) {
        self.projects.replace(projects);
        self.mark_synced(SYNCED_PROJECTS);
        self.resync();
    }

    // ---- environments ----

    pub fn upsert_environment(&self, environment: EnvironmentView) {
        if let Some(environment) = self.environments.upsert(environment) {
            self.publish(|seq| Delta::EnvironmentUpsert { seq, environment });
        }
    }

    pub fn remove_environment(&self, name: &str) {
        if self.environments.remove(name) {
            self.publish(|seq| Delta::EnvironmentDelete {
                seq,
                key: name.to_owned(),
            });
        }
    }

    #[must_use]
    pub fn environments(&self) -> Vec<Arc<EnvironmentView>> {
        self.environments.sorted()
    }

    #[must_use]
    pub fn environment(&self, name: &str) -> Option<Arc<EnvironmentView>> {
        self.environments.get(name)
    }

    pub fn replace_environments(&self, environments: Vec<EnvironmentView>) {
        self.environments.replace(environments);
        self.mark_synced(SYNCED_ENVIRONMENTS);
        self.resync();
    }

    // ---- apps ----

    pub fn upsert_app(&self, app: AppView) {
        if let Some(app) = self.apps.upsert(app) {
            self.publish(|seq| Delta::AppUpsert { seq, app });
        }
    }

    pub fn remove_app(&self, key: &str) {
        if self.apps.remove(key) {
            self.publish(|seq| Delta::AppDelete {
                seq,
                key: key.to_owned(),
            });
        }
    }

    #[must_use]
    pub fn apps(&self) -> Vec<Arc<AppView>> {
        self.apps.sorted()
    }

    #[must_use]
    pub fn app(&self, namespace: &str, name: &str) -> Option<Arc<AppView>> {
        self.apps.get(&format!("{namespace}/{name}"))
    }

    pub fn replace_apps(&self, apps: Vec<AppView>) {
        self.apps.replace(apps);
        self.mark_synced(SYNCED_APPS);
        self.resync();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pod(name: &str, ready: bool) -> PodView {
        PodView {
            key: format!("ns/{name}"),
            namespace: "ns".into(),
            name: name.into(),
            org: None,
            app: Some("api".into()),
            process: Some("web".into()),
            phase: PodPhase::Running,
            ready,
            restarts: 0,
            reason: None,
            node: None,
            started_at: None,
        }
    }

    fn env(name: &str, ready: bool) -> EnvironmentView {
        EnvironmentView {
            name: name.into(),
            uid: None,
            project: "shop".into(),
            org: Some("o".into()),
            env_type: "standard",
            namespace: format!("kb-{name}"),
            phase: None,
            ready,
            message: None,
            deleting: false,
            deletion_scheduled_at: None,
            created_at: None,
        }
    }

    #[test]
    fn upsert_publishes_delta_and_bumps_seq() {
        let p = Projections::new();
        let mut rx = p.subscribe();
        p.upsert_pod(pod("a", false));
        assert_eq!(p.seq(), 1);
        p.upsert_pod(pod("a", false)); // identical → no delta
        assert_eq!(p.seq(), 1);
        p.upsert_pod(pod("a", true));
        assert_eq!(p.seq(), 2);
        let d = rx.try_recv().expect("delta");
        assert!(matches!(&*d, Delta::PodUpsert { seq: 1, .. }));
        let d = rx.try_recv().expect("delta");
        assert!(matches!(&*d, Delta::PodUpsert { seq: 2, pod } if pod.ready));
        p.remove_pod("ns/a");
        assert_eq!(p.pod_count(), 0);
        assert!(matches!(
            &*rx.try_recv().expect("delta"),
            Delta::PodDelete { seq: 3, .. }
        ));
    }

    #[test]
    fn snapshot_is_sorted_and_tagged() {
        let p = Projections::new();
        p.upsert_pod(pod("b", true));
        p.upsert_pod(pod("a", true));
        p.upsert_environment(env("shop-prod", true));
        let s = p.snapshot();
        assert_eq!(s.seq, 3);
        assert_eq!(
            s.pods.iter().map(|x| x.name.as_str()).collect::<Vec<_>>(),
            vec!["a", "b"]
        );
        assert_eq!(s.environments.len(), 1);
    }

    #[test]
    fn resync_replaces_and_notifies() {
        let p = Projections::new();
        let mut rx = p.subscribe();
        p.upsert_pod(pod("old", true));
        p.replace_pods(vec![pod("new", true)]);
        assert!(p.pod("ns/old").is_none());
        assert!(p.pod("ns/new").is_some());
        let _ = rx.try_recv();
        assert!(matches!(&*rx.try_recv().expect("resync"), Delta::Resync { .. }));
    }

    #[tokio::test]
    async fn synced_only_after_every_informer_listed_once() {
        let p = Arc::new(Projections::new());
        let waiter = tokio::spawn({
            let p = p.clone();
            async move { p.wait_synced().await }
        });
        p.upsert_pod(pod("a", true)); // watch events do not count as a sync
        p.replace_pods(vec![]);
        p.replace_projects(vec![]);
        p.replace_environments(vec![]);
        assert!(!p.is_synced(), "apps have not listed yet");
        assert!(!waiter.is_finished());
        p.replace_apps(vec![]);
        assert!(p.is_synced());
        tokio::time::timeout(std::time::Duration::from_secs(5), waiter)
            .await
            .expect("wait_synced resolves")
            .expect("join");
        p.replace_pods(vec![]); // a later re-list keeps it synced
        assert!(p.is_synced());
    }

    #[test]
    fn names_the_informers_still_listing() {
        let p = Projections::new();
        assert_eq!(p.pending_kinds(), ["pods", "projects", "environments", "apps"]);
        p.replace_pods(vec![]);
        p.replace_environments(vec![]);
        assert_eq!(p.pending_kinds(), ["projects", "apps"]);
        p.replace_projects(vec![]);
        p.replace_apps(vec![]);
        assert!(p.pending_kinds().is_empty());
    }

    #[test]
    fn environments_and_pods_of_app() {
        let p = Projections::new();
        p.upsert_environment(env("shop-dev", false));
        p.upsert_environment(env("shop-dev", true));
        assert!(p.environment("shop-dev").is_some_and(|e| e.ready));
        p.remove_environment("shop-dev");
        assert!(p.environments().is_empty());

        p.upsert_pod(pod("api-1", true));
        let mut other = pod("worker-1", true);
        other.app = Some("worker".into());
        p.upsert_pod(other);
        assert_eq!(p.pods_of_app("ns", "api").len(), 1);
    }
}

//! One SSE stream per browser tab (ADR-014): a `snapshot` event followed by
//! `delta` events. Every event carries the global sequence as its `id`, so
//! `EventSource` reconnects with `Last-Event-ID`; if the client is too far
//! behind (broadcast lag) it receives `resync` and refetches.
//!
//! Invariant I-1: the stream is filtered to the caller's orgs. Deletes are
//! only forwarded for objects the connection has seen, so not even names of
//! other tenants' objects leak.

use std::{collections::HashSet, convert::Infallible, time::Duration};

use axum::{
    extract::State,
    http::HeaderMap,
    response::sse::{Event, KeepAlive, Sse},
};
use futures::Stream;
use kuben_platform::projection::{Delta, Snapshot};
use tokio::sync::broadcast::error::RecvError;

use crate::{authz::Authz, error::ApiResult, state::ApiState};

/// Per-connection tenant filter.
#[derive(Debug)]
pub struct Visibility {
    orgs: HashSet<String>,
    seen: HashSet<String>,
}

impl Visibility {
    #[must_use]
    pub fn new(orgs: impl IntoIterator<Item = String>) -> Self {
        Self {
            orgs: orgs.into_iter().collect(),
            seen: HashSet::new(),
        }
    }

    fn allowed(&self, org: Option<&str>) -> bool {
        org.is_some_and(|o| self.orgs.contains(o))
    }

    fn track(&mut self, key: String, org: Option<&str>) -> bool {
        if self.allowed(org) {
            self.seen.insert(key);
            true
        } else {
            self.seen.remove(&key);
            false
        }
    }

    /// Filter a snapshot to the caller's orgs and remember what was sent.
    pub fn snapshot(&mut self, mut s: Snapshot) -> Snapshot {
        s.pods
            .retain(|p| self.orgs.contains(p.org.as_deref().unwrap_or_default()));
        s.projects
            .retain(|p| self.orgs.contains(p.org.as_deref().unwrap_or_default()));
        s.environments
            .retain(|e| self.orgs.contains(e.org.as_deref().unwrap_or_default()));
        s.apps
            .retain(|a| self.orgs.contains(a.org.as_deref().unwrap_or_default()));
        self.seen.extend(s.pods.iter().map(|p| format!("pod:{}", p.key)));
        self.seen
            .extend(s.projects.iter().map(|p| format!("project:{}", p.name)));
        self.seen
            .extend(s.environments.iter().map(|e| format!("environment:{}", e.name)));
        self.seen.extend(s.apps.iter().map(|a| format!("app:{}", a.key)));
        s
    }

    /// Whether this delta may be sent on the connection.
    pub fn admit(&mut self, delta: &Delta) -> bool {
        match delta {
            Delta::PodUpsert { pod, .. } => self.track(format!("pod:{}", pod.key), pod.org.as_deref()),
            Delta::PodDelete { key, .. } => self.seen.remove(&format!("pod:{key}")),
            Delta::ProjectUpsert { project, .. } => {
                self.track(format!("project:{}", project.name), project.org.as_deref())
            }
            Delta::ProjectDelete { key, .. } => self.seen.remove(&format!("project:{key}")),
            Delta::EnvironmentUpsert { environment, .. } => self.track(
                format!("environment:{}", environment.name),
                environment.org.as_deref(),
            ),
            Delta::EnvironmentDelete { key, .. } => self.seen.remove(&format!("environment:{key}")),
            Delta::AppUpsert { app, .. } => self.track(format!("app:{}", app.key), app.org.as_deref()),
            Delta::AppDelete { key, .. } => self.seen.remove(&format!("app:{key}")),
            Delta::Resync { .. } => true,
        }
    }
}

/// Cluster event stream for the authenticated user, filtered to their orgs.
pub async fn handler(
    State(state): State<ApiState>,
    authz: Authz,
    headers: HeaderMap,
) -> ApiResult<Sse<impl Stream<Item = Result<Event, Infallible>>>> {
    let last_seen: Option<u64> = headers
        .get("last-event-id")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse().ok());
    let mut visibility = Visibility::new(authz.org_ids().iter().map(ToString::to_string));
    let mut rx = state.projections.subscribe();
    let snapshot = visibility.snapshot(state.projections.snapshot());

    let stream = async_stream::stream! {
        // A reconnecting client that is still current only needs deltas.
        if last_seen != Some(snapshot.seq) {
            yield Ok(Event::default().event("snapshot").id(snapshot.seq.to_string())
                .json_data(&snapshot).unwrap_or_else(|_| Event::default().event("error").data("serialize")));
        }
        loop {
            match rx.recv().await {
                Ok(delta) => {
                    if !visibility.admit(&delta) {
                        continue;
                    }
                    let name = if matches!(&*delta, Delta::Resync { .. }) { "resync" } else { "delta" };
                    let ev = Event::default().event(name).id(delta.seq().to_string());
                    yield Ok(ev.json_data(&*delta).unwrap_or_else(|_| Event::default().event("error").data("serialize")));
                }
                Err(RecvError::Lagged(n)) => {
                    metrics::counter!("kuben_sse_lagged_total").increment(n);
                    yield Ok(Event::default().event("resync").data(n.to_string()));
                }
                Err(RecvError::Closed) => break,
            }
        }
    };

    Ok(Sse::new(stream).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)).text("ping")))
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use kuben_platform::projection::{PodPhase, PodView, ProjectView, Projections};

    use super::*;

    fn project(name: &str, org: &str) -> ProjectView {
        ProjectView {
            name: name.into(),
            uid: None,
            display_name: name.into(),
            description: None,
            org: Some(org.into()),
            environments: 0,
            ready: true,
            deleting: false,
            created_at: None,
        }
    }

    fn pod(name: &str, org: &str) -> PodView {
        PodView {
            key: format!("ns/{name}"),
            namespace: "ns".into(),
            name: name.into(),
            org: Some(org.into()),
            app: None,
            process: None,
            phase: PodPhase::Running,
            ready: true,
            restarts: 0,
            reason: None,
            node: None,
            started_at: None,
        }
    }

    #[test]
    fn snapshot_and_deltas_are_tenant_filtered() {
        let p = Projections::new();
        p.upsert_project(project("mine", "a"));
        p.upsert_project(project("theirs", "b"));
        let mut v = Visibility::new(["a".to_owned()]);
        let snap = v.snapshot(p.snapshot());
        assert_eq!(
            snap.projects.iter().map(|x| x.name.as_str()).collect::<Vec<_>>(),
            vec!["mine"]
        );

        let other = Delta::PodUpsert {
            seq: 9,
            pod: Arc::new(pod("x", "b")),
        };

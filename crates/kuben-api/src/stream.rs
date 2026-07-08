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

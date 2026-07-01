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

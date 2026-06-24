//! Subsystem health registry backing `/livez`, `/readyz` and
//! `/healthz/details`.

use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicI64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

use dashmap::DashMap;
use serde::Serialize;

#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum State {
    Ok,
    Degraded,
    Starting,
    /// Healthy but deliberately idle, e.g. waiting for the controller lease.
    Standby,
}

#[derive(Clone, Debug, Serialize)]
pub struct Subsystem {
    pub state: State,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
    pub updated_at_ms: i64,
}

#[derive(Clone, Debug, Default)]
pub struct Health {
    inner: Arc<Inner>,
}

#[derive(Debug)]
struct Inner {
    ready: AtomicBool,
    /// Last watchdog heartbeat (unix ms). `/livez` fails if this goes stale.
    heartbeat: AtomicI64,
    subsystems: DashMap<&'static str, Subsystem>,
}

impl Default for Inner {
    fn default() -> Self {
        Self {
            ready: AtomicBool::new(false),
            heartbeat: AtomicI64::new(now_ms()),
            subsystems: DashMap::new(),
        }
    }
}

fn now_ms() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_millis()),
    )
    .unwrap_or(i64::MAX)
}

impl Health {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    pub fn set_ready(&self, ready: bool) {
        self.inner.ready.store(ready, Ordering::SeqCst);
    }

    #[must_use]
    pub fn is_ready(&self) -> bool {
        self.inner.ready.load(Ordering::SeqCst)
    }

    /// Called by the watchdog task every second.
    pub fn heartbeat(&self) {
        self.inner.heartbeat.store(now_ms(), Ordering::Relaxed);
    }

    /// Liveness: the runtime has heartbeated within `max_age_ms`.
    #[must_use]
    pub fn is_live(&self, max_age_ms: i64) -> bool {
        now_ms() - self.inner.heartbeat.load(Ordering::Relaxed) < max_age_ms
    }

    pub fn starting(&self, name: &'static str) {
        self.set(name, State::Starting, None);
    }

    pub fn ok(&self, name: &'static str) {
        self.set(name, State::Ok, None);
    }

    pub fn standby(&self, name: &'static str) {
        self.set(name, State::Standby, None);
    }

    pub fn degraded(&self, name: &'static str, error: &str) {
        self.set(name, State::Degraded, Some(error.to_owned()));
    }

    fn set(&self, name: &'static str, state: State, last_error: Option<String>) {
        self.inner.subsystems.insert(
            name,
            Subsystem {
                state,
                last_error,
                updated_at_ms: now_ms(),
            },
        );
    }

    /// Snapshot of every subsystem, sorted by name.
    #[must_use]
    pub fn details(&self) -> Vec<(&'static str, Subsystem)> {
        let mut v: Vec<_> = self
            .inner
            .subsystems
            .iter()
            .map(|e| (*e.key(), e.value().clone()))
            .collect();
        v.sort_by_key(|(k, _)| *k);
        v
    }

    #[must_use]
    pub fn any_degraded(&self) -> bool {
        self.inner
            .subsystems
            .iter()
            .any(|e| e.value().state == State::Degraded)
    }
}

#[cfg(test)]
mod tests {
    use super::*;


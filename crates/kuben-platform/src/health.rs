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

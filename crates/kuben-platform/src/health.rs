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

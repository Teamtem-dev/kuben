//! Scale-to-zero activator (Master Blueprint §3.3). Phase 2 implements the
//! hyper proxy; phase 0 only reserves the role so `--roles=activator` is a
//! valid, health-reporting no-op.

use std::sync::Arc;


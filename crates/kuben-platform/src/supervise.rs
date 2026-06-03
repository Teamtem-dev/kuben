//! Supervisor: restarts a subsystem with jittered exponential backoff when it
//! fails or panics. Requires `panic = "unwind"` (set in the workspace
//! release profile) so a panic is caught by the `JoinHandle` instead of
//! aborting the whole binary.

use std::{future::Future, time::Duration};

use backon::{BackoffBuilder, ExponentialBuilder};
use tokio_util::sync::CancellationToken;


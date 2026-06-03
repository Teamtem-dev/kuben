//! Supervisor: restarts a subsystem with jittered exponential backoff when it
//! fails or panics. Requires `panic = "unwind"` (set in the workspace
//! release profile) so a panic is caught by the `JoinHandle` instead of
//! aborting the whole binary.

use std::{future::Future, time::Duration};

use backon::{BackoffBuilder, ExponentialBuilder};
use tokio_util::sync::CancellationToken;

use crate::health::Health;

/// Run `make` in a loop until `token` is cancelled or the task completes
/// successfully.
pub async fn supervise<F, Fut>(name: &'static str, token: CancellationToken, health: Health, mut make: F)
where
    F: FnMut(CancellationToken) -> Fut,
    Fut: Future<Output = anyhow::Result<()>> + Send + 'static,
{
    let mut backoff = ExponentialBuilder::default()
        .with_min_delay(Duration::from_millis(500))
        .with_max_delay(Duration::from_mins(1))
        .with_jitter()
        .without_max_times()
        .build();

    health.starting(name);
    loop {
        let handle = tokio::spawn(make(token.child_token()));
        let outcome = handle.await;
        if token.is_cancelled() {

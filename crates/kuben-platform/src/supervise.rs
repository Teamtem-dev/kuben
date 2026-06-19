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
            return;
        }
        match outcome {
            Ok(Ok(())) => {
                health.ok(name);
                return;
            }
            Ok(Err(e)) => {
                health.degraded(name, &e.to_string());
                metrics::counter!("kuben_subsystem_failures_total", "subsystem" => name).increment(1);
                tracing::error!(subsystem = name, error = %e, "subsystem failed; restarting");
            }
            Err(join) if join.is_panic() => {
                health.degraded(name, "panic");
                metrics::counter!("kuben_subsystem_panics_total", "subsystem" => name).increment(1);
                tracing::error!(subsystem = name, "subsystem panicked; restarting");
            }
            Err(_) => return, // cancelled
        }
        let delay = backoff.next().unwrap_or(Duration::from_mins(1));
        tokio::select! {
            () = token.cancelled() => return,
            () = tokio::time::sleep(delay) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicU32, Ordering},
    };

    use super::*;

    #[tokio::test]
    async fn restarts_after_failure_then_succeeds() {
        let attempts = Arc::new(AtomicU32::new(0));
        let health = Health::new();
        let token = CancellationToken::new();
        let a = attempts.clone();
        tokio::time::timeout(
            Duration::from_secs(10),
            supervise("test", token, health.clone(), move |_t| {
                let a = a.clone();
                async move {
                    if a.fetch_add(1, Ordering::SeqCst) < 2 {
                        anyhow::bail!("transient")
                    }
                    Ok(())
                }
            }),

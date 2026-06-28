//! Scale-to-zero activator (Master Blueprint §3.3). Phase 2 implements the
//! hyper proxy; phase 0 only reserves the role so `--roles=activator` is a
//! valid, health-reporting no-op.

use std::sync::Arc;

use tokio_util::sync::CancellationToken;

use crate::{health::Health, projection::Projections, registry::ClusterRegistry};

pub async fn run(
    _registry: ClusterRegistry,
    _projections: Arc<Projections>,
    health: Health,
    token: CancellationToken,
    bind: String,
) -> anyhow::Result<()> {
    tracing::info!(%bind, "activator role enabled (proxy lands in phase 2)");
    health.ok("activator");
    token.cancelled().await;
    Ok(())
}

//! Structured logging + Prometheus metrics exporter.

use kuben_core::config::Config;
use tracing_subscriber::{EnvFilter, fmt, prelude::*};

pub fn init(cfg: &Config) -> anyhow::Result<()> {
    let filter =
        EnvFilter::try_from_default_env().or_else(|_| EnvFilter::try_new(&cfg.telemetry.log_level))?;
    let registry = tracing_subscriber::registry().with(filter);
    if cfg.telemetry.log_format == "pretty" {
        registry
            .with(fmt::layer().with_target(true).compact())
            .try_init()?;
    } else {
        registry
            .with(fmt::layer().json().flatten_event(true).with_current_span(true))
            .try_init()?;
    }
    Ok(())
}

/// Install the Prometheus exporter on `metrics_bind`. Must be called inside a
/// Tokio runtime. Failing to bind is logged, not fatal.
pub fn install_metrics(cfg: &Config) {
    let Ok(addr) = cfg.server.metrics_bind.parse::<std::net::SocketAddr>() else {
        tracing::warn!(bind = %cfg.server.metrics_bind, "invalid metrics bind address; metrics disabled");
        return;
    };
    match metrics_exporter_prometheus::PrometheusBuilder::new()
        .with_http_listener(addr)

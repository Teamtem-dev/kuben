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

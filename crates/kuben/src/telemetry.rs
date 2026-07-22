//! Structured logging + Prometheus metrics exporter.

use kuben_core::config::Config;
use tracing_subscriber::{EnvFilter, fmt, prelude::*};

pub fn init(cfg: &Config) -> anyhow::Result<()> {

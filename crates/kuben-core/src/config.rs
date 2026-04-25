//! Configuration. Precedence (lowest → highest):
//! built-in defaults → `/etc/kuben/config.toml` → `./kuben.toml` → `KUBEN_*`
//! environment variables (nested keys separated by `__`, e.g.
//! `KUBEN_SERVER__BIND=0.0.0.0:9000`).

use figment::{
    Figment,
    providers::{Env, Format, Serialized, Toml},
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Role {
    /// Everything in one process (default for self-hosting).
    All,
    Api,
    Controller,
    Activator,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Config {
    pub server: ServerCfg,
    pub database: DatabaseCfg,
    pub runtime: RuntimeCfg,
    pub kube: KubeCfg,
    pub security: SecurityCfg,
    pub telemetry: TelemetryCfg,
    pub bootstrap: BootstrapCfg,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct ServerCfg {
    pub bind: String,
    pub metrics_bind: String,
    pub activator_bind: String,
    pub public_url: Option<String>,
    pub roles: Vec<Role>,
    /// Request timeout for non-streaming endpoints, seconds.
    pub request_timeout_secs: u64,
    /// Maximum request body, bytes.
    pub max_body_bytes: usize,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct DatabaseCfg {
    /// `sqlite:///data/kuben.db` or `postgres://user:pass@host/db`.
    pub url: String,
    pub max_connections: u32,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct RuntimeCfg {
    pub worker_threads: Option<usize>,
    pub max_blocking_threads: usize,
    /// ADR-013: run controllers on a second runtime (off in phase 0).
    pub bulkhead: bool,

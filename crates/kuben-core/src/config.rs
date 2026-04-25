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

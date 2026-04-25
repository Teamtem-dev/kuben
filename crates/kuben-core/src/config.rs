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
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct KubeCfg {
    pub kubeconfig: Option<String>,
    pub context: Option<String>,
    pub watch_namespace: Option<String>,
    /// If true, refuse to start when no cluster is reachable.
    pub required: bool,
    /// Namespace Kuben itself runs in (controller lease, initial admin
    /// Secret). Defaults to the pod's service-account namespace.
    pub namespace: Option<String>,
    /// Run the controllers only on the replica holding the `kuben-controller`
    /// Lease (ADR-023). Required whenever more than one replica has the
    /// controller role; the Helm chart always enables it.
    pub leader_election: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct SecurityCfg {
    pub session_ttl_hours: u64,
    pub session_cache_ttl_secs: u64,
    pub argon2_m_kib: u32,
    pub argon2_t: u32,
    pub argon2_p: u32,
    pub login_concurrency: usize,
    /// Set `Secure` on the session cookie (disable only for plain-http dev).
    pub cookie_secure: bool,
    /// Failed logins allowed per (email, client IP) within `login_window_secs`.
    pub login_max_failures: u32,
    /// Failed logins allowed per client IP, across all accounts.
    pub login_max_failures_per_ip: u32,
    /// Failed logins allowed per account, across all IPs (high on purpose, so
    /// victims cannot be locked out cheaply).
    pub login_max_failures_per_account: u32,
    pub login_window_secs: u64,
    /// Take the client IP from the **last** `X-Forwarded-For` hop. Enable only
    /// behind a proxy that appends it (the Helm chart does); otherwise the
    /// TCP peer address is used.
    pub trust_forwarded_for: bool,
    pub password_min_length: usize,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct TelemetryCfg {
    /// `json` or `pretty`.
    pub log_format: String,
    pub log_level: String,
    pub otlp_endpoint: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct BootstrapCfg {
    pub org_slug: String,
    pub org_name: String,
    pub admin_email: String,
    /// Initial admin password. If unset, a random one is generated: in a pod
    /// it is stored in the `kuben-initial-admin` Secret (never logged),
    /// elsewhere it is printed once.

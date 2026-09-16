//! Configuration. Precedence (lowest → highest):
//! built-in defaults → `/etc/kuben/config.toml` → `./kuben.toml` → `KUBEN_*`
//! environment variables (nested keys separated by `__`, e.g.
//! `KUBEN_SERVER__BIND=0.0.0.0:9000`).

use std::{
    ffi::OsString,
    net::SocketAddr,
    path::{Path, PathBuf},
};

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
    pub agent: AgentCfg,
    pub git: GitCfg,
    pub build: BuildCfg,
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
    /// Directory for files that belong to this installation: the setup token
    /// and a generated initial admin password. Default: see
    /// [`Config::state_dir`].
    pub state_dir: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct DatabaseCfg {
    /// `postgres://user:pass@host/db`. Required, with no default: Kuben keeps
    /// its data in PostgreSQL (ADR-025).
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
    /// `Secure` on the session cookie: `true`, `false`, or `"auto"` (the
    /// default), which follows `server.public_url`. See [`CookieSecure`].
    pub cookie_secure: CookieSecure,
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
    /// With `trust_forwarded_for`: the proxies (CIDRs, e.g. the pod network
    /// `10.42.0.0/16`) whose `X-Forwarded-*` headers are believed. Empty:
    /// every peer's, for a proxy that is the only way in (the Helm chart).
    pub trusted_proxies: Vec<String>,
    /// Let the first admin be created over plain HTTP from another machine.
    /// Off (the default): only over HTTPS (a trusted proxy's
    /// `X-Forwarded-Proto`), from this machine (an SSH tunnel), or when the
    /// console listens on loopback only (ADR-031).
    pub insecure_setup: bool,
    pub password_min_length: usize,
}

impl SecurityCfg {
    /// Whether the `X-Forwarded-*` headers of a request from `peer` are
    /// believed.
    #[must_use]
    pub fn trusts_forwarded(&self, peer: Option<std::net::IpAddr>) -> bool {
        self.trust_forwarded_for
            && (self.trusted_proxies.is_empty()
                || peer.is_some_and(|ip| self.trusted_proxies.iter().any(|cidr| cidr_contains(cidr, ip))))
    }
}

/// Whether `ip` lies in `cidr` (`10.42.0.0/16`, `fd00::/8`, or one address).
#[must_use]
pub fn cidr_contains(cidr: &str, ip: std::net::IpAddr) -> bool {
    use std::net::IpAddr;
    let (network, bits) = match cidr.split_once('/') {
        Some((network, bits)) => (network, bits.parse::<u32>().ok()),
        None => (cidr, None),
    };
    let Ok(network) = network.trim().parse::<IpAddr>() else {
        return false;
    };
    match (network, ip) {
        (IpAddr::V4(net), IpAddr::V4(ip)) => {
            let bits = bits.unwrap_or(32).min(32);
            let mask = u32::MAX.checked_shl(32 - bits).unwrap_or(0);
            u32::from(net) & mask == u32::from(ip) & mask
        }
        (IpAddr::V6(net), IpAddr::V6(ip)) => {
            let bits = bits.unwrap_or(128).min(128);
            let mask = u128::MAX.checked_shl(128 - bits).unwrap_or(0);
            u128::from(net) & mask == u128::from(ip) & mask
        }
        _ => false,
    }
}

/// Whether the session cookie carries `Secure` (and the `__Host-` prefix).
///
/// Browsers drop a `Secure` cookie over plain http, except on `localhost`, so
/// a console reached at `http://<server-ip>:3000` could never sign in. `Auto`
/// sets the flag exactly when the console is published over https
/// (`server.public_url` starts with `https://`), so a fresh install works over
/// http and becomes `Secure` the moment a TLS address is configured. Set
/// `true` behind a TLS proxy that does not set `public_url`, and `false` only
/// for development.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum CookieSecure {
    /// `true` or `false`, as configured.
    Fixed(bool),
    /// `"auto"`: `Secure` when `public_url` is https.
    Auto(Auto),
}

/// The word `auto`, so that [`CookieSecure`] parses `"auto"` from TOML and
/// `KUBEN_SECURITY__COOKIE_SECURE=auto`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Auto {
    Auto,
}

impl CookieSecure {
    pub const AUTO: Self = Self::Auto(Auto::Auto);
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
    /// it is stored in the `kuben-initial-admin` Secret; elsewhere it is
    /// printed once to the terminal or, without one, written to an
    /// owner-only file in [`Config::state_dir`]. It is never logged.
    pub admin_password: Option<String>,
}

impl Default for ServerCfg {
    fn default() -> Self {
        Self {
            bind: "0.0.0.0:8080".into(),
            metrics_bind: "0.0.0.0:9090".into(),
            activator_bind: "0.0.0.0:8081".into(),
            public_url: None,
            roles: vec![Role::All],
            request_timeout_secs: 30,
            max_body_bytes: 1 << 20,
            state_dir: None,
        }
    }
}

impl Default for DatabaseCfg {
    fn default() -> Self {
        Self {
            url: String::new(),
            max_connections: 4,
        }
    }
}

/// The volume of the container image and the Helm chart, and where binaries
/// before 1.0.3 kept their data.
const LEGACY_DATA_DIR: &str = "/data";

fn default_data_dir(legacy_exists: bool, env: impl Fn(&str) -> Option<OsString>) -> PathBuf {
    if legacy_exists {
        return PathBuf::from(LEGACY_DATA_DIR);
    }
    let var = |key: &str| env(key).filter(|v| !v.is_empty());
    // systemd sets `$STATE_DIRECTORY` for `StateDirectory=`: absolute paths,
    // colon-separated when the unit lists several.
    if let Some(dirs) = var("STATE_DIRECTORY")
        && let Some(first) = dirs.to_string_lossy().split(':').find(|d| !d.is_empty())
    {
        return PathBuf::from(first);
    }
    if let Some(state) = var("XDG_STATE_HOME") {
        return PathBuf::from(state).join("kuben");
    }
    if let Some(home) = var("HOME") {
        return PathBuf::from(home).join(".local").join("state").join("kuben");
    }
    if let Some(local) = var("LOCALAPPDATA") {
        return PathBuf::from(local).join("kuben");
    }
    PathBuf::from(".")
}

/// Whether this process runs in a Kubernetes pod.
#[must_use]
pub fn in_cluster() -> bool {
    std::env::var_os("KUBERNETES_SERVICE_HOST").is_some()
}

impl Default for RuntimeCfg {
    fn default() -> Self {
        Self {
            worker_threads: None,
            max_blocking_threads: 16,
            bulkhead: false,
        }
    }
}

impl Default for SecurityCfg {
    fn default() -> Self {
        Self {
            session_ttl_hours: 12,
            session_cache_ttl_secs: 5,
            argon2_m_kib: 19 * 1024,
            argon2_t: 2,
            argon2_p: 1,
            login_concurrency: 2,
            cookie_secure: CookieSecure::AUTO,
            login_max_failures: 5,
            login_max_failures_per_ip: 30,
            login_max_failures_per_account: 100,
            login_window_secs: 900,
            trust_forwarded_for: false,
            trusted_proxies: Vec::new(),
            insecure_setup: false,
            password_min_length: 12,
        }
    }
}

impl Default for TelemetryCfg {
    fn default() -> Self {
        Self {
            log_format: "json".into(),
            log_level: "info".into(),
            otlp_endpoint: None,
        }
    }
}

impl Default for BootstrapCfg {
    fn default() -> Self {
        Self {
            org_slug: "default".into(),
            org_name: "Default".into(),
            admin_email: "admin@kuben.local".into(),
            admin_password: None,
        }
    }
}

/// AgentLink, the hub's endpoint for cluster agents (ADR-027).
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct AgentCfg {
    /// Where the hub listens for agents (`host:port`); unset, it does not.
    pub bind: Option<String>,
    /// The name the hub's certificate carries; agents check it.
    pub hub_name: String,
    /// Lifetime of the client certificates the hub issues, hours.
    pub certificate_hours: u64,
    /// How often agents send a heartbeat, seconds.
    pub heartbeat_secs: u64,
    /// Enroll an agent inside Kuben's own cluster (M2.8): the hub publishes
    /// its address, CA and a bootstrap token in a Secret the agent mounts.
    pub local: bool,
    /// The address agents in the cluster dial (`host:port`), e.g. the
    /// Service of Kuben or the node's address.
    pub advertise: Option<String>,
    /// Namespace of the local agent and its Secrets; default: Kuben's own,
    /// else `kuben-system`.
    pub namespace: Option<String>,
}

impl Default for AgentCfg {
    fn default() -> Self {
        Self {
            bind: None,
            hub_name: "hub.kuben.internal".into(),
            certificate_hours: 24,
            heartbeat_secs: 10,
            local: false,
            advertise: None,
            namespace: None,
        }
    }
}

/// Git providers (M3): the GitHub App Kuben acts as.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct GitCfg {
    /// The GitHub App's numeric id; unset, Git sources are off.
    pub github_app_id: Option<u64>,
    /// PEM file of the App's private key (PKCS#1 or PKCS#8 RSA).
    pub github_private_key_file: Option<String>,
    /// The secret GitHub signs webhook deliveries with.
    pub github_webhook_secret: Option<String>,
    /// The REST API root, for GitHub Enterprise Server.
    pub github_api_url: String,
    /// Where build pods clone from.
    pub github_clone_url: String,
}

impl Default for GitCfg {
    fn default() -> Self {
        Self {
            github_app_id: None,
            github_private_key_file: None,
            github_webhook_secret: None,
            github_api_url: "https://api.github.com".into(),
            github_clone_url: "https://github.com".into(),
        }
    }
}

impl GitCfg {
    /// Whether the GitHub App is configured completely.
    #[must_use]
    pub fn github_enabled(&self) -> bool {
        self.github_app_id.is_some()
            && self
                .github_private_key_file
                .as_deref()
                .is_some_and(|f| !f.is_empty())
            && self
                .github_webhook_secret
                .as_deref()
                .is_some_and(|s| !s.is_empty())
    }
}

/// Isolated builds (ADR-028): one rootless BuildKit Job per attempt.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct BuildCfg {
    /// Run the build worker.
    pub enabled: bool,
    /// Namespace of build Jobs; default: Kuben's own, else `kuben-builds`.
    pub namespace: Option<String>,
    /// Rootless BuildKit image. The defaults are pinned by tag; production
    /// pins every build image by digest (`image@sha256:…`), and the build
    /// worker warns at start about any image that is not.
    pub buildkit_image: String,
    /// Image with `git` that fetches the source.
    pub fetch_image: String,
    /// Railpack's BuildKit frontend image; with `railpack_image`, enables
    /// Railpack builds. Unset, only Dockerfile builds run.
    pub railpack_frontend: Option<String>,
    /// Image with `sh` and the `railpack` CLI that writes the build plan;
    /// unset, only Dockerfile builds run.
    pub railpack_image: Option<String>,
    pub cpu_request: String,
    pub cpu_limit: String,
    /// Memory request and limit are equal (ADR-028).
    pub memory: String,
    pub ephemeral_storage: String,
    /// Hard deadline of one attempt.
    pub deadline_secs: u64,
    /// Builds at once, over all organizations; 0 disables building.
    pub max_concurrent: u32,
    pub max_concurrent_per_org: u32,
    /// A `kubernetes.io/dockerconfigjson` Secret in the build namespace with
    /// push access to the image repositories; unset for an open registry.
    pub push_secret: Option<String>,
    /// Push over plain HTTP (an in-cluster registry without TLS).
    pub insecure_registry: bool,
    /// Registry credentials the verifier uses (`user:password`), if any.
    pub registry_auth_file: Option<String>,
    /// Node selector `key=value` of the build pool; required when set.
    pub node_pool: Option<String>,
}

impl BuildCfg {
    /// The build images that are not pinned by digest.
    #[must_use]
    pub fn unpinned_images(&self) -> Vec<&str> {
        [
            Some(self.buildkit_image.as_str()),
            Some(self.fetch_image.as_str()),
            self.railpack_image.as_deref(),
            self.railpack_frontend.as_deref(),
        ]
        .into_iter()
        .flatten()
        .filter(|image| !image.is_empty() && !image.contains("@sha256:"))
        .collect()
    }
}

impl Default for BuildCfg {
    fn default() -> Self {
        Self {
            enabled: false,
            namespace: None,
            buildkit_image: "moby/buildkit:v0.33.0-rootless".into(),
            fetch_image: "alpine/git:2.49.1".into(),
            railpack_frontend: None,
            railpack_image: None,
            cpu_request: "500m".into(),
            cpu_limit: "2".into(),
            memory: "2Gi".into(),
            ephemeral_storage: "10Gi".into(),
            deadline_secs: 1800,
            max_concurrent: 2,
            max_concurrent_per_org: 1,
            push_secret: None,
            insecure_registry: false,
            registry_auth_file: None,
            node_pool: None,
        }
    }
}

impl Config {
    /// Load configuration using the documented precedence.
    #[allow(clippy::result_large_err)] // figment::Error is large by design; load runs once at startup
    pub fn load() -> figment::Result<Self> {
        Self::figment().extract()
    }

    /// The raw figment, exposed so tests and the CLI can layer overrides.
    #[must_use]
    pub fn figment() -> Figment {
        Figment::from(Serialized::defaults(Self::default()))
            .merge(Toml::file("/etc/kuben/config.toml"))
            .merge(Toml::file("kuben.toml"))
            .merge(Env::prefixed("KUBEN_").split("__"))
    }

    /// Whether the given role is enabled (`All` enables every role).
    #[must_use]
    pub fn has_role(&self, role: Role) -> bool {
        self.server.roles.iter().any(|r| *r == Role::All || *r == role)
    }

    /// Why signing in would fail, if it would: browsers drop a `Secure`
    /// cookie over plain http (except on `localhost`), so a console reached at
    /// `http://<server-ip>:8080` loops back to the login page without an
    /// error. `None` in a pod (reached through a port-forward on localhost or
    /// a TLS Gateway), with an https public URL, or when bound to loopback.
    #[must_use]
    pub fn insecure_cookie_warning(&self, in_cluster: bool) -> Option<String> {
        if !self.cookie_secure() || in_cluster || self.public_url_is_https() {
            return None;
        }
        if self.bind_is_loopback() {
            return None;
        }
        let port = self.bind_port();
        Some(format!(
            "the session cookie is Secure and browsers drop it over plain http, so signing in at \
             http://<this server>:{port} loops back to the login page. Serve the console over \
             HTTPS (and set KUBEN_SERVER__PUBLIC_URL), open it through an SSH tunnel to \
             localhost, or set KUBEN_SECURITY__COOKIE_SECURE=auto"
        ))
    }

    /// Whether the session cookie gets `Secure`: `security.cookie_secure`,
    /// with `auto` resolved against `server.public_url`.
    #[must_use]
    pub fn cookie_secure(&self) -> bool {
        match self.security.cookie_secure {
            CookieSecure::Fixed(secure) => secure,
            CookieSecure::Auto(_) => self.public_url_is_https(),
        }
    }

    #[must_use]
    pub fn public_url_is_https(&self) -> bool {
        self.server
            .public_url
            .as_deref()
            .is_some_and(|url| url.starts_with("https://"))
    }

    /// Whether the first admin is created from the console (`/setup`)
    /// instead of a configured or generated password: outside a cluster, when
    /// `bootstrap.admin_password` is not set. A pod keeps the Secret flow.
    #[must_use]
    pub fn setup_wizard(&self) -> bool {
        !in_cluster() && self.bootstrap.admin_password.as_deref().is_none_or(str::is_empty)
    }

    /// The console's address for links: `server.public_url`, else plain http
    /// on `host` and the bound port.
    #[must_use]
    pub fn console_url_with_host(&self, host: &str) -> String {
        if let Some(url) = self.server.public_url.as_deref().filter(|u| !u.is_empty()) {
            return url.trim_end_matches('/').to_owned();
        }
        format!("http://{host}:{}", self.bind_port())
    }

    /// The port of `server.bind`.
    #[must_use]
    pub fn bind_port(&self) -> u16 {
        self.server
            .bind
            .rsplit(':')
            .next()
            .and_then(|port| port.parse().ok())
            .unwrap_or(8080)
    }

    /// Whether the API listens on loopback only.
    #[must_use]
    pub fn bind_is_loopback(&self) -> bool {
        let bind = &self.server.bind;
        bind.parse::<SocketAddr>()
            .map_or_else(|_| bind.starts_with("localhost:"), |addr| addr.ip().is_loopback())
    }

    /// Where files that belong to this installation go (the setup token, a
    /// generated initial admin password): `server.state_dir` when set. Else an
    /// existing `/data` (the container volume, and where binaries before 1.0.3
    /// kept their data), systemd's `StateDirectory=`, the user's state
    /// directory (`~/.local/state/kuben`), or the working directory, so
    /// `kuben serve` and `kuben setup-token` work without root.
    #[must_use]
    pub fn state_dir(&self) -> PathBuf {
        match self.server.state_dir.as_deref().filter(|dir| !dir.is_empty()) {
            Some(dir) => PathBuf::from(dir),
            None => default_data_dir(Path::new(LEGACY_DATA_DIR).is_dir(), |key| std::env::var_os(key)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn forwarded_headers_are_believed_only_from_trusted_proxies() {
        let ip = |s: &str| s.parse::<std::net::IpAddr>().expect("ip");
        assert!(cidr_contains("10.42.0.0/16", ip("10.42.3.7")));
        assert!(!cidr_contains("10.42.0.0/16", ip("10.43.0.1")));
        assert!(cidr_contains("203.0.113.7", ip("203.0.113.7")));
        assert!(cidr_contains("0.0.0.0/0", ip("198.51.100.1")));
        assert!(cidr_contains("fd00::/8", ip("fd12::1")));
        assert!(!cidr_contains("fd00::/8", ip("10.42.0.1")), "families never mix");
        assert!(!cidr_contains("not a network", ip("10.42.0.1")));

        let mut sec = SecurityCfg::default();
        assert!(!sec.trusts_forwarded(Some(ip("10.42.0.5"))), "off by default");
        sec.trust_forwarded_for = true;
        assert!(
            sec.trusts_forwarded(None),
            "no list: every peer (the chart's proxy)"
        );
        sec.trusted_proxies = vec!["10.42.0.0/16".into()];
        assert!(sec.trusts_forwarded(Some(ip("10.42.0.5"))));
        assert!(
            !sec.trusts_forwarded(Some(ip("203.0.113.9"))),
            "a direct client is not a proxy"
        );
        assert!(!sec.trusts_forwarded(None));
    }

    #[test]
    fn defaults_are_sane() {
        let cfg = Config::default();
        assert_eq!(cfg.server.bind, "0.0.0.0:8080");
        assert!(cfg.has_role(Role::Api));
        assert!(cfg.has_role(Role::Controller));
        assert_eq!(cfg.security.cookie_secure, CookieSecure::AUTO);
        assert!(
            cfg.database.url.is_empty(),
            "PostgreSQL has no default URL (ADR-025)"
        );
        assert!(cfg.agent.bind.is_none(), "AgentLink is off unless configured");
        assert!(!cfg.build.enabled, "builds are off unless configured");
        assert!(!cfg.git.github_enabled());
    }

    #[test]
    fn unpinned_build_images_are_reported() {
        let mut build = BuildCfg::default();
        assert_eq!(build.unpinned_images().len(), 2, "the tag-pinned defaults");
        build.buildkit_image = format!("moby/buildkit@sha256:{}", "a".repeat(64));
        build.fetch_image = format!("alpine/git@sha256:{}", "b".repeat(64));
        assert!(build.unpinned_images().is_empty());
        build.railpack_frontend = Some("ghcr.io/railwayapp/railpack-frontend".into());
        assert_eq!(
            build.unpinned_images(),
            vec!["ghcr.io/railwayapp/railpack-frontend"]
        );
    }

    #[test]
    fn the_github_app_needs_all_three_settings() {
        let mut git = GitCfg {
            github_app_id: Some(1),
            github_private_key_file: Some("/etc/kuben/github.pem".into()),
            ..GitCfg::default()
        };
        assert!(!git.github_enabled(), "no webhook secret");
        git.github_webhook_secret = Some("s3cret".into());
        assert!(git.github_enabled());
        git.github_private_key_file = Some(String::new());
        assert!(!git.github_enabled());
    }

    fn env<'a>(vars: &'a [(&'a str, &'a str)]) -> impl Fn(&str) -> Option<OsString> + 'a {
        move |key| {
            vars.iter()
                .find(|(name, _)| *name == key)
                .map(|(_, value)| OsString::from(value))
        }
    }

    #[test]
    fn state_dir_keeps_an_existing_data_volume() {
        let dir = default_data_dir(
            true,
            env(&[("STATE_DIRECTORY", "/var/lib/kuben"), ("HOME", "/home/u")]),
        );
        assert_eq!(dir, PathBuf::from("/data"));
    }

    #[test]
    fn state_dir_needs_no_root_on_a_fresh_server() {
        let systemd = env(&[
            ("STATE_DIRECTORY", "/var/lib/kuben:/var/lib/other"),
            ("HOME", "/root"),
        ]);
        assert_eq!(default_data_dir(false, systemd), PathBuf::from("/var/lib/kuben"));
        let xdg = env(&[("XDG_STATE_HOME", "/xdg"), ("HOME", "/home/u")]);
        assert_eq!(default_data_dir(false, xdg), PathBuf::from("/xdg/kuben"));
        let home = env(&[("STATE_DIRECTORY", ""), ("HOME", "/home/u")]);
        assert_eq!(
            default_data_dir(false, home),
            PathBuf::from("/home/u/.local/state/kuben")
        );
        assert_eq!(default_data_dir(false, env(&[])), PathBuf::from("."));
    }

    #[test]
    fn warns_when_the_secure_cookie_meets_plain_http() {
        let mut cfg = Config::default();
        assert!(
            cfg.insecure_cookie_warning(false).is_none(),
            "auto over http is not Secure"
        );
        cfg.security.cookie_secure = CookieSecure::Fixed(true);
        assert!(
            cfg.insecure_cookie_warning(false).is_some(),
            "forced Secure on 0.0.0.0 over http"
        );
        assert!(cfg.insecure_cookie_warning(true).is_none(), "in a pod");
        cfg.server.public_url = Some("https://kuben.example.com".into());
        assert!(cfg.insecure_cookie_warning(false).is_none(), "https public URL");
        cfg.server.public_url = None;
        cfg.server.bind = "127.0.0.1:8080".into();
        assert!(cfg.insecure_cookie_warning(false).is_none(), "loopback only");
        cfg.server.bind = "0.0.0.0:8080".into();
        cfg.security.cookie_secure = CookieSecure::Fixed(false);
        assert!(cfg.insecure_cookie_warning(false).is_none(), "cookie not Secure");
    }

    #[test]
    fn console_url_and_bind_helpers() {
        let mut cfg = Config::default();
        assert_eq!(cfg.bind_port(), 8080);
        assert!(!cfg.bind_is_loopback());
        cfg.server.bind = "127.0.0.1:3000".into();
        assert_eq!(cfg.bind_port(), 3000);
        assert!(cfg.bind_is_loopback());
        assert_eq!(
            cfg.console_url_with_host("203.0.113.7"),
            "http://203.0.113.7:3000"
        );
        cfg.server.public_url = Some("https://kuben.example.com/".into());
        assert_eq!(cfg.console_url_with_host("ignored"), "https://kuben.example.com");
        cfg.server.state_dir = Some("/var/lib/kuben".into());
        assert_eq!(cfg.state_dir(), PathBuf::from("/var/lib/kuben"));
    }

    #[test]
    fn setup_wizard_only_without_a_configured_password() {
        let mut cfg = Config::default();
        let outside = !in_cluster();
        assert_eq!(cfg.setup_wizard(), outside);
        cfg.bootstrap.admin_password = Some(String::new());
        assert_eq!(cfg.setup_wizard(), outside, "empty counts as unset");
        cfg.bootstrap.admin_password = Some("configured".into());
        assert!(!cfg.setup_wizard());
    }

    #[test]
    fn cookie_secure_auto_follows_the_public_url() {
        let mut cfg = Config::default();
        assert!(!cfg.cookie_secure(), "no public URL: plain http install");
        cfg.server.public_url = Some("http://203.0.113.7:3000".into());
        assert!(!cfg.cookie_secure());
        cfg.server.public_url = Some("https://kuben.example.com".into());
        assert!(cfg.cookie_secure());
        cfg.security.cookie_secure = CookieSecure::Fixed(false);
        assert!(!cfg.cookie_secure(), "an explicit value wins");
    }

    #[test]
    #[allow(clippy::result_large_err)]
    fn cookie_secure_parses_bools_and_auto() {
        figment::Jail::expect_with(|jail| {
            for (value, want) in [
                ("true", CookieSecure::Fixed(true)),
                ("false", CookieSecure::Fixed(false)),
                ("auto", CookieSecure::AUTO),
            ] {
                jail.set_env("KUBEN_SECURITY__COOKIE_SECURE", value);
                let cfg: Config = Config::figment().extract()?;
                assert_eq!(cfg.security.cookie_secure, want, "{value}");
            }
            jail.set_env("KUBEN_SECURITY__COOKIE_SECURE", "sometimes");
            assert!(Config::figment().extract::<Config>().is_err());
            Ok(())
        });
    }

    #[test]
    #[allow(clippy::result_large_err)] // figment::Jail closures return figment::Error
    fn env_overrides_nested_keys() {
        figment::Jail::expect_with(|jail| {
            jail.set_env("KUBEN_SERVER__BIND", "127.0.0.1:1234");
            jail.set_env("KUBEN_SECURITY__SESSION_TTL_HOURS", "1");
            let cfg: Config = Config::figment().extract()?;
            assert_eq!(cfg.server.bind, "127.0.0.1:1234");
            assert_eq!(cfg.security.session_ttl_hours, 1);
            Ok(())
        });
    }
}

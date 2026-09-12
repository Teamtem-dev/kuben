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
    /// `sqlite://<path>` or `postgres://user:pass@host/db`; defaults to
    /// [`default_sqlite_url`].
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
    pub password_min_length: usize,
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
    /// owner-only file next to the database. It is never logged.
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
        }
    }
}

impl Default for DatabaseCfg {
    fn default() -> Self {
        Self {
            url: default_sqlite_url(),
            max_connections: 4,
        }
    }
}

impl DatabaseCfg {
    /// The database file, when the URL names an on-disk SQLite database.
    #[must_use]
    pub fn sqlite_file(&self) -> Option<PathBuf> {
        let rest = self.url.strip_prefix("sqlite:")?;
        let rest = rest.strip_prefix("//").unwrap_or(rest);
        let path = rest.split('?').next().unwrap_or_default();
        (!path.is_empty() && !path.contains(":memory:")).then(|| PathBuf::from(path))
    }
}

/// The volume of the container image and the Helm chart, and where binaries
/// before 1.0.3 kept their database.
const LEGACY_DATA_DIR: &str = "/data";

/// SQLite URL used when `database.url` is not configured.
///
/// An existing `/data` wins: it is the container volume (the image and the
/// chart also set the URL explicitly) and where earlier binaries kept their
/// data, so upgrading never starts over with an empty database. On a fresh
/// server the database goes to systemd's `StateDirectory=`, else to the
/// user's state directory (`~/.local/state/kuben`), so `kuben doctor` and
/// `kuben serve` work without root.
#[must_use]
pub fn default_sqlite_url() -> String {
    let dir = default_data_dir(Path::new(LEGACY_DATA_DIR).is_dir(), |key| std::env::var_os(key));
    format!("sqlite://{}", dir.join("kuben.db").display())
}

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
        let bind = &self.server.bind;
        let loopback = bind
            .parse::<SocketAddr>()
            .map_or_else(|_| bind.starts_with("localhost:"), |addr| addr.ip().is_loopback());
        if loopback {
            return None;
        }
        let port = bind.rsplit(':').next().unwrap_or("8080");
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
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_sane() {
        let cfg = Config::default();
        assert_eq!(cfg.server.bind, "0.0.0.0:8080");
        assert!(cfg.has_role(Role::Api));
        assert!(cfg.has_role(Role::Controller));
        assert_eq!(cfg.security.cookie_secure, CookieSecure::AUTO);
        assert!(cfg.database.url.starts_with("sqlite://"));
    }

    fn env<'a>(vars: &'a [(&'a str, &'a str)]) -> impl Fn(&str) -> Option<OsString> + 'a {
        move |key| {
            vars.iter()
                .find(|(name, _)| *name == key)
                .map(|(_, value)| OsString::from(value))
        }
    }

    #[test]
    fn database_default_keeps_an_existing_data_volume() {
        let dir = default_data_dir(
            true,
            env(&[("STATE_DIRECTORY", "/var/lib/kuben"), ("HOME", "/home/u")]),
        );
        assert_eq!(dir, PathBuf::from("/data"));
    }

    #[test]
    fn database_default_needs_no_root_on_a_fresh_server() {
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
    fn sqlite_file_of_database_urls() {
        let file = |url: &str| {
            DatabaseCfg {
                url: url.into(),
                max_connections: 1,
            }
            .sqlite_file()
        };
        assert_eq!(
            file("sqlite:///data/kuben.db"),
            Some(PathBuf::from("/data/kuben.db"))
        );
        assert_eq!(
            file("sqlite://./.dev/kuben.db?mode=rwc"),
            Some(PathBuf::from("./.dev/kuben.db"))
        );
        assert_eq!(file("sqlite::memory:"), None);
        assert_eq!(file("postgres://u:p@db/kuben"), None);
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

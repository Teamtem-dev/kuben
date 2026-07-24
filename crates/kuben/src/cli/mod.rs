pub mod admin;
pub mod backup;
pub mod doctor;

use clap::{Args, Parser, Subcommand, ValueEnum};
use kuben_core::config::{Config, Role};

pub const VERSION: &str = env!("CARGO_PKG_VERSION");

#[must_use]
pub fn version_string() -> String {
    format!(
        "kuben {VERSION} ({} {})",
        std::env::consts::OS,
        std::env::consts::ARCH
    )
}

#[derive(Debug, Parser)]
#[command(name = "kuben", version = VERSION, about = "Kuben — Kubernetes-native PaaS in a single binary")]
pub struct Cli {
    /// Path to a TOML config file (merged over defaults, under env vars).
    #[arg(long, global = true, env = "KUBEN_CONFIG")]
    pub config: Option<std::path::PathBuf>,

    #[command(subcommand)]
    pub command: Command,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Run the server (API, controllers, activator) according to --roles.
    Serve(ServeOpts),
    /// Apply database migrations and exit.
    Migrate,
    /// Check the environment (database, cluster, gateway, cert-manager) and exit.
    Doctor,
    /// Reset (or create) the admin user's password.
    ResetAdmin(ResetAdminOpts),
    /// Export Projects, Environments and Apps (CRDs) to a directory (secret values are never exported).
    Backup(BackupOpts),
    /// Restore a backup created by `kuben backup`.
    Restore(RestoreOpts),
    /// Print version information.
    Version,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum RoleArg {
    All,
    Api,
    Controller,
    Activator,
}

impl From<RoleArg> for Role {
    fn from(r: RoleArg) -> Self {
        match r {
            RoleArg::All => Self::All,
            RoleArg::Api => Self::Api,
            RoleArg::Controller => Self::Controller,
            RoleArg::Activator => Self::Activator,
        }
    }
}

#[derive(Debug, Args)]
pub struct ServeOpts {
    /// Roles to run in this process (comma separated). Defaults to config.
    #[arg(long, value_delimiter = ',', env = "KUBEN_ROLES")]
    pub roles: Vec<RoleArg>,
    /// Development mode: pretty logs, insecure cookie, in-memory SQLite if no DB configured.
    #[arg(long)]
    pub dev: bool,
}

#[derive(Debug, Args)]
pub struct ResetAdminOpts {
    /// New password. If omitted, a random one is generated and printed.
    #[arg(long, env = "KUBEN_ADMIN_PASSWORD")]
    pub password: Option<String>,
}

#[derive(Debug, Args)]
pub struct BackupOpts {
    /// Output directory (created if missing).
    #[arg(long, default_value = "./kuben-backup")]
    pub out: std::path::PathBuf,
}

#[derive(Debug, Args)]
pub struct RestoreOpts {
    /// Directory produced by `kuben backup`.
    #[arg(long)]
    pub from: std::path::PathBuf,
}

impl Cli {
    /// Load configuration, layering `--config` (if given) above the defaults
    /// and below environment variables.
    pub fn load_config(&self) -> anyhow::Result<Config> {
        use figment_shim::{Format, Toml};
        let mut figment = Config::figment();
        if let Some(path) = &self.config {
            figment = figment.merge(Toml::file(path)).merge(figment_shim::env());
        }
        let mut cfg: Config = figment.extract()?;
        if let Command::Serve(opts) = &self.command {
            if !opts.roles.is_empty() {
                cfg.server.roles = opts.roles.iter().copied().map(Role::from).collect();
            }
            if opts.dev {
                cfg.security.cookie_secure = false;
                cfg.telemetry.log_format = "pretty".into();
                if cfg.telemetry.log_level == "info" {
                    cfg.telemetry.log_level = "debug,hyper=info,h2=info,tower=info".into();
                }
            }
        }
        Ok(cfg)
    }
}

/// Tiny re-export shim so `kuben-core`'s figment version is the only one used.
mod figment_shim {
    pub use figment::providers::{Format, Toml};
    pub fn env() -> figment::providers::Env {
        figment::providers::Env::prefixed("KUBEN_").split("__")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_serve_roles() {
        let cli = Cli::parse_from(["kuben", "serve", "--roles=api,controller", "--dev"]);
        let Command::Serve(opts) = cli.command else {
            panic!("expected serve")
        };
        assert_eq!(opts.roles, vec![RoleArg::Api, RoleArg::Controller]);
        assert!(opts.dev);
    }

    #[test]
    fn dev_flag_disables_secure_cookie() {
        let cli = Cli::parse_from(["kuben", "serve", "--dev"]);
        let cfg = cli.load_config().expect("config");
        assert!(!cfg.security.cookie_secure);
        assert_eq!(cfg.telemetry.log_format, "pretty");
    }
}

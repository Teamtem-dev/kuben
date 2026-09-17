pub mod admin;
pub mod agent;
pub mod backup;
pub mod client;
pub mod doctor;
pub mod setup;
pub mod ui;

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
    Doctor(DoctorOpts),
    /// Reset (or create) the admin user's password.
    ResetAdmin(ResetAdminOpts),
    /// Print a fresh link to the first-run setup page (`/setup`), with its token.
    SetupToken,
    /// Issue a bootstrap token for a cluster's agent: printed once, only its
    /// hash is kept.
    AgentToken(AgentTokenOpts),
    /// Make this Linux server a Kuben server: k3s if needed, a system user, the
    /// config, a systemd service, the firewall. Re-run to upgrade or repair.
    Setup(setup::SetupOpts),
    /// An app's state and Doctor (`kuben status shop/prod/web`); without an
    /// app, the service, admin account, cluster and console address of this
    /// server.
    Status(client::StatusOpts),
    /// Sign in to a Kuben server with an API token; later commands use it.
    Login(client::LoginOpts),
    /// The apps you can see, with their state.
    Apps(client::AppsOpts),
    /// Deploy an image to an app and wait until it runs.
    Deploy(client::DeployOpts),
    /// An app's log lines; `-f` keeps following them.
    Logs(client::LogsOpts),
    /// Return an app to an earlier revision.
    Rollback(client::RollbackOpts),
    /// Remove the service installed by `kuben setup` (with --purge: everything).
    Uninstall(setup::UninstallOpts),
    /// Back up the database (and, if asked, the secret keyring) into a new
    /// directory under `backup.dir`.
    Backup(BackupOpts),
    /// Restore a backup made by `kuben backup` into an empty database.
    Restore(RestoreOpts),
    /// Print version information; `--bundle` adds what a release installs
    /// besides Kuben, with its digests.
    Version(VersionOpts),
    /// Copy this binary to a path (the chart's backup job runs it next to
    /// PostgreSQL's client tools).
    #[command(hide = true)]
    CopySelf(CopySelfOpts),
}

#[derive(Debug, Args)]
pub struct CopySelfOpts {
    /// Where the copy goes (made executable).
    pub to: std::path::PathBuf,
}

/// `kuben copy-self`.
pub fn copy_self(opts: &CopySelfOpts) -> anyhow::Result<()> {
    let me = std::env::current_exe()?;
    std::fs::copy(&me, &opts.to)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&opts.to, std::fs::Permissions::from_mode(0o755))?;
    }
    Ok(())
}

/// Options of `kuben version`.
#[derive(Debug, Default, clap::Args)]
pub struct VersionOpts {
    /// Also the pinned k3s, Gateway API, cert-manager and PostgreSQL.
    #[arg(long)]
    pub bundle: bool,
    /// With --bundle: the lock file itself (JSON).
    #[arg(long, requires = "bundle")]
    pub json: bool,
}

/// Options of `kuben doctor`.
#[derive(Debug, Default, clap::Args)]
pub struct DoctorOpts {
    /// Check only the cluster (before installing Kuben into it, BYOK): what
    /// each feature needs, what is there, and the permissions Kuben needs.
    /// No database is needed.
    #[arg(long)]
    pub cluster: bool,
}

/// Options of `kuben agent-token`.
#[derive(Debug, clap::Args)]
pub struct AgentTokenOpts {
    /// The cluster the agent runs in; made if it does not exist yet.
    #[arg(long, default_value = "primary")]
    pub cluster: String,
    /// The organization (slug); the bootstrap organization by default.
    #[arg(long)]
    pub org: Option<String>,
    /// How long the token stays valid, in minutes.
    #[arg(long, default_value_t = 30)]
    pub ttl_minutes: u64,
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
    /// Development mode: pretty logs and an insecure cookie. The database is
    /// still PostgreSQL (`database.url`).
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
    /// Directory the backup is written under [default: `backup.dir`].
    #[arg(long)]
    pub out: Option<std::path::PathBuf>,
    /// Also copy the secret keyring into the backup. Whoever holds such a
    /// backup can read every secret: keep it encrypted.
    #[arg(long)]
    pub include_keyring: bool,
    /// Backups kept under the directory [default: `backup.keep`].
    #[arg(long)]
    pub keep: Option<u32>,
    /// Record the backup as scheduled (the timer and the CronJob set it).
    #[arg(long, hide = true)]
    pub scheduled: bool,
}

#[derive(Debug, Args)]
pub struct RestoreOpts {
    /// A backup directory (`kuben-<time>`) made by `kuben backup`.
    #[arg(long)]
    pub from: std::path::PathBuf,
    /// Only check that the backup is intact and restorable.
    #[arg(long)]
    pub check: bool,
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
                cfg.security.cookie_secure = kuben_core::config::CookieSecure::Fixed(false);
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
        assert!(!cfg.cookie_secure());
        assert_eq!(cfg.telemetry.log_format, "pretty");
    }
}

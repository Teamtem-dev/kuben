//! `kuben setup`: this machine becomes a Kuben server. `kuben status` and
//! `kuben uninstall` share its pieces.
//!
//! Every step checks before it acts, so the same command installs, upgrades,
//! repairs and resumes an interrupted run: an existing k3s or kubeconfig is
//! used instead of installing k3s, an existing config file is kept, and the
//! service restarts only when its binary, unit or configuration changed; a
//! repeated run changes nothing. Linux with systemd, as root; everything else
//! gets a clear message and no changes. `kuben setup --plan` shows what a run
//! would do without doing it.
//!
//! Every run is recorded in the install journal ([`journal`]), with the owner
//! of each resource: the binary at `/usr/local/bin/kuben`, the system user
//! `kuben`, `/etc/kuben/config.toml`, `/var/lib/kuben` (kubeconfig copy, setup
//! token, journal), the PostgreSQL role and database `kuben` (PostgreSQL from
//! the distribution's packages when it was missing), `kuben.service`, a
//! firewall rule, and k3s. `kuben uninstall` removes only what setup created;
//! what was there before stays (I12).

use std::{
    io::{Read as _, Write as _},
    net::{TcpListener, TcpStream},
    path::{Path, PathBuf},
    process::Command,
    time::{Duration, Instant},
};

use anyhow::{Context as _, anyhow, bail};
use clap::Args;
use kuben_core::config::Config;

use self::journal::{Book, Kind};
use super::ui::Ui;

pub mod journal;
mod plan;
pub mod platform;

pub const BIN: &str = "/usr/local/bin/kuben";
pub const USER: &str = "kuben";
pub const STATE_DIR: &str = "/var/lib/kuben";
/// The server's own PostgreSQL, reached over its Unix socket with peer
/// authentication as [`USER`], so no password is stored (ADR-025).
pub const LOCAL_DATABASE_URL: &str = "postgres:///kuben?host=/run/postgresql&user=kuben";
pub const CONFIG_DIR: &str = "/etc/kuben";
pub const CONFIG_FILE: &str = "/etc/kuben/config.toml";
pub const UNIT_FILE: &str = "/etc/systemd/system/kuben.service";
const UNIT_NAME: &str = "kuben.service";
pub const K3S_KUBECONFIG: &str = "/etc/rancher/k3s/k3s.yaml";
pub const K3S_UNINSTALL: &str = "/usr/local/bin/k3s-uninstall.sh";
/// Written by setup before the install journal existed, when it installed
/// k3s itself; still honoured for such installs.
pub const K3S_MARKER: &str = "/var/lib/kuben/.k3s-installed-by-kuben";
/// The first line of the unit file setup writes.
const UNIT_MARKER: &str = "# Written by `kuben setup`";
pub const DOCS: &str = "https://kuben.teamtem.com/docs/getting-started/binary/";

#[derive(Debug, Args)]
pub struct SetupOpts {
    /// Port of the console and API [default: 3000, or the next free port when
    /// 3000 is taken; an existing install keeps its port].
    #[arg(long, env = "KUBEN_PORT")]
    pub port: Option<u16>,
    /// Manage this cluster instead of installing k3s.
    #[arg(long, env = "KUBEN_KUBECONFIG")]
    pub kubeconfig: Option<PathBuf>,
    /// Never install k3s; fail when no cluster is found.
    #[arg(long)]
    pub no_k3s: bool,
    /// Listen on 127.0.0.1 only (reach the console through an SSH tunnel).
    #[arg(long)]
    pub bind_local: bool,
    /// Go ahead on warnings without asking.
    #[arg(long, short = 'y')]
    pub yes: bool,
    /// Show what setup would do, and change nothing.
    #[arg(long)]
    pub plan: bool,
    /// Base domain for app hostnames (`<app>-<environment>.<domain>`); point
    /// a wildcard DNS record at this server.
    #[arg(long, env = "KUBEN_DOMAIN")]
    pub domain: Option<String>,
    /// Email for Let's Encrypt: apps get HTTPS certificates. Without it they
    /// are served over plain HTTP.
    #[arg(long, env = "KUBEN_ACME_EMAIL")]
    pub acme_email: Option<String>,
    /// Use Let's Encrypt's staging server (untrusted certificates, for tests).
    #[arg(long)]
    pub acme_staging: bool,
    /// How an installed k3s keeps its state: sqlite (one server) or etcd (a
    /// server that will grow).
    #[arg(long, value_enum, default_value_t)]
    pub datastore: platform::Datastore,
}

impl SetupOpts {
    fn wanted(&self) -> platform::Wanted<'_> {
        platform::Wanted {
            domain: self.domain.as_deref(),
            acme_email: self.acme_email.as_deref(),
            acme_staging: self.acme_staging,
        }
    }
}

#[derive(Debug, Args)]
pub struct UninstallOpts {
    /// Also delete the data, the configuration, the binary, and k3s when
    /// `kuben setup` installed it.
    #[arg(long)]
    pub purge: bool,
    /// Do not ask for confirmation.
    #[arg(long, short = 'y')]
    pub yes: bool,
}

// ---------------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------------

pub fn setup(opts: &SetupOpts) -> anyhow::Result<()> {
    let ui = Ui::new();
    ui.banner(super::VERSION);
    if opts.plan {
        plan::show(ui, opts);
        return Ok(());
    }
    report_download(ui);
    let host = preflight(ui, opts)?;
    // The journal lives in the state directory: whether that directory is
    // new is known only before the journal is first saved.
    let fresh_state = !Path::new(STATE_DIR).exists();
    let mut book = Book::open(Path::new(STATE_DIR), user_ids(USER).map(|(_, gid)| gid))?;
    if let Some(last) = book.journal().unfinished() {
        let stopped = last
            .steps
            .iter()
            .rev()
            .find(|s| s.result == journal::StepResult::Failed)
            .map_or_else(
                || "was interrupted".to_owned(),
                |s| format!("stopped at {}", s.id),
            );
        ui.note(&format!(
            "The last run {stopped}; this run checks every step again and carries on."
        ));
    }
    book.begin(super::VERSION)?;
    let result = install(ui, opts, &host, fresh_state, &mut book);
    book.finish(result.as_ref().err())?;
    result?;
    if !book.journal().last_run_changed() {
        ui.note("Nothing changed: this server was already set up this way.");
    }
    Ok(())
}

fn install(ui: Ui, opts: &SetupOpts, host: &Host, fresh_state: bool, book: &mut Book) -> anyhow::Result<()> {
    adopt_earlier_install(book)?;
    book.start("binary");
    let binary_changed = install_binary(ui, book)?;
    book.start("user");
    let (uid, gid) = ensure_user(ui, fresh_state, book)?;
    book.set_group(gid);
    book.start("cluster");
    let kubeconfig = ensure_cluster(ui, opts, uid, gid, book)?;
    let configured = std::fs::read_to_string(CONFIG_FILE).ok();
    book.start("database");
    ensure_database(ui, configured.as_deref(), book)?;
    book.start("config");
    let port = choose_port(
        ui,
        opts.port,
        configured.as_deref().and_then(config_port),
        opts.yes,
    )?;
    // The managed path: this server's k3s gets what exposes apps (ADR-031)
    // and its own agent (M2.8). A cluster brought with --kubeconfig is left
    // as it is.
    let managed = opts.kubeconfig.is_none() && Path::new(K3S_KUBECONFIG).exists();
    let hub = managed
        .then(kuben_api::host::advertise_ip)
        .flatten()
        .map(|ip| ip.to_string());
    let config_changed = write_config(
        ui,
        opts,
        &kubeconfig,
        configured.as_deref(),
        port,
        hub.as_deref(),
        book,
    )?;
    if managed {
        book.start("platform");
        platform::ensure_platform(ui, &kubeconfig, &opts.wanted(), hub.is_some(), book)?;
    }
    let cfg = Config::load().context("reading /etc/kuben/config.toml")?;
    let port = cfg.bind_port();
    book.start("service");
    start_service(ui, port, binary_changed || config_changed, book)?;
    if managed {
        book.start("kubenconfig");
        platform::ensure_kuben_config(ui, &kubeconfig, &opts.wanted(), book)?;
    }
    book.start("firewall");
    open_firewall(ui, port, hub.is_some(), book)?;
    warn_web_ports(ui);
    let wanted = opts.wanted();
    announce(
        ui,
        &cfg,
        uid,
        gid,
        configured.is_none(),
        host,
        managed.then_some(&wanted),
    )?;
    Ok(())
}

/// A server set up before the journal existed: its unit file (or k3s
/// marker) shows setup made it, and it made the rest too — the binary, the
/// user, the directories, the configuration and this server's own database.
/// Recorded as Kuben's once, so `uninstall --purge` still removes them.
fn adopt_earlier_install(book: &mut Book) -> anyhow::Result<()> {
    let unit = std::fs::read_to_string(UNIT_FILE).ok();
    let earlier = book.journal().resources.is_empty()
        && (unit.as_deref().is_some_and(written_by_setup) || Path::new(K3S_MARKER).exists());
    if !earlier {
        return Ok(());
    }
    let local = data_home(std::fs::read_to_string(CONFIG_FILE).ok().as_deref()) == DataHome::Local;
    let mut ours = vec![
        (Kind::File, BIN),
        (Kind::SystemUser, USER),
        (Kind::Directory, STATE_DIR),
        (Kind::Directory, CONFIG_DIR),
        (Kind::File, CONFIG_FILE),
        (Kind::SystemdUnit, UNIT_NAME),
    ];
    if local {
        ours.extend([(Kind::PostgresRole, USER), (Kind::PostgresDatabase, USER)]);
    }
    for (kind, name) in ours {
        book.claim(kind, name, true)?;
    }
    Ok(())
}

/// A unit file setup wrote (also before it carried [`UNIT_MARKER`]).
fn written_by_setup(unit: &str) -> bool {
    unit.starts_with(UNIT_MARKER) || unit.contains(&format!("ExecStart={BIN} serve"))
}

/// Port Kuben listens on when neither `--port` nor an install says otherwise.
pub const DEFAULT_PORT: u16 = 3000;

/// The port to use. A running install keeps its own. An explicit `--port`, or
/// the port of a service that already ran, must be free: another process
/// holding it is named. Otherwise 3000; when something else has it (the
/// Dokploy console, a Node app…) the operator is asked, with the next free
/// port as the answer Enter gives, and without a terminal (or with `--yes`)
/// that port is taken, so a first install never stops on it.
fn choose_port(ui: Ui, requested: Option<u16>, configured: Option<u16>, yes: bool) -> anyhow::Result<u16> {
    if service_active()
        && (requested.is_none() || requested == configured)
        && let Some(port) = configured
    {
        return Ok(port); // the port is ours
    }
    // A service that was started once keeps its port; a first run that stopped
    // halfway (config written, never started) may still move.
    let settled = configured.is_some() && Path::new(UNIT_FILE).exists();
    let wanted = requested
        .or(if settled { configured } else { None })
        .unwrap_or(DEFAULT_PORT);
    let step = ui.step(format!("Checking port {wanted}"));
    if port_free(wanted) {
        step.done("free");
        return Ok(wanted);
    }
    let owner = port_owner(wanted).unwrap_or_else(|| "another process".to_owned());
    let suggestion = if requested.is_none() && !settled {
        next_free_port(wanted)
    } else {
        None
    };
    let Some(suggestion) = suggestion else {
        step.fail(format!("used by {owner}"));
        bail!("port {wanted} is already in use by {owner}; pick another with `kuben setup --port <port>`");
    };
    step.warn(format!("used by {owner}"));
    let chosen = |port: u16, why: &str| {
        ui.done("Port", format!("{port}{why}"));
        Ok(port)
    };
    if yes {
        return chosen(suggestion, ", the next free one");
    }
    for _ in 0..3 {
        let Some(answer) = ui.ask("Which port should Kuben use?", &suggestion.to_string()) else {
            return chosen(suggestion, ", the next free one");
        };
        match parse_port(&answer) {
            Some(port) if port_free(port) => return chosen(port, ""),
            Some(port) => ui.warn(
                &format!("Port {port}"),
                format!(
                    "used by {}",
                    port_owner(port).unwrap_or_else(|| "another process".to_owned())
                ),
            ),
            None => ui.warn("Port", format!("{answer:?} is not a port number (1–65535)")),
        }
    }
    bail!("no usable port chosen; run `kuben setup --port <port>`")
}

fn next_free_port(after: u16) -> Option<u16> {
    (after.saturating_add(1)..after.saturating_add(100)).find(|port| port_free(*port))
}

fn parse_port(answer: &str) -> Option<u16> {
    answer.trim().parse::<u16>().ok().filter(|port| *port > 0)
}

/// The installer's download, reported here so that the banner comes first:
/// install.sh passes `KUBEN_INSTALLED="<version> <target> <path> <sha256>"`
/// when it hands over to `kuben setup`.
fn report_download(ui: Ui) {
    let Ok(info) = std::env::var("KUBEN_INSTALLED") else {
        return;
    };
    let fields: Vec<&str> = info.split_whitespace().collect();
    if let [version, target, path, sha] = fields.as_slice() {
        ui.done(
            &format!("Installed kuben {version} ({target}) to {path}"),
            format!("sha256 {sha}"),
        );
    }
}

fn port_free(port: u16) -> bool {
    TcpListener::bind(("0.0.0.0", port)).is_ok()
}

/// Apps get public addresses through the cluster's ingress on 80 and 443
/// (Traefik on k3s). Another web server on the host (Dokploy's Traefik,
/// nginx, Caddy) keeps them, so say so before anyone wonders why an app
/// has no public address. The console does not depend on them.
fn warn_web_ports(ui: Ui) {
    let taken: Vec<String> = [80, 443]
        .into_iter()
        .filter(|port| !port_free(*port))
        .map(|port| {
            format!(
                "{port} is used by {}",
                port_owner(port).unwrap_or_else(|| "another process".to_owned())
            )
        })
        .collect();
    if !taken.is_empty() {
        ui.warn("Ports 80 and 443", taken.join("; "));
        ui.note(
            "Apps get their public addresses through the cluster's Traefik on these ports; until that\n\
             server moves, the console works but app routes are not reachable from outside.",
        );
    }
}

struct Host {
    os: String,
    arch: &'static str,
    mem_gb: Option<f64>,
}

fn preflight(ui: Ui, opts: &SetupOpts) -> anyhow::Result<Host> {
    let step = ui.step("Preflight checks");
    if !cfg!(target_os = "linux") {
        step.fail("Linux only");
        bail!(
            "`kuben setup` sets up a Linux server; on this machine run `kuben serve` yourself (see {DOCS})"
        );
    }
    if !is_root() {
        step.fail("not root");
        bail!("run it as root: sudo kuben setup");
    }
    if !Path::new("/run/systemd/system").is_dir() {
        step.fail("no systemd");
        bail!("this system does not run systemd; run `kuben serve` under your own supervisor (see {DOCS})");
    }
    let host = Host {
        os: os_name().unwrap_or_else(|| "Linux".to_owned()),
        arch: std::env::consts::ARCH,
        mem_gb: mem_total_gb(),
    };
    let detail = match host.mem_gb {
        Some(gb) => format!("{}, {}, {gb:.0} GB RAM", host.os, host.arch),
        None => format!("{}, {}", host.os, host.arch),
    };
    if in_container() && opts.kubeconfig.is_none() {
        step.fail(detail);
        bail!(
            "this looks like a container; k3s cannot run here. Pass --kubeconfig for a cluster that exists, or run kuben setup on a VM or bare metal"
        );
    }
    if host.mem_gb.is_some_and(|gb| gb < 1.9) {
        step.warn(detail);
        ui.note("Less than 2 GB of RAM: k3s and Kuben will run, but slowly. 2 GB or more is recommended.");
        // Without a terminal (or with --yes) a warning does not stop the install.
        if !opts.yes && ui.confirm("Continue anyway?") == Some(false) {
            bail!("aborted");
        }
    } else {
        step.done(detail);
    }
    Ok(host)
}

/// `true` when the binary at [`BIN`] changed.
fn install_binary(ui: Ui, book: &mut Book) -> anyhow::Result<bool> {
    let me = std::env::current_exe().context("locating the running binary")?;
    let target = Path::new(BIN);
    let existed = target.exists();
    let version = format!("v{}", super::VERSION);
    // The installer put it there (install.sh records that download itself),
    // or it is this very binary: nothing to copy.
    let installed_now = std::env::var_os("KUBEN_INSTALLED").is_some();
    if same_file(&me, target) {
        book.claim(Kind::File, BIN, installed_now)?;
        book.done(installed_now, format!("{version} at {BIN}"))?;
        return Ok(installed_now);
    }
    if existed && std::fs::read(&me).ok() == std::fs::read(target).ok() {
        book.claim(Kind::File, BIN, false)?;
        book.done(false, format!("{version} at {BIN}, up to date"))?;
        return Ok(false);
    }
    let step = ui.step("Installing the kuben binary");
    if let Some(dir) = target.parent() {
        std::fs::create_dir_all(dir)?;
    }
    // A running service keeps its old inode open; replace, do not overwrite.
    let staged = target.with_extension("new");
    std::fs::copy(&me, &staged)
        .with_context(|| format!("copying {} to {}", me.display(), staged.display()))?;
    set_mode(&staged, 0o755)?;
    std::fs::rename(&staged, target)?;
    book.claim(Kind::File, BIN, !existed)?;
    step.done(format!("{version} → {BIN}"));
    book.done(true, format!("{version} → {BIN}"))?;
    Ok(true)
}

fn ensure_user(ui: Ui, fresh_state: bool, book: &mut Book) -> anyhow::Result<(u32, u32)> {
    let step = ui.step("Creating the kuben system user");
    let existed = user_ids(USER);
    let ids = if let Some(ids) = existed {
        step.done("exists");
        ids
    } else {
        run(&[
            "useradd",
            "--system",
            "--home-dir",
            STATE_DIR,
            "--shell",
            "/usr/sbin/nologin",
            "--user-group",
            USER,
        ])?;
        let ids = user_ids(USER).ok_or_else(|| anyhow!("user {USER} not found after useradd"))?;
        step.done(format!("{USER} (uid {})", ids.0));
        ids
    };
    book.claim(Kind::SystemUser, USER, existed.is_none())?;
    let mut changed = existed.is_none();
    for dir in [STATE_DIR, CONFIG_DIR] {
        let created = if dir == STATE_DIR {
            fresh_state
        } else {
            !Path::new(dir).is_dir()
        };
        std::fs::create_dir_all(dir)?;
        book.claim(Kind::Directory, dir, created)?;
        changed |= created;
    }
    set_mode(Path::new(STATE_DIR), 0o750)?;
    chown(Path::new(STATE_DIR), ids.0, ids.1)?;
    book.done(changed, format!("{USER} (uid {})", ids.0))?;
    Ok(ids)
}

/// Where a configuration keeps Kuben's data: this server's PostgreSQL (also
/// when there is no configuration yet: setup writes one that uses it), the
/// SQLite file of Kuben 1.x, or a PostgreSQL of the operator's own.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DataHome {
    Local,
    Sqlite,
    External,
}

fn data_home(config: Option<&str>) -> DataHome {
    let Some(text) = config else {
        return DataHome::Local;
    };
    let url = text
        .lines()
        .map(str::trim_start)
        .find_map(|line| line.strip_prefix("url")?.trim_start().strip_prefix('='))
        .map(|value| value.trim().trim_matches('"'));
    match url {
        Some(url) if url.starts_with("sqlite:") => DataHome::Sqlite,
        Some(url) if url != LOCAL_DATABASE_URL => DataHome::External,
        _ => DataHome::Local,
    }
}

/// The PostgreSQL this server keeps its data in (ADR-025): installed from the
/// distribution's packages when missing, started, with the role and database
/// `kuben`. The role logs in over the Unix socket by peer authentication as
/// the `kuben` system user, so it has no password; it owns its database and
/// is no superuser, so row-level security applies to it.
fn ensure_database(ui: Ui, configured: Option<&str>, book: &mut Book) -> anyhow::Result<()> {
    let step = ui.step("PostgreSQL");
    match data_home(configured) {
        DataHome::External => {
            step.done(format!("the database in {CONFIG_FILE}"));
            book.done(false, "an external database")?;
            return Ok(());
        }
        DataHome::Sqlite => {
            step.fail("Kuben 1.x data in SQLite");
            bail!(
                "{CONFIG_FILE} keeps the data in SQLite, as Kuben 1.x did; this version keeps it in \
                 PostgreSQL and does not carry 1.x data over. Delete {CONFIG_FILE} (setup then \
                 writes one for this server's PostgreSQL), or set [database] url to an empty \
                 PostgreSQL of your own, then run kuben setup again"
            );
        }
        DataHome::Local => {}
    }
    let installed = postgres_installed();
    if !installed {
        let os_release = std::fs::read_to_string("/etc/os-release").unwrap_or_default();
        let Some(packages) = packages_of(&os_release) else {
            step.fail("not installed");
            bail!(
                "install PostgreSQL 14 or newer with its systemd service, then run kuben setup again; \
                 or set [database] url in {CONFIG_FILE} to a PostgreSQL of your own"
            );
        };
        ui.command(packages.describe());
        packages.install()?;
    }
    book.claim(Kind::Package, "postgresql", !installed)?;
    let was_running = service_is_active("postgresql");
    run(&["systemctl", "enable", "--now", "--quiet", "postgresql"])?;
    wait_for_postgres(Duration::from_mins(1))?;
    let role = psql_as_postgres("SELECT 1 FROM pg_roles WHERE rolname = 'kuben'")?.trim() != "1";
    if role {
        psql_as_postgres("CREATE ROLE kuben LOGIN")?;
    }
    book.claim(Kind::PostgresRole, USER, role)?;
    let database = psql_as_postgres("SELECT 1 FROM pg_database WHERE datname = 'kuben'")?.trim() != "1";
    if database {
        psql_as_postgres("CREATE DATABASE kuben OWNER kuben")?;
    }
    book.claim(Kind::PostgresDatabase, USER, database)?;
    let version = psql_as_postgres("SHOW server_version")?;
    let detail = format!("PostgreSQL {}, role and database kuben", version.trim());
    step.done(&detail);
    book.done(!installed || !was_running || role || database, detail)?;
    Ok(())
}

fn postgres_installed() -> bool {
    Command::new("systemctl")
        .args(["cat", "postgresql.service"])
        .output()
        .is_ok_and(|o| o.status.success())
}

/// How this distribution installs the PostgreSQL server, from `/etc/os-release`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Packages {
    Apt,
    Dnf,
    Zypper,
}

fn packages_of(os_release: &str) -> Option<Packages> {
    let ids: Vec<&str> = os_release
        .lines()
        .filter_map(|line| line.strip_prefix("ID=").or_else(|| line.strip_prefix("ID_LIKE=")))
        .flat_map(|value| value.trim_matches('"').split_whitespace())
        .collect();
    let any = |names: &[&str]| ids.iter().any(|id| names.contains(id));
    if any(&["debian", "ubuntu"]) {
        Some(Packages::Apt)
    } else if any(&["fedora", "rhel", "centos"]) {
        Some(Packages::Dnf)
    } else if any(&["suse", "opensuse", "sles"]) {
        Some(Packages::Zypper)
    } else {
        None
    }
}

impl Packages {
    const fn describe(self) -> &'static str {
        match self {
            Self::Apt => "apt-get install postgresql",
            Self::Dnf => "dnf install postgresql-server && postgresql-setup --initdb",
            Self::Zypper => "zypper install postgresql-server",
        }
    }

    fn install(self) -> anyhow::Result<()> {
        match self {
            Self::Apt => {
                run(&["apt-get", "update", "-qq"])?;
                run_env(
                    &["apt-get", "install", "-y", "-qq", "postgresql"],
                    &[("DEBIAN_FRONTEND", "noninteractive")],
                )?;
            }
            Self::Dnf => {
                run(&["dnf", "install", "-y", "-q", "postgresql-server"])?;
                if !Path::new("/var/lib/pgsql/data/PG_VERSION").exists() {
                    run(&["postgresql-setup", "--initdb"])?;
                }
            }
            Self::Zypper => {
                run(&["zypper", "--non-interactive", "install", "postgresql-server"])?;
            }
        }
        Ok(())
    }
}

/// Wait until PostgreSQL answers on its Unix socket.
fn wait_for_postgres(timeout: Duration) -> anyhow::Result<()> {
    let start = Instant::now();
    while start.elapsed() < timeout {
        if Path::new("/run/postgresql/.s.PGSQL.5432").exists() && psql_as_postgres("SELECT 1").is_ok() {
            return Ok(());
        }
        std::thread::sleep(Duration::from_secs(1));
    }
    bail!(
        "PostgreSQL did not answer on /run/postgresql within {}s (journalctl -u postgresql has why)",
        timeout.as_secs()
    )
}

/// One statement as the superuser `postgres` over the Unix socket: its
/// unaligned output.
fn psql_as_postgres(sql: &str) -> anyhow::Result<String> {
    run(&[
        "runuser",
        "-u",
        "postgres",
        "--",
        "psql",
        "--no-psqlrc",
        "-v",
        "ON_ERROR_STOP=1",
        "-tAqc",
        sql,
    ])
}

/// The kubeconfig Kuben will use: a copy in the state directory, owned by
/// the service user (k3s writes its own for root only).
fn ensure_cluster(ui: Ui, opts: &SetupOpts, uid: u32, gid: u32, book: &mut Book) -> anyhow::Result<PathBuf> {
    let mut installed = false;
    let source = if let Some(path) = &opts.kubeconfig {
        let step = ui.step("Checking the cluster");
        std::fs::metadata(path).with_context(|| format!("cannot read {}", path.display()))?;
        step.done(format!("kubeconfig {}", path.display()));
        path.clone()
    } else if Path::new(K3S_KUBECONFIG).exists() {
        ui.done("Installing k3s", "already installed");
        // Installs from before the journal left a marker when setup made k3s.
        book.claim(Kind::Cluster, "k3s", Path::new(K3S_MARKER).exists())?;
        PathBuf::from(K3S_KUBECONFIG)
    } else if let Some(existing) = existing_kubeconfig() {
        ui.done(
            "Installing k3s",
            format!("using the cluster in {}", existing.display()),
        );
        existing
    } else if opts.no_k3s {
        ui.fail("Finding a cluster", "none");
        bail!("no cluster found and --no-k3s given: pass --kubeconfig <file>");
    } else {
        platform::install_k3s(ui, opts.datastore)?;
        book.claim(Kind::Cluster, "k3s", true)?;
        installed = true;
        PathBuf::from(K3S_KUBECONFIG)
    };

    let step = ui.step("Waiting for the cluster");
    let nodes = wait_for_nodes(&source, Duration::from_mins(3))?;
    step.done(nodes);

    let copy = Path::new(STATE_DIR).join("kubeconfig");
    let content = std::fs::read(&source).with_context(|| format!("reading {}", source.display()))?;
    let copied = std::fs::read(&copy).ok().as_ref() != Some(&content);
    if copied {
        std::fs::write(&copy, content)?;
    }
    set_mode(&copy, 0o600)?;
    chown(&copy, uid, gid)?;
    book.done(installed || copied, format!("kubeconfig {}", source.display()))?;
    Ok(copy)
}

/// Poll until a node reports Ready; returns a one-line summary.
fn wait_for_nodes(kubeconfig: &Path, timeout: Duration) -> anyhow::Result<String> {
    use k8s_openapi::api::core::v1::Node;
    use kube::{Api, Client, api::ListParams, config::KubeConfigOptions};

    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()?;
    let started = Instant::now();
    let mut last_error = String::from("no answer from the API server yet");
    while started.elapsed() < timeout {
        let attempt = runtime.block_on(async {
            let raw = kube::config::Kubeconfig::read_from(kubeconfig)?;
            let config = kube::Config::from_custom_kubeconfig(raw, &KubeConfigOptions::default()).await?;
            let client = Client::try_from(config)?;
            let nodes = Api::<Node>::all(client).list(&ListParams::default()).await?;
            let ready = nodes
                .items
                .iter()
                .filter(|n| {
                    n.status
                        .as_ref()
                        .and_then(|s| s.conditions.as_ref())
                        .is_some_and(|c| c.iter().any(|c| c.type_ == "Ready" && c.status == "True"))
                })
                .count();
            let version = nodes
                .items
                .first()
                .and_then(|n| n.status.as_ref())
                .and_then(|s| s.node_info.as_ref())
                .map(|i| i.kubelet_version.clone())
                .unwrap_or_default();
            anyhow::Ok((ready, nodes.items.len(), version))
        });
        match attempt {
            Ok((ready, total, version)) if ready > 0 => {
                let nodes = if total == 1 {
                    "1 node".to_owned()
                } else {
                    format!("{ready}/{total} nodes ready")
                };
                return Ok(format!("{nodes}, Kubernetes {version}"));
            }
            Ok(_) => "the node is not Ready yet".clone_into(&mut last_error),
            Err(e) => last_error = e.to_string(),
        }
        std::thread::sleep(Duration::from_secs(2));
    }
    bail!(
        "the cluster did not become ready within {}s: {last_error}",
        timeout.as_secs()
    )
}

/// `true` when the file was written now (a first install).
/// `true` when the file was written now (a first install). An existing file
/// is kept, except for its port when `port` differs from it.
fn write_config(
    ui: Ui,
    opts: &SetupOpts,
    kubeconfig: &Path,
    existing: Option<&str>,
    port: u16,
    hub: Option<&str>,
    book: &mut Book,
) -> anyhow::Result<bool> {
    let step = ui.step(format!("Writing {CONFIG_FILE}"));
    if let Some(text) = existing {
        book.claim(Kind::File, CONFIG_FILE, false)?;
        let mut updated = match config_port(text) {
            Some(old) if old != port => with_port(text, old, port),
            _ => text.to_owned(),
        };
        // A configuration setup wrote gains the local agent once.
        let agent =
            hub.filter(|_| book.journal().owns(Kind::File, CONFIG_FILE) && !has_section(text, "agent"));
        if let Some(hub) = agent {
            updated.push_str(&agent_section(hub));
        }
        let changed = updated != text;
        if changed {
            std::fs::write(CONFIG_FILE, &updated)?;
            step.done(format!(
                "port {port}{}, everything else kept",
                if agent.is_some() {
                    ", the cluster agent added"
                } else {
                    ""
                }
            ));
        } else {
            step.done("kept; edit it to change the public URL");
        }
        book.done(changed, format!("port {port}"))?;
        return Ok(changed);
    }
    let host = if opts.bind_local {
        "localhost".to_owned()
    } else {
        kuben_api::host::advertise_ip().map_or_else(|| "localhost".to_owned(), |ip| ip.to_string())
    };
    let bind_host = if opts.bind_local { "127.0.0.1" } else { "0.0.0.0" };
    let mut content = config_template(bind_host, port, &host, kubeconfig);
    if let Some(hub) = hub {
        content.push_str(&agent_section(hub));
    }
    std::fs::write(CONFIG_FILE, content)?;
    set_mode(Path::new(CONFIG_FILE), 0o644)?;
    book.claim(Kind::File, CONFIG_FILE, true)?;
    step.done(format!("port {port}"));
    book.done(true, format!("port {port}"))?;
    Ok(true)
}

/// The port AgentLink listens on for the agent in this server's k3s.
pub const AGENT_PORT: u16 = 7443;

/// The `[agent]` section of the local agent (M2.8): pods dial `hub`, an
/// address of this server.
fn agent_section(hub: &str) -> String {
    format!(
        "\n[agent]\n\
         # The cluster agent in this server's k3s enrolls from what Kuben publishes.\n\
         bind = \"0.0.0.0:{AGENT_PORT}\"\n\
         local = true\n\
         advertise = \"{hub}:{AGENT_PORT}\"\n\
         namespace = \"{}\"\n",
        platform::GATEWAY_NAMESPACE
    )
}

fn has_section(text: &str, name: &str) -> bool {
    text.lines().any(|line| line.trim() == format!("[{name}]"))
}

/// The lines of the `[server]` section of a config file.
fn server_lines(text: &str) -> impl Iterator<Item = &str> {
    let mut in_server = true;
    text.lines().filter(move |line| {
        let key = line.trim();
        if key.starts_with('[') {
            in_server = key == "[server]";
        }
        in_server
    })
}

/// The port of `bind` under `[server]` in a config file.
fn config_port(text: &str) -> Option<u16> {
    server_lines(text)
        .map(str::trim_start)
        .find_map(|line| line.strip_prefix("bind")?.trim_start().strip_prefix('='))?
        .trim()
        .trim_matches('"')
        .rsplit(':')
        .next()?
        .parse()
        .ok()
}

/// `text` with the port of `bind` and `public_url` changed from `old` to
/// `new`; every other line, comments included, stays as it is.
fn with_port(text: &str, old: u16, new: u16) -> String {
    let from = format!(":{old}\"");
    let to = format!(":{new}\"");
    let mut in_server = true;
    let mut out: String = text
        .lines()
        .map(|line| {
            let key = line.trim_start();
            if key.starts_with('[') {
                in_server = key.trim_end() == "[server]";
            }
            if in_server && (key.starts_with("bind") || key.starts_with("public_url")) {
                line.replace(&from, &to)
            } else {
                line.to_owned()
            }
        })
        .collect::<Vec<_>>()
        .join("\n");
    if text.ends_with('\n') {
        out.push('\n');
    }
    out
}

fn config_template(bind_host: &str, port: u16, public_host: &str, kubeconfig: &Path) -> String {
    format!(
        "# Written by `kuben setup`. Every kuben command reads this file; restart the\n\
         # service after a change: systemctl restart kuben\n\
         \n\
         [server]\n\
         bind = \"{bind_host}:{port}\"\n\
         metrics_bind = \"127.0.0.1:9090\"\n\
         # Set to the https:// address once Kuben sits behind TLS; the session cookie\n\
         # then becomes Secure on its own.\n\
         public_url = \"http://{public_host}:{port}\"\n\
         # The setup token and a generated first admin password go here.\n\
         state_dir = \"{STATE_DIR}\"\n\
         \n\
         [database]\n\
         # PostgreSQL on this server, over its Unix socket as the kuben user.\n\
         url = \"{LOCAL_DATABASE_URL}\"\n\
         \n\
         [kube]\n\
         kubeconfig = \"{}\"\n",
        kubeconfig.display()
    )
}

/// Install the unit and (re)start the service: only when its binary, unit
/// or configuration changed, or it is not running. A unit file someone else
/// wrote is never replaced.
fn start_service(ui: Ui, port: u16, changed: bool, book: &mut Book) -> anyhow::Result<()> {
    let step = ui.step("Starting kuben.service");
    let current = std::fs::read_to_string(UNIT_FILE).ok();
    if let Some(unit) = &current
        && !written_by_setup(unit)
        && !book.journal().owns(Kind::SystemdUnit, UNIT_NAME)
    {
        step.fail("not written by kuben setup");
        bail!(
            "{UNIT_FILE} exists and was not written by kuben setup; move it aside, then run kuben setup again"
        );
    }
    book.claim(Kind::SystemdUnit, UNIT_NAME, true)?;
    let desired = unit_template();
    let unit_changed = current.as_deref() != Some(desired.as_str());
    if unit_changed {
        std::fs::write(UNIT_FILE, &desired)?;
        run(&["systemctl", "daemon-reload"])?;
    }
    run(&["systemctl", "enable", "--quiet", "kuben"])?;
    let restart = changed || unit_changed || !service_active();
    if !restart {
        step.done("running, unchanged");
        return book.done(false, "running, unchanged");
    }
    run(&["systemctl", "restart", "kuben"])?;
    match wait_for_http(port, "/livez", Duration::from_mins(1)) {
        Ok(()) => {}
        Err(e) => {
            step.fail("not answering");
            ui.note(&service_log(15));
            bail!(
                "kuben.service did not come up: {e}; the lines above are its last log (journalctl -u kuben has more)"
            );
        }
    }
    if wait_for_http(port, "/readyz", Duration::from_secs(90)).is_ok() {
        step.done("running");
    } else {
        step.warn("running, not ready yet: still syncing with the cluster");
        ui.note(&service_problems(8));
    }
    book.done(true, "restarted")
}

/// The service's latest warnings and errors, for a step that did not finish.
fn service_problems(lines: usize) -> String {
    let log = Command::new("journalctl")
        .args(["-u", "kuben", "--no-pager", "-o", "cat", "-n", "400"])
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
        .unwrap_or_default();
    let problems: Vec<&str> = log
        .lines()
        .filter(|l| l.contains(" WARN ") || l.contains(" ERROR "))
        .collect();
    if problems.is_empty() {
        return "It carries on in the background; journalctl -u kuben -f shows how far it is.".to_owned();
    }
    let recent = &problems[problems.len().saturating_sub(lines)..];
    format!(
        "Its latest warnings (journalctl -u kuben has more):\n{}",
        recent.join("\n")
    )
}

/// The host firewall that is active, if any.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Firewall {
    Ufw,
    Firewalld,
}

fn active_firewall() -> Option<Firewall> {
    let output = |program: &str, arg: &str| {
        Command::new(program)
            .arg(arg)
            .output()
            .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
            .unwrap_or_default()
    };
    if output("ufw", "status").starts_with("Status: active") {
        Some(Firewall::Ufw)
    } else if output("firewall-cmd", "--state").trim() == "running" {
        Some(Firewall::Firewalld)
    } else {
        None
    }
}

/// k3s's default pod network: agent pods reach the hub on this server from
/// there.
pub const POD_NETWORK: &str = "10.42.0.0/16";

/// A TCP port setup opens, from anywhere or from one network only.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Opening<'a> {
    port: u16,
    source: Option<&'a str>,
}

impl Firewall {
    /// The journal's name of the rule, e.g. `ufw:3000/tcp` or
    /// `ufw:10.42.0.0/16:7443/tcp`.
    fn rule(self, opening: Opening<'_>) -> String {
        let tool = match self {
            Self::Ufw => "ufw",
            Self::Firewalld => "firewalld",
        };
        match opening.source {
            Some(source) => format!("{tool}:{source}:{}/tcp", opening.port),
            None => format!("{tool}:{}/tcp", opening.port),
        }
    }

    /// The firewall and opening a journal rule names.
    fn parse(rule: &str) -> Option<(Self, Opening<'_>)> {
        let mut parts = rule.split(':');
        let tool = match parts.next()? {
            "ufw" => Self::Ufw,
            "firewalld" => Self::Firewalld,
            _ => return None,
        };
        let rest: Vec<&str> = parts.collect();
        let (source, port) = match rest.as_slice() {
            [port] => (None, *port),
            [source, port] => (Some(*source), *port),
            _ => return None,
        };
        let port = port.strip_suffix("/tcp")?.parse().ok()?;
        Some((tool, Opening { port, source }))
    }

    fn rich_rule(opening: Opening<'_>) -> String {
        format!(
            "rule family=ipv4 source address={} port port={} protocol=tcp accept",
            opening.source.unwrap_or("0.0.0.0/0"),
            opening.port
        )
    }

    fn is_open(self, opening: Opening<'_>) -> bool {
        let port = format!("{}/tcp", opening.port);
        match (self, opening.source) {
            (Self::Ufw, _) => Command::new("ufw").arg("status").output().is_ok_and(|o| {
                String::from_utf8_lossy(&o.stdout).lines().any(|l| {
                    l.split_whitespace().next() == Some(port.as_str())
                        && opening.source.is_none_or(|s| l.contains(s))
                })
            }),
            (Self::Firewalld, None) => Command::new("firewall-cmd")
                .arg(format!("--query-port={port}"))
                .status()
                .is_ok_and(|s| s.success()),
            (Self::Firewalld, Some(_)) => Command::new("firewall-cmd")
                .arg(format!("--query-rich-rule={}", Self::rich_rule(opening)))
                .status()
                .is_ok_and(|s| s.success()),
        }
    }

    fn change(self, opening: Opening<'_>, open: bool) -> anyhow::Result<()> {
        let port = opening.port.to_string();
        match (self, opening.source) {
            (Self::Ufw, None) => {
                let rule = format!("{port}/tcp");
                if open {
                    run(&["ufw", "allow", &rule])
                } else {
                    run(&["ufw", "delete", "allow", &rule])
                }
            }
            (Self::Ufw, Some(source)) => {
                let mut args = vec!["ufw"];
                if !open {
                    args.push("delete");
                }
                args.extend([
                    "allow", "from", source, "to", "any", "port", &port, "proto", "tcp",
                ]);
                run(&args)
            }
            (Self::Firewalld, source) => {
                let arg = match (source, open) {
                    (None, true) => format!("--add-port={port}/tcp"),
                    (None, false) => format!("--remove-port={port}/tcp"),
                    (Some(_), true) => format!("--add-rich-rule={}", Self::rich_rule(opening)),
                    (Some(_), false) => format!("--remove-rich-rule={}", Self::rich_rule(opening)),
                };
                run(&["firewall-cmd", "--permanent", &arg]).and_then(|_| run(&["firewall-cmd", "--reload"]))
            }
        }
        .map(drop)
    }
}

/// Open the console's port, and with the local agent its port for the pod
/// network only. Each rule setup adds is recorded; one that was open is not.
fn open_firewall(ui: Ui, port: u16, agent: bool, book: &mut Book) -> anyhow::Result<()> {
    let label = format!("Opening port {port} in the firewall");
    let Some(firewall) = active_firewall() else {
        ui.done(&label, "no host firewall is active");
        return book.done(false, "no host firewall");
    };
    let mut openings = vec![Opening { port, source: None }];
    if agent {
        openings.push(Opening {
            port: AGENT_PORT,
            source: Some(POD_NETWORK),
        });
    }
    let step = ui.step(&label);
    let (mut changed, mut notes) = (false, Vec::new());
    for opening in openings {
        let rule = firewall.rule(opening);
        if firewall.is_open(opening) {
            book.claim(Kind::FirewallRule, &rule, false)?;
            notes.push(format!("{rule} already open"));
            continue;
        }
        // A firewall problem never stops the install; it is reported.
        match firewall.change(opening, true) {
            Ok(()) => {
                book.claim(Kind::FirewallRule, &rule, true)?;
                changed = true;
                notes.push(rule);
            }
            Err(e) => notes.push(format!("{rule} failed: {e}")),
        }
    }
    let detail = notes.join(", ");
    if detail.contains("failed") {
        step.warn(&detail);
    } else {
        step.done(&detail);
    }
    book.done(changed, detail)
}

fn announce(
    ui: Ui,
    cfg: &Config,
    uid: u32,
    gid: u32,
    fresh_config: bool,
    host: &Host,
    managed: Option<&platform::Wanted<'_>>,
) -> anyhow::Result<()> {
    let port = cfg.bind_port();
    let needed = wait_for_http(port, "/api/v1/setup", Duration::from_secs(10))
        .ok()
        .and_then(|()| http_get(port, "/api/v1/setup").ok())
        .is_some_and(|(_, body)| body.contains("\"needed\":true"));
    Ui::blank();
    let url = if needed {
        // `serve` wrote the token as the service user; issue one if it did not.
        let token = if kuben_api::setup::token_required(cfg) {
            let file = kuben_api::setup::token_file(cfg);
            let token = kuben_api::setup::current_or_new_token(cfg)?;
            chown(&file, uid, gid)?;
            Some(token)
        } else {
            None
        };
        let url = kuben_api::setup::setup_url(cfg, token.as_deref());
        ui.heading("Kuben is running. Finish the setup in your browser:");
        eprintln!("\n    {url}\n");
        if token.is_some() {
            ui.note("The link is valid for 30 minutes; print a new one with `kuben setup-token`.");
        }
        url
    } else {
        let url = kuben_api::host::console_url(cfg);
        ui.heading(if fresh_config {
            "Kuben is running:"
        } else {
            "Kuben is up to date and running:"
        });
        eprintln!("\n    {url}\n");
        url
    };
    if !cfg.bind_is_loopback() {
        ui.note("Behind a cloud firewall (Hetzner, AWS, GCP, …)? Allow the port there too.");
    }
    match managed {
        Some(platform::Wanted {
            domain: Some(domain),
            acme_email: Some(_),
            ..
        }) => ui.note(&format!(
            "Apps get HTTPS addresses under {domain}; point a wildcard DNS record (*.{domain}) at this server."
        )),
        Some(platform::Wanted { domain: Some(domain), .. }) => ui.note(&format!(
            "Apps get plain-HTTP addresses under {domain}; add --acme-email you@example.com for HTTPS."
        )),
        Some(_) => ui.note(
            "Apps get public addresses once a base domain is set: kuben setup --domain apps.example.com --acme-email you@example.com",
        ),
        None => ui.note("Apps get public HTTPS addresses once a Gateway and a base domain are configured; see the docs."),
    }
    ui.note(&format!("{DOCS} · {} / {}", host.os, host.arch));
    println!("{url}");
    Ok(())
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

pub fn status() -> anyhow::Result<()> {
    let ui = Ui::new();
    let version = format!("v{}", super::VERSION);
    if !Path::new(UNIT_FILE).exists() {
        ui.done(
            "kuben.service",
            "not installed on this machine (kuben setup installs it)",
        );
        return Ok(());
    }
    let cfg = Config::load()?;
    let port = cfg.bind_port();
    if service_active() {
        ui.done("kuben.service", format!("active, {version}"));
    } else {
        ui.fail("kuben.service", "not running (journalctl -u kuben)");
    }
    match http_get(port, "/api/v1/setup") {
        Ok((200, body)) if body.contains("\"needed\":true") => {
            ui.warn(
                "Admin account",
                "not created yet: kuben setup-token prints the link",
            );
        }
        Ok((200, _)) => ui.done("Admin account", "created"),
        Ok((code, _)) => ui.warn("API", format!("HTTP {code} on /api/v1/setup")),
        Err(e) => ui.fail("API", format!("no answer on port {port}: {e}")),
    }
    match cfg.kube.kubeconfig.as_deref().map(Path::new) {
        Some(path) => match wait_for_nodes(path, Duration::from_secs(5)) {
            Ok(nodes) => ui.done("Cluster", nodes),
            Err(e) => ui.fail("Cluster", e.to_string()),
        },
        None => ui.warn("Cluster", "no kubeconfig configured"),
    }
    ui.done("Console", kuben_api::host::console_url(&cfg));
    Ok(())
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

pub fn uninstall(opts: &UninstallOpts) -> anyhow::Result<()> {
    let ui = Ui::new();
    if !is_root() {
        bail!("run it as root: sudo kuben uninstall");
    }
    let mut book = Book::open(Path::new(STATE_DIR), None)?;
    let owned = Owned::of(book.journal());
    if !opts.yes {
        let what = if opts.purge {
            format!(
                "Remove Kuben, its data in {STATE_DIR}, {CONFIG_DIR}{}?",
                if owned.k3s {
                    " and the k3s it installed"
                } else {
                    ""
                }
            )
        } else {
            "Stop and remove kuben.service? (data and configuration stay)".to_owned()
        };
        match ui.confirm(&what) {
            Some(true) => {}
            Some(false) => bail!("aborted"),
            None => bail!("no terminal to confirm on; pass --yes"),
        }
    }
    let step = ui.step("Stopping kuben.service");
    if Path::new(UNIT_FILE).exists() && owned.unit {
        run(&["systemctl", "disable", "--now", "--quiet", "kuben"]).ok();
        std::fs::remove_file(UNIT_FILE)?;
        run(&["systemctl", "daemon-reload"])?;
        book.release(Kind::SystemdUnit, UNIT_NAME)?;
        step.done("removed");
    } else if Path::new(UNIT_FILE).exists() {
        step.warn("not written by kuben setup; left in place");
    } else {
        step.done("not installed");
    }
    if !opts.purge {
        ui.note(&format!(
            "Kept: the PostgreSQL database kuben, {STATE_DIR}, {CONFIG_FILE}, {BIN}. `kuben uninstall --purge` \
             removes what kuben setup created (PostgreSQL itself stays)."
        ));
        if owned.k3s {
            ui.note("k3s stays as well; --purge removes it too.");
        }
        return Ok(());
    }
    let kubeconfig = Path::new(STATE_DIR).join("kubeconfig");
    if !owned.k3s && kubeconfig.exists() {
        let kept = platform::purge_objects(ui, &kubeconfig, book.journal());
        if !kept.is_empty() {
            ui.note(&format!(
                "Kept in the cluster, other workloads may use them: {}.",
                kept.join(", ")
            ));
        }
    }
    purge(ui, &owned);
    Ok(())
}

/// What `kuben uninstall --purge` may remove: what the journal says setup
/// created. A server set up before the journal existed keeps the old rule:
/// everything but a database of the operator's own, and k3s only with its
/// marker.
#[derive(Debug)]
struct Owned {
    unit: bool,
    state: bool,
    config: bool,
    database: bool,
    role: bool,
    k3s: bool,
    binary: bool,
    user: bool,
    firewall: Vec<String>,
}

impl Owned {
    fn of(journal: &journal::Journal) -> Self {
        let local = data_home(std::fs::read_to_string(CONFIG_FILE).ok().as_deref()) == DataHome::Local;
        if journal.resources.is_empty() {
            return Self {
                unit: true,
                state: true,
                config: true,
                database: local,
                role: local,
                k3s: Path::new(K3S_MARKER).exists(),
                binary: true,
                user: false,
                firewall: Vec::new(),
            };
        }
        Self {
            unit: journal.owns(Kind::SystemdUnit, UNIT_NAME),
            state: journal.owns(Kind::Directory, STATE_DIR),
            config: journal.owns(Kind::File, CONFIG_FILE),
            // Never an external database, whatever the journal says.
            database: local && journal.owns(Kind::PostgresDatabase, USER),
            role: local && journal.owns(Kind::PostgresRole, USER),
            k3s: journal.owns(Kind::Cluster, "k3s"),
            binary: journal.owns(Kind::File, BIN),
            user: journal.owns(Kind::SystemUser, USER),
            firewall: journal
                .resources
                .iter()
                .filter(|r| r.kind == Kind::FirewallRule && r.owner == journal::Owner::Kuben)
                .map(|r| r.name.clone())
                .collect(),
        }
    }
}

fn purge(ui: Ui, owned: &Owned) {
    let step = ui.step("Deleting data and configuration");
    let mut removed = Vec::new();
    let mut kept = Vec::new();
    for (path, ours) in [(CONFIG_FILE, owned.config), (STATE_DIR, owned.state)] {
        let gone = if !ours {
            false
        } else if Path::new(path).is_dir() {
            std::fs::remove_dir_all(path).is_ok()
        } else {
            std::fs::remove_file(path).is_ok()
        };
        if gone {
            removed.push(path);
        } else if Path::new(path).exists() {
            kept.push(path);
        }
    }
    // The configuration directory goes only when setup made it and nothing
    // else lives there.
    std::fs::remove_dir(CONFIG_DIR).ok();
    step.done(if kept.is_empty() {
        removed.join(", ")
    } else {
        format!(
            "{}; kept (not created by kuben setup): {}",
            removed.join(", "),
            kept.join(", ")
        )
    });
    if (owned.database || owned.role) && which("psql").is_some() {
        let step = ui.step("Dropping what kuben setup created in PostgreSQL");
        let mut statements = Vec::new();
        if owned.database {
            statements.push("DROP DATABASE IF EXISTS kuben WITH (FORCE)");
        }
        if owned.role {
            statements.push("DROP ROLE IF EXISTS kuben");
        }
        match statements
            .iter()
            .try_for_each(|sql| psql_as_postgres(sql).map(drop))
        {
            Ok(()) => step.done("PostgreSQL itself stays installed"),
            Err(e) => step.warn(e.to_string()),
        }
    }
    for rule in &owned.firewall {
        let closed = Firewall::parse(rule).map(|(firewall, opening)| firewall.change(opening, false));
        match closed {
            Some(Ok(())) => ui.done("Closing the firewall port", rule),
            Some(Err(e)) => ui.warn("Closing the firewall port", format!("{rule}: {e}")),
            None => {}
        }
    }
    if owned.k3s && Path::new(K3S_UNINSTALL).exists() {
        let step = ui.step("Uninstalling k3s");
        ui.command(K3S_UNINSTALL);
        match Command::new(K3S_UNINSTALL).output() {
            Ok(out) if out.status.success() => step.done(""),
            Ok(out) => {
                step.warn("k3s-uninstall.sh failed");
                ui.note(&tail(&out.stdout, &out.stderr, 8));
            }
            Err(e) => step.warn(e.to_string()),
        }
    }
    if owned.user && user_ids(USER).is_some() {
        let step = ui.step("Removing the kuben system user");
        match run(&["userdel", USER]) {
            Ok(_) => step.done(USER),
            Err(e) => step.warn(e.to_string()),
        }
    }
    if owned.binary {
        let step = ui.step("Removing the binary");
        std::fs::remove_file(BIN).ok();
        step.done(BIN);
    }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

fn unit_template() -> String {
    format!(
        "{UNIT_MARKER}; `kuben uninstall` removes it.\n\
         [Unit]\n\
         Description=Kuben\n\
         Documentation={DOCS}\n\
         After=network-online.target k3s.service postgresql.service\n\
         Wants=network-online.target postgresql.service\n\
         \n\
         [Service]\n\
         ExecStart={BIN} serve\n\
         User={USER}\n\
         Group={USER}\n\
         StateDirectory={USER}\n\
         Environment=KUBEN_TELEMETRY__LOG_FORMAT=pretty\n\
         Restart=always\n\
         RestartSec=2\n\
         NoNewPrivileges=true\n\
         ProtectSystem=full\n\
         PrivateTmp=true\n\
         \n\
         [Install]\n\
         WantedBy=multi-user.target\n"
    )
}

fn is_root() -> bool {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt as _;
        std::fs::metadata("/proc/self").is_ok_and(|m| m.uid() == 0)
    }
    #[cfg(not(unix))]
    {
        false
    }
}

fn os_name() -> Option<String> {
    let release = std::fs::read_to_string("/etc/os-release").ok()?;
    parse_os_release(&release)
}

fn parse_os_release(release: &str) -> Option<String> {
    release
        .lines()
        .find_map(|line| line.strip_prefix("PRETTY_NAME="))
        .map(|name| name.trim_matches('"').to_owned())
}

fn mem_total_gb() -> Option<f64> {
    let meminfo = std::fs::read_to_string("/proc/meminfo").ok()?;
    parse_mem_total_gb(&meminfo)
}

fn parse_mem_total_gb(meminfo: &str) -> Option<f64> {
    let kb: f64 = meminfo
        .lines()
        .find_map(|line| line.strip_prefix("MemTotal:"))?
        .split_whitespace()
        .next()?
        .parse()
        .ok()?;
    Some(kb / 1024.0 / 1024.0)
}

fn in_container() -> bool {
    if Path::new("/.dockerenv").exists() || std::env::var_os("container").is_some() {
        return true;
    }
    std::fs::read_to_string("/proc/1/cgroup")
        .is_ok_and(|c| c.contains("docker") || c.contains("lxc") || c.contains("containerd"))
}

/// The kubeconfig an admin on this machine already uses, if any.
fn existing_kubeconfig() -> Option<PathBuf> {
    if let Some(paths) = std::env::var_os("KUBECONFIG")
        && let Some(first) = std::env::split_paths(&paths).find(|p| p.is_file())
    {
        return Some(first);
    }
    let home = std::env::var_os("HOME").map(PathBuf::from)?;
    let default = home.join(".kube").join("config");
    default.is_file().then_some(default)
}

fn user_ids(name: &str) -> Option<(u32, u32)> {
    let out = Command::new("getent").args(["passwd", name]).output().ok()?;
    if !out.status.success() {
        return None;
    }
    parse_passwd_ids(&String::from_utf8_lossy(&out.stdout))
}

fn parse_passwd_ids(line: &str) -> Option<(u32, u32)> {
    let mut fields = line.trim().split(':');
    let uid = fields.nth(2)?.parse().ok()?;
    let gid = fields.next()?.parse().ok()?;
    Some((uid, gid))
}

fn same_file(a: &Path, b: &Path) -> bool {
    match (std::fs::canonicalize(a), std::fs::canonicalize(b)) {
        (Ok(a), Ok(b)) => a == b,
        _ => false,
    }
}

fn set_mode(path: &Path, mode: u32) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
    }
    #[cfg(not(unix))]
    {
        let _ = (path, mode);
        Ok(())
    }
}

fn chown(path: &Path, uid: u32, gid: u32) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        std::os::unix::fs::chown(path, Some(uid), Some(gid))
    }
    #[cfg(not(unix))]
    {
        let _ = (path, uid, gid);
        Ok(())
    }
}

fn which(program: &str) -> Option<PathBuf> {
    let paths = std::env::var_os("PATH")?;
    std::env::split_paths(&paths)
        .map(|dir| dir.join(program))
        .find(|candidate| candidate.is_file())
}

fn service_active() -> bool {
    service_is_active("kuben")
}

fn service_is_active(unit: &str) -> bool {
    Command::new("systemctl")
        .args(["is-active", "--quiet", unit])
        .status()
        .is_ok_and(|s| s.success())
}

fn service_log(lines: usize) -> String {
    Command::new("journalctl")
        .args(["-u", "kuben", "--no-pager", "-n", &lines.to_string()])
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
        .unwrap_or_default()
}

/// What listens on `port`, from `ss`, when it can tell.
fn port_owner(port: u16) -> Option<String> {
    let out = Command::new("ss")
        .args(["-ltnp", &format!("sport = :{port}")])
        .output()
        .ok()?;
    let text = String::from_utf8_lossy(&out.stdout);
    let line = text.lines().nth(1)?;
    let process = line
        .split("users:(")
        .nth(1)?
        .split(',')
        .next()?
        .trim_matches(|c| c == '(' || c == '"');
    Some(process.to_owned())
}

/// Run a command; on failure, its last lines are the error.
fn run(command: &[&str]) -> anyhow::Result<String> {
    run_env(command, &[])
}

/// [`run`] with extra environment variables.
fn run_env(command: &[&str], env: &[(&str, &str)]) -> anyhow::Result<String> {
    let (program, args) = command.split_first().expect("a program");
    let output = Command::new(program)
        .args(args)
        .envs(env.iter().copied())
        .output()
        .with_context(|| format!("running {program}"))?;
    if !output.status.success() {
        bail!(
            "{} failed: {}",
            command.join(" "),
            tail(&output.stdout, &output.stderr, 6).trim()
        );
    }
    Ok(String::from_utf8_lossy(&output.stdout).into_owned())
}

fn tail(stdout: &[u8], stderr: &[u8], lines: usize) -> String {
    let mut text = String::from_utf8_lossy(stdout).into_owned();
    text.push_str(&String::from_utf8_lossy(stderr));
    let all: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    all.iter()
        .rev()
        .take(lines)
        .rev()
        .copied()
        .collect::<Vec<_>>()
        .join("\n")
}

/// A plain HTTP/1.1 GET on localhost, without an HTTP client dependency.
fn http_get(port: u16, path: &str) -> std::io::Result<(u16, String)> {
    let addr = std::net::SocketAddr::from(([127, 0, 0, 1], port));
    let mut stream = TcpStream::connect_timeout(&addr, Duration::from_secs(2))?;
    stream.set_read_timeout(Some(Duration::from_secs(5)))?;
    stream.set_write_timeout(Some(Duration::from_secs(5)))?;
    write!(
        stream,
        "GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
    )?;
    let mut raw = String::new();
    stream.read_to_string(&mut raw)?;
    parse_http_response(&raw).ok_or_else(|| std::io::Error::other("malformed HTTP response"))
}

fn parse_http_response(raw: &str) -> Option<(u16, String)> {
    let status: u16 = raw.lines().next()?.split_whitespace().nth(1)?.parse().ok()?;
    let body = raw
        .split_once("\r\n\r\n")
        .map(|(_, b)| b.to_owned())
        .unwrap_or_default();
    Some((status, body))
}

fn wait_for_http(port: u16, path: &str, timeout: Duration) -> anyhow::Result<()> {
    let started = Instant::now();
    let mut last = String::new();
    while started.elapsed() < timeout {
        match http_get(port, path) {
            Ok((200, _)) => return Ok(()),
            Ok((code, _)) => last = format!("HTTP {code}"),
            Err(e) => last = e.to_string(),
        }
        std::thread::sleep(Duration::from_millis(500));
    }
    bail!("{path} did not answer 200 within {}s ({last})", timeout.as_secs())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_host_facts() {
        assert_eq!(
            parse_os_release("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n").as_deref(),
            Some("Ubuntu 24.04.4 LTS")
        );
        let gb = parse_mem_total_gb("MemTotal:        4015036 kB\nMemFree: 1 kB\n").expect("mem");
        assert!((gb - 3.83).abs() < 0.01, "{gb}");
        assert_eq!(
            parse_passwd_ids("kuben:x:998:997::/var/lib/kuben:/usr/sbin/nologin\n"),
            Some((998, 997))
        );
        assert_eq!(parse_passwd_ids(""), None);
    }

    #[test]
    fn config_and_unit_templates_are_valid_toml_and_ini() {
        use figment::{
            Figment,
            providers::{Format as _, Serialized, Toml},
        };
        let toml = config_template(
            "0.0.0.0",
            3000,
            "203.0.113.7",
            Path::new("/var/lib/kuben/kubeconfig"),
        );
        let cfg: Config = Figment::from(Serialized::defaults(Config::default()))
            .merge(Toml::string(&toml))
            .extract()
            .expect("the template is a valid Kuben config");
        assert_eq!(cfg.server.bind, "0.0.0.0:3000");
        assert_eq!(cfg.server.public_url.as_deref(), Some("http://203.0.113.7:3000"));
        assert_eq!(cfg.kube.kubeconfig.as_deref(), Some("/var/lib/kuben/kubeconfig"));
        assert_eq!(cfg.database.url, LOCAL_DATABASE_URL);
        assert_eq!(cfg.state_dir(), Path::new(STATE_DIR));
        assert!(!cfg.cookie_secure(), "plain http until public_url is https");
        let unit = unit_template();
        assert!(unit.contains("ExecStart=/usr/local/bin/kuben serve"));
        assert!(unit.contains("User=kuben"));
        assert!(unit.contains("StateDirectory=kuben"));
        assert!(unit.contains("After=network-online.target k3s.service postgresql.service"));
        assert!(unit.contains("Wants=network-online.target postgresql.service"));
    }

    #[test]
    fn knows_where_the_data_lives_and_how_to_install_postgresql() {
        assert_eq!(data_home(None), DataHome::Local);
        let written = config_template(
            "0.0.0.0",
            3000,
            "localhost",
            Path::new("/var/lib/kuben/kubeconfig"),
        );
        assert_eq!(data_home(Some(&written)), DataHome::Local);
        assert_eq!(
            data_home(Some("[database]\nurl = \"sqlite:///var/lib/kuben/kuben.db\"\n")),
            DataHome::Sqlite
        );
        assert_eq!(
            data_home(Some(
                "[server]\npublic_url = \"http://x:3000\"\n[database]\nurl = \"postgres://kuben:x@db/kuben\"\n"
            )),
            DataHome::External
        );
        assert_eq!(packages_of("ID=ubuntu\nID_LIKE=debian\n"), Some(Packages::Apt));
        assert_eq!(
            packages_of("ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n"),
            Some(Packages::Dnf)
        );
        assert_eq!(packages_of("ID=fedora\n"), Some(Packages::Dnf));
        assert_eq!(
            packages_of("ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n"),
            Some(Packages::Zypper)
        );
        assert_eq!(packages_of("ID=arch\n"), None);
    }

    #[test]
    fn parses_port_answers() {
        assert_eq!(parse_port(" 3001 "), Some(3001));
        assert_eq!(parse_port("0"), None);
        assert_eq!(parse_port("70000"), None);
        assert_eq!(parse_port("three"), None);
    }

    #[test]
    fn reads_and_changes_the_configured_port() {
        let text = config_template(
            "0.0.0.0",
            3000,
            "203.0.113.7",
            Path::new("/var/lib/kuben/kubeconfig"),
        );
        assert_eq!(config_port(&text), Some(3000), "bind, not metrics_bind");
        let moved = with_port(&text, 3000, 3001);
        assert_eq!(config_port(&moved), Some(3001));
        assert!(
            moved.contains("public_url = \"http://203.0.113.7:3001\""),
            "{moved}"
        );
        assert!(
            moved.contains("metrics_bind = \"127.0.0.1:9090\""),
            "other ports stay"
        );
        assert!(moved.contains("# Written by `kuben setup`"), "comments stay");
        assert!(moved.ends_with('\n'));
        assert_eq!(config_port("[server]\nbind=\"127.0.0.1:8080\"\n"), Some(8080));
        assert_eq!(config_port("[server]\n"), None);
    }

    #[test]
    fn parses_http_responses() {
        let raw = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"needed\":true}";
        assert_eq!(parse_http_response(raw), Some((200, "{\"needed\":true}".into())));
        assert_eq!(parse_http_response("garbage"), None);
    }

    #[test]
    fn tail_keeps_the_last_non_empty_lines() {
        assert_eq!(tail(b"a\n\nb\nc\n", b"d\n", 2), "c\nd");
    }

    #[test]
    fn the_agent_section_leaves_the_server_port_alone() {
        let mut config = config_template(
            "0.0.0.0",
            3000,
            "203.0.113.7",
            Path::new("/var/lib/kuben/kubeconfig"),
        );
        assert!(!has_section(&config, "agent"));
        config.push_str(&agent_section("203.0.113.7"));
        assert!(has_section(&config, "agent"));
        assert!(config.contains("advertise = \"203.0.113.7:7443\""));
        assert_eq!(
            config_port(&config),
            Some(3000),
            "the agent's bind is not the server's"
        );
        let moved = with_port(&config, 3000, 7443);
        assert_eq!(config_port(&moved), Some(7443));
        assert!(
            moved.contains("bind = \"0.0.0.0:7443\"\nlocal = true"),
            "the agent section is untouched"
        );
        let back = with_port(&moved, 7443, 3000);
        assert_eq!(back, config, "only [server] lines moved");
    }

    #[test]
    fn firewall_rules_name_their_source_and_read_back() {
        let open = Opening {
            port: 3000,
            source: None,
        };
        let pods = Opening {
            port: AGENT_PORT,
            source: Some(POD_NETWORK),
        };
        assert_eq!(Firewall::Ufw.rule(open), "ufw:3000/tcp");
        assert_eq!(Firewall::Firewalld.rule(pods), "firewalld:10.42.0.0/16:7443/tcp");
        for (firewall, opening) in [(Firewall::Ufw, open), (Firewall::Firewalld, pods)] {
            assert_eq!(
                Firewall::parse(&firewall.rule(opening)),
                Some((firewall, opening))
            );
        }
        assert_eq!(Firewall::parse("iptables:22/tcp"), None);
        assert!(Firewall::rich_rule(pods).contains("source address=10.42.0.0/16 port port=7443"));
    }
}

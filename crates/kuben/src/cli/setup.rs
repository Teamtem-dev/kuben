//! `kuben setup`: this machine becomes a Kuben server. `kuben status` and
//! `kuben uninstall` share its pieces.
//!
//! Every step checks before it acts, so the same command installs, upgrades,
//! repairs and resumes an interrupted run: an existing k3s or kubeconfig is
//! used instead of installing k3s, an existing config file is kept, and an
//! existing service is restarted on the new binary. Linux with systemd, as
//! root; everything else gets a clear message and no changes.
//!
//! What it leaves behind: the binary at `/usr/local/bin/kuben`, the system
//! user `kuben`, `/etc/kuben/config.toml`, `/var/lib/kuben` (database,
//! kubeconfig copy, setup token), `kuben.service`, and k3s when it installed
//! it (marked, so `uninstall --purge` knows to remove it).

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

use super::ui::Ui;

pub const BIN: &str = "/usr/local/bin/kuben";
pub const USER: &str = "kuben";
pub const STATE_DIR: &str = "/var/lib/kuben";
pub const CONFIG_DIR: &str = "/etc/kuben";
pub const CONFIG_FILE: &str = "/etc/kuben/config.toml";
pub const UNIT_FILE: &str = "/etc/systemd/system/kuben.service";
pub const K3S_KUBECONFIG: &str = "/etc/rancher/k3s/k3s.yaml";
pub const K3S_INSTALLER: &str = "https://get.k3s.io";
pub const K3S_UNINSTALL: &str = "/usr/local/bin/k3s-uninstall.sh";
/// Present when `kuben setup` installed k3s itself.
pub const K3S_MARKER: &str = "/var/lib/kuben/.k3s-installed-by-kuben";
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
    report_download(ui);
    let host = preflight(ui, opts)?;
    install_binary(ui)?;
    let (uid, gid) = ensure_user(ui)?;
    let kubeconfig = ensure_cluster(ui, opts, uid, gid)?;
    let configured = std::fs::read_to_string(CONFIG_FILE).ok();
    let port = choose_port(
        ui,
        opts.port,
        configured.as_deref().and_then(config_port),
        opts.yes,
    )?;
    let fresh_config = write_config(ui, opts, &kubeconfig, configured.as_deref(), port)?;
    let cfg = Config::load().context("reading /etc/kuben/config.toml")?;
    let port = cfg.bind_port();
    start_service(ui, port)?;
    open_firewall(ui, port);
    warn_web_ports(ui);
    announce(ui, &cfg, uid, gid, fresh_config, &host)?;
    Ok(())
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

fn install_binary(ui: Ui) -> anyhow::Result<()> {
    let me = std::env::current_exe().context("locating the running binary")?;
    let target = Path::new(BIN);
    if same_file(&me, target) {
        return Ok(()); // the installer put it there; nothing to say
    }
    let step = ui.step("Installing the kuben binary");
    let version = format!("v{}", super::VERSION);
    if let Some(dir) = target.parent() {
        std::fs::create_dir_all(dir)?;
    }
    // A running service keeps its old inode open; replace, do not overwrite.
    let staged = target.with_extension("new");
    std::fs::copy(&me, &staged)
        .with_context(|| format!("copying {} to {}", me.display(), staged.display()))?;
    set_mode(&staged, 0o755)?;
    std::fs::rename(&staged, target)?;
    step.done(format!("{version} → {BIN}"));
    Ok(())
}

fn ensure_user(ui: Ui) -> anyhow::Result<(u32, u32)> {
    let step = ui.step("Creating the kuben system user");
    let ids = if let Some(ids) = user_ids(USER) {
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
    std::fs::create_dir_all(STATE_DIR)?;
    set_mode(Path::new(STATE_DIR), 0o750)?;
    chown(Path::new(STATE_DIR), ids.0, ids.1)?;
    std::fs::create_dir_all(CONFIG_DIR)?;
    Ok(ids)
}

/// The kubeconfig Kuben will use: a copy in the state directory, owned by
/// the service user (k3s writes its own for root only).
fn ensure_cluster(ui: Ui, opts: &SetupOpts, uid: u32, gid: u32) -> anyhow::Result<PathBuf> {
    let source = if let Some(path) = &opts.kubeconfig {
        let step = ui.step("Checking the cluster");
        std::fs::metadata(path).with_context(|| format!("cannot read {}", path.display()))?;
        step.done(format!("kubeconfig {}", path.display()));
        path.clone()
    } else if Path::new(K3S_KUBECONFIG).exists() {
        ui.done("Installing k3s", "already installed");
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
        install_k3s(ui)?;
        PathBuf::from(K3S_KUBECONFIG)
    };

    let step = ui.step("Waiting for the cluster");
    let nodes = wait_for_nodes(&source, Duration::from_mins(3))?;
    step.done(nodes);

    let copy = Path::new(STATE_DIR).join("kubeconfig");
    let content = std::fs::read(&source).with_context(|| format!("reading {}", source.display()))?;
    std::fs::write(&copy, content)?;
    set_mode(&copy, 0o600)?;
    chown(&copy, uid, gid)?;
    Ok(copy)
}

fn install_k3s(ui: Ui) -> anyhow::Result<()> {
    let step = ui.step("Installing k3s");
    if which("curl").is_none() {
        step.fail("curl is missing");
        bail!("install curl first (apt-get install -y curl), then run kuben setup again");
    }
    ui.command(&format!("curl -sfL {K3S_INSTALLER} | sh -"));
    // k3s's installer verifies its download against the release's checksum.
    let output = Command::new("sh")
        .arg("-c")
        .arg(format!("curl -sfL {K3S_INSTALLER} | sh -"))
        .output()
        .context("running the k3s installer")?;
    if !output.status.success() {
        step.fail("the k3s installer failed");
        ui.note(&tail(&output.stdout, &output.stderr, 12));
        bail!("k3s did not install; the lines above are its last output (journalctl -u k3s has more)");
    }
    std::fs::write(K3S_MARKER, "installed by kuben setup\n").ok();
    let version = Command::new("k3s")
        .arg("--version")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .and_then(|s| s.split_whitespace().nth(2).map(str::to_owned));
    step.done(version.unwrap_or_default());
    Ok(())
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
) -> anyhow::Result<bool> {
    let step = ui.step(format!("Writing {CONFIG_FILE}"));
    if let Some(text) = existing {
        match config_port(text) {
            Some(old) if old != port => {
                std::fs::write(CONFIG_FILE, with_port(text, old, port))?;
                step.done(format!("port {old} → {port}, everything else kept"));
            }
            _ => step.done("kept; edit it to change the public URL"),
        }
        return Ok(false);
    }
    let host = if opts.bind_local {
        "localhost".to_owned()
    } else {
        kuben_api::host::advertise_ip().map_or_else(|| "localhost".to_owned(), |ip| ip.to_string())
    };
    let bind_host = if opts.bind_local { "127.0.0.1" } else { "0.0.0.0" };
    let content = config_template(bind_host, port, &host, kubeconfig);
    std::fs::write(CONFIG_FILE, content)?;
    set_mode(Path::new(CONFIG_FILE), 0o644)?;
    step.done(format!("port {port}"));
    Ok(true)
}

/// The port of `bind` under `[server]` in a config file.
fn config_port(text: &str) -> Option<u16> {
    text.lines()
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
    let mut out: String = text
        .lines()
        .map(|line| {
            let key = line.trim_start();
            if key.starts_with("bind") || key.starts_with("public_url") {
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
         \n\
         [database]\n\
         url = \"sqlite://{STATE_DIR}/kuben.db\"\n\
         \n\
         [kube]\n\
         kubeconfig = \"{}\"\n",
        kubeconfig.display()
    )
}

fn start_service(ui: Ui, port: u16) -> anyhow::Result<()> {
    let step = ui.step("Starting kuben.service");
    std::fs::write(UNIT_FILE, unit_template())?;
    run(&["systemctl", "daemon-reload"])?;
    run(&["systemctl", "enable", "--quiet", "kuben"])?;
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
    let ready = wait_for_http(port, "/readyz", Duration::from_secs(90)).is_ok();
    step.done(if ready {
        "running"
    } else {
        "running, still syncing with the cluster"
    });
    Ok(())
}

fn open_firewall(ui: Ui, port: u16) {
    let label = format!("Opening port {port} in the firewall");
    let ufw = Command::new("ufw").arg("status").output();
    if let Ok(out) = ufw
        && String::from_utf8_lossy(&out.stdout).starts_with("Status: active")
    {
        let step = ui.step(&label);
        match run(&["ufw", "allow", &format!("{port}/tcp")]) {
            Ok(_) => step.done("ufw"),
            Err(e) => step.warn(format!("ufw failed: {e}")),
        }
        return;
    }
    let firewalld = Command::new("firewall-cmd").arg("--state").output();
    if let Ok(out) = firewalld
        && String::from_utf8_lossy(&out.stdout).trim() == "running"
    {
        let step = ui.step(&label);
        let added = run(&["firewall-cmd", "--permanent", &format!("--add-port={port}/tcp")])
            .and_then(|_| run(&["firewall-cmd", "--reload"]));
        match added {
            Ok(_) => step.done("firewalld"),
            Err(e) => step.warn(format!("firewalld failed: {e}")),
        }
        return;
    }
    ui.done(&label, "no host firewall is active");
}

fn announce(ui: Ui, cfg: &Config, uid: u32, gid: u32, fresh_config: bool, host: &Host) -> anyhow::Result<()> {
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
    ui.note("Apps get public HTTPS addresses once a Gateway and a base domain are configured; see the docs.");
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
    let k3s_ours = Path::new(K3S_MARKER).exists();
    if !opts.yes {
        let what = if opts.purge {
            format!(
                "Remove Kuben, its data in {STATE_DIR}, {CONFIG_DIR}{}?",
                if k3s_ours { " and the k3s it installed" } else { "" }
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
    if Path::new(UNIT_FILE).exists() {
        run(&["systemctl", "disable", "--now", "--quiet", "kuben"]).ok();
        std::fs::remove_file(UNIT_FILE)?;
        run(&["systemctl", "daemon-reload"])?;
        step.done("removed");
    } else {
        step.done("not installed");
    }
    if opts.purge {
        let step = ui.step("Deleting data and configuration");
        std::fs::remove_dir_all(STATE_DIR).ok();
        std::fs::remove_dir_all(CONFIG_DIR).ok();
        step.done(format!("{STATE_DIR}, {CONFIG_DIR}"));
        if k3s_ours && Path::new(K3S_UNINSTALL).exists() {
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
        let step = ui.step("Removing the binary");
        std::fs::remove_file(BIN).ok();
        step.done(BIN);
    } else {
        ui.note(&format!(
            "Kept: {STATE_DIR} (database), {CONFIG_FILE}, {BIN}. `kuben uninstall --purge` removes them."
        ));
        if k3s_ours {
            ui.note("k3s stays as well; --purge removes it too.");
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

fn unit_template() -> String {
    format!(
        "[Unit]\n\
         Description=Kuben\n\
         Documentation={DOCS}\n\
         After=network-online.target k3s.service\n\
         Wants=network-online.target\n\
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
    Command::new("systemctl")
        .args(["is-active", "--quiet", "kuben"])
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
    let (program, args) = command.split_first().expect("a program");
    let output = Command::new(program)
        .args(args)
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
        assert_eq!(cfg.database.url, "sqlite:///var/lib/kuben/kuben.db");
        assert!(!cfg.cookie_secure(), "plain http until public_url is https");
        let unit = unit_template();
        assert!(unit.contains("ExecStart=/usr/local/bin/kuben serve"));
        assert!(unit.contains("User=kuben"));
        assert!(unit.contains("StateDirectory=kuben"));
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
}

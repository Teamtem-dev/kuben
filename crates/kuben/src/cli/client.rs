//! The client commands (M2.14): `kuben login`, `apps`, `deploy`, `status
//! <app>`, `logs`, `rollback`, against a Kuben server with an API token.
//!
//! The server and token come from the context file `kuben login` writes
//! (owner-only), or from `KUBEN_URL` and `KUBEN_TOKEN` (CI).

use std::{
    collections::BTreeMap,
    io::{BufRead as _, IsTerminal as _, Write as _},
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::{Context as _, bail};
use clap::Args;
use futures::StreamExt as _;
use kuben_api::{
    client::{ApiClient, AppPath, FollowEvent, LogOptions},
    oci::{ImageResolver as _, RegistryResolver},
    routes::apps::{
        deployments::{DeployReason, StartDeploymentRequest},
        doctor::DoctorReport,
        releases::ReleaseDto,
    },
};
use kuben_core::ops::RunPhase;
use serde::{Deserialize, Serialize};

/// Where the server and project are chosen, on every client command.
#[derive(Debug, Default, Args)]
pub struct Target {
    /// A context written by `kuben login` (default: the current one).
    #[arg(long, env = "KUBEN_CONTEXT")]
    pub context: Option<String>,
    /// Project of apps named without one.
    #[arg(long, env = "KUBEN_PROJECT")]
    pub project: Option<String>,
    /// Environment of apps named without one.
    #[arg(long, env = "KUBEN_ENVIRONMENT")]
    pub environment: Option<String>,
}

#[derive(Debug, Args)]
pub struct LoginOpts {
    /// The server, e.g. https://kuben.example.com
    pub url: String,
    /// An API token (Account → API tokens); without it, it is read from
    /// standard input.
    #[arg(long, env = "KUBEN_TOKEN", hide_env_values = true)]
    pub token: Option<String>,
    /// Name of the context (default: the server's host).
    #[arg(long)]
    pub name: Option<String>,
    #[command(flatten)]
    pub target: Target,
}

#[derive(Debug, Args)]
pub struct AppsOpts {
    #[command(flatten)]
    pub target: Target,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
}

#[derive(Debug, Args)]
pub struct DeployOpts {
    /// The app: project/environment/app, or app with --project and --environment.
    pub app: String,
    /// The image; a tag is resolved to its digest first.
    #[arg(long)]
    pub image: String,
    /// Retrying with the same key returns the same deployment (default: a new key).
    #[arg(long)]
    pub idempotency_key: Option<String>,
    /// Return once the deployment is accepted.
    #[arg(long)]
    pub no_wait: bool,
    /// How long to wait for the deployment, seconds.
    #[arg(long, default_value_t = 600)]
    pub timeout: u64,
    #[command(flatten)]
    pub target: Target,
}

#[derive(Debug, Args)]
pub struct StatusOpts {
    /// An app (project/environment/app): its state and Doctor. Without one,
    /// the state of this server.
    pub app: Option<String>,
    /// Print JSON.
    #[arg(long)]
    pub json: bool,
    #[command(flatten)]
    pub target: Target,
}

#[derive(Debug, Args)]
pub struct LogsOpts {
    pub app: String,
    /// Keep printing new lines.
    #[arg(short, long)]
    pub follow: bool,
    /// Lines of the container before its last restart.
    #[arg(long, conflicts_with = "follow")]
    pub previous: bool,
    /// Lines per pod to start with.
    #[arg(long)]
    pub tail: Option<i64>,
    /// Only pods of this process.
    #[arg(long)]
    pub process: Option<String>,
    #[command(flatten)]
    pub target: Target,
}

#[derive(Debug, Args)]
pub struct RollbackOpts {
    pub app: String,
    /// The revision to return to (default: the one before the current).
    #[arg(long)]
    pub to: Option<i64>,
    #[command(flatten)]
    pub target: Target,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
struct Context {
    url: String,
    token: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    project: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    environment: Option<String>,
}

#[derive(Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
struct Contexts {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    current: Option<String>,
    #[serde(default)]
    contexts: BTreeMap<String, Context>,
}

/// `KUBEN_CONTEXT_FILE`, else `$XDG_CONFIG_HOME/kuben/contexts.json`, else
/// `~/.config/kuben/contexts.json`.
fn contexts_file() -> anyhow::Result<PathBuf> {
    if let Some(file) = std::env::var_os("KUBEN_CONTEXT_FILE") {
        return Ok(file.into());
    }
    let config = std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|home| Path::new(&home).join(".config")))
        .context("neither XDG_CONFIG_HOME nor HOME is set; set KUBEN_CONTEXT_FILE")?;
    Ok(config.join("kuben").join("contexts.json"))
}

impl Contexts {
    fn load(file: &Path) -> anyhow::Result<Self> {
        match std::fs::read_to_string(file) {
            Ok(text) => serde_json::from_str(&text).with_context(|| format!("reading {}", file.display())),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Self::default()),
            Err(e) => Err(e).with_context(|| format!("reading {}", file.display())),
        }
    }

    /// Written whole, readable by its owner only: it holds tokens.
    fn save(&self, file: &Path) -> anyhow::Result<()> {
        if let Some(dir) = file.parent() {
            std::fs::create_dir_all(dir)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt as _;
                std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)).ok();
            }
        }
        let partial = file.with_extension("json.partial");
        kuben_api::host::write_owner_only(&partial, &serde_json::to_string_pretty(self)?)?;
        std::fs::rename(&partial, file)?;
        Ok(())
    }

    /// The context to use: `KUBEN_URL` with `KUBEN_TOKEN`, else the named or
    /// current one.
    fn resolve(&self, name: Option<&str>) -> anyhow::Result<Context> {
        if let (Ok(url), Ok(token)) = (std::env::var("KUBEN_URL"), std::env::var("KUBEN_TOKEN")) {
            return Ok(Context {
                url,
                token,
                ..Context::default()
            });
        }
        let name = name
            .or(self.current.as_deref())
            .context("not logged in: kuben login https://<your server> (or set KUBEN_URL and KUBEN_TOKEN)")?;
        self.contexts
            .get(name)
            .cloned()
            .with_context(|| format!("no context `{name}`; kuben login creates it"))
    }
}

struct Session {
    client: ApiClient,
    context: Context,
    target: Target,
}

impl Session {
    fn open(target: Target) -> anyhow::Result<Self> {
        let context = Contexts::load(&contexts_file()?)?.resolve(target.context.as_deref())?;
        Ok(Self {
            client: ApiClient::new(&context.url, &context.token),
            context,
            target,
        })
    }

    fn app(&self, text: &str) -> anyhow::Result<AppPath> {
        let project = self.target.project.as_deref().or(self.context.project.as_deref());
        let environment = self
            .target
            .environment
            .as_deref()
            .or(self.context.environment.as_deref());
        AppPath::parse(text, project, environment).map_err(anyhow::Error::msg)
    }
}

/// A token from standard input, not echoed on a terminal.
fn read_token() -> anyhow::Result<String> {
    let stdin = std::io::stdin();
    let terminal = stdin.is_terminal();
    let echo = |on: bool| {
        #[cfg(unix)]
        if terminal {
            let _ = std::process::Command::new("stty")
                .arg(if on { "echo" } else { "-echo" })
                .stdin(std::process::Stdio::inherit())
                .status();
        }
        #[cfg(not(unix))]
        let _ = on;
    };
    if terminal {
        eprint!("API token (Account → API tokens): ");
        std::io::stderr().flush().ok();
    }
    echo(false);
    let mut line = String::new();
    let read = stdin.lock().read_line(&mut line);
    echo(true);
    if terminal {
        eprintln!();
    }
    read?;
    let token = line.trim().to_owned();
    if token.is_empty() {
        bail!("no token given");
    }
    Ok(token)
}

fn host_of(url: &str) -> String {
    let rest = url.split_once("://").map_or(url, |(_, rest)| rest);
    rest.split(['/', ':']).next().unwrap_or(rest).to_owned()
}

pub async fn login(opts: LoginOpts) -> anyhow::Result<()> {
    let url = opts.url.trim_end_matches('/').to_owned();
    if !(url.starts_with("https://") || url.starts_with("http://")) {
        bail!("give the server's address with its scheme, e.g. https://{url}");
    }
    let token = match opts.token {
        Some(token) => token,
        None => read_token()?,
    };
    let me = ApiClient::new(&url, &token)
        .me()
        .await
        .with_context(|| format!("signing in to {url}"))?;
    let name = opts.name.unwrap_or_else(|| host_of(&url));
    let file = contexts_file()?;
    let mut contexts = Contexts::load(&file)?;
    contexts.contexts.insert(
        name.clone(),
        Context {
            url: url.clone(),
            token,
            project: opts.target.project,
            environment: opts.target.environment,
        },
    );
    contexts.current = Some(name.clone());
    contexts.save(&file)?;
    if url.starts_with("http://") && !matches!(host_of(&url).as_str(), "localhost" | "127.0.0.1" | "::1") {
        eprintln!("warning: {url} is plain HTTP; the token crosses the network unencrypted");
    }
    println!(
        "Logged in to {url} as {} (context {name}, saved in {})",
        me["email"].as_str().unwrap_or("?"),
        file.display()
    );
    Ok(())
}

pub async fn apps(opts: AppsOpts) -> anyhow::Result<()> {
    let session = Session::open(opts.target)?;
    let client = &session.client;
    let only_project = session
        .target
        .project
        .clone()
        .or_else(|| session.context.project.clone());
    let only_environment = session
        .target
        .environment
        .clone()
        .or_else(|| session.context.environment.clone());
    let mut rows = Vec::new();
    for project in client.projects().await? {
        if only_project.as_ref().is_some_and(|p| p != &project.name) {
            continue;
        }
        for environment in client.environments(&project.name).await? {
            if only_environment.as_ref().is_some_and(|e| e != &environment.name) {
                continue;
            }
            rows.extend(client.apps(&project.name, &environment.name).await?);
        }
    }
    if opts.json {
        println!("{}", serde_json::to_string_pretty(&rows)?);
        return Ok(());
    }
    let table: Vec<[String; 5]> = rows
        .iter()
        .map(|a| {
            [
                format!("{}/{}/{}", a.project, a.environment, a.name),
                if a.ready {
                    "ready".into()
                } else {
                    a.reason.clone().unwrap_or_else(|| "not ready".into())
                },
                a.image.clone().unwrap_or_default(),
                a.url.clone().unwrap_or_default(),
                a.processes.len().to_string(),
            ]
        })
        .collect();
    print_table(&["APP", "STATE", "IMAGE", "URL", "PROCESSES"], &table);
    Ok(())
}

fn print_table<const N: usize>(header: &[&str; N], rows: &[[String; N]]) {
    let mut widths = header.map(str::len);
    for row in rows {
        for (w, cell) in widths.iter_mut().zip(row) {
            *w = (*w).max(cell.chars().count());
        }
    }
    let line = |cells: Vec<&str>| {
        let text: Vec<String> = cells
            .iter()
            .zip(widths)
            .map(|(c, w)| format!("{c:<w$}"))
            .collect();
        println!("{}", text.join("  ").trim_end());
    };
    line(header.to_vec());
    for row in rows {
        line(row.iter().map(String::as_str).collect());
    }
}

/// The revision the app is on now (0 before its first run).
fn current_revision(releases: &[ReleaseDto]) -> i64 {
    releases
        .iter()
        .find(|r| r.current)
        .or_else(|| releases.first())
        .map_or(0, |r| r.revision)
}

pub async fn deploy(opts: DeployOpts) -> anyhow::Result<()> {
    let session = Session::open(opts.target)?;
    let app = session.app(&opts.app)?;
    let image = if opts.image.contains("@sha256:") {
        opts.image.clone()
    } else {
        let resolved = RegistryResolver::new()
            .resolve(&opts.image)
            .await
            .with_context(|| format!("resolving {}", opts.image))?;
        let pinned = resolved.pinned();
        eprintln!("{} → {pinned}", opts.image);
        pinned
    };
    let releases = session.client.releases(&app).await?;
    let request = StartDeploymentRequest {
        image: Some(image),
        release: None,
        config: None,
        reason: DeployReason::Deploy,
        expected_generation: u64::try_from(current_revision(&releases)).unwrap_or(0),
    };
    let key = opts
        .idempotency_key
        .unwrap_or_else(|| format!("kuben-cli-{}", uuid::Uuid::now_v7()));
    let run = session.client.deploy(&app, &request, &key).await?;
    println!("{app}: revision {} accepted ({})", run.generation, run.phase);
    if opts.no_wait {
        return Ok(());
    }
    wait_for(
        &session.client,
        &app,
        run.run,
        run.phase,
        Duration::from_secs(opts.timeout),
    )
    .await
}

async fn wait_for(
    client: &ApiClient,
    app: &AppPath,
    run: uuid::Uuid,
    mut phase: String,
    timeout: Duration,
) -> anyhow::Result<()> {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        match RunPhase::parse(&phase) {
            Some(RunPhase::Succeeded) => {
                println!("{app}: deployed");
                return Ok(());
            }
            Some(p) if p.is_final() => {
                bail!("{app}: the deployment ended {phase}; kuben status {app} says why")
            }
            _ => {}
        }
        if tokio::time::Instant::now() >= deadline {
            bail!(
                "{app}: still {phase} after {}s; it goes on on the server",
                timeout.as_secs()
            );
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
        let now = client.deployment(app, run).await?.phase;
        if now != phase {
            println!("{app}: {now}");
            phase = now;
        }
    }
}

pub async fn status(opts: StatusOpts, app: &str) -> anyhow::Result<()> {
    let session = Session::open(opts.target)?;
    let path = session.app(app)?;
    let (detail, releases, doctor) = tokio::join!(
        session.client.app(&path),
        session.client.releases(&path),
        session.client.doctor(&path)
    );
    let (detail, releases) = (detail?, releases?);
    if opts.json {
        let doctor = doctor.ok();
        println!(
            "{}",
            serde_json::to_string_pretty(&serde_json::json!({
                "app": detail.app, "pods": detail.pods, "releases": releases, "doctor": doctor,
            }))?
        );
        return Ok(());
    }
    let a = &detail.app;
    let state = if a.ready { "ready" } else { "not ready" };
    println!(
        "{path}  {state}{}",
        a.reason.as_ref().map(|r| format!(" ({r})")).unwrap_or_default()
    );
    if let Some(message) = a.message.as_deref().filter(|m| !m.is_empty()) {
        println!("  {message}");
    }
    println!("image     {}", a.image.as_deref().unwrap_or("-"));
    println!("url       {}", a.url.as_deref().unwrap_or("-"));
    if let Some(r) = releases.iter().find(|r| r.current) {
        println!(
            "revision  {} ({}{})",
            r.revision,
            r.reason,
            r.actor.as_ref().map(|a| format!(" by {a}")).unwrap_or_default()
        );
    }
    let ready = detail.pods.iter().filter(|p| p.ready).count();
    println!("pods      {ready}/{} ready", detail.pods.len());
    for pod in &detail.pods {
        println!(
            "  {}  {}{}  restarts {}",
            pod.name,
            pod.phase,
            pod.reason.as_ref().map(|r| format!(" ({r})")).unwrap_or_default(),
            pod.restarts
        );
    }
    match doctor {
        Ok(report) => print_doctor(&report),
        Err(e) => println!("doctor    unavailable: {e}"),
    }
    Ok(())
}

fn print_doctor(report: &DoctorReport) {
    println!("doctor    {}", report.status);
    for check in &report.checks {
        let tag = match check.status.as_str() {
            "ok" => "OK  ",
            "warn" => "WARN",
            "fail" => "FAIL",
            _ => "????",
        };
        let subject = if check.subject.is_empty() {
            String::new()
        } else {
            format!(" {}", check.subject)
        };
        println!("  [{tag}] {}{subject}: {}", check.id, check.detail);
        if let Some(hint) = &check.hint {
            println!("         {hint}");
        }
    }
}

pub async fn logs(opts: LogsOpts) -> anyhow::Result<()> {
    let session = Session::open(opts.target)?;
    let app = session.app(&opts.app)?;
    let options = LogOptions {
        tail: opts.tail,
        process: opts.process,
        previous: opts.previous,
    };
    if !opts.follow {
        let pods = session.client.logs(&app, &options).await?;
        let prefix = pods.len() > 1;
        for pod in pods {
            if let Some(error) = &pod.error {
                eprintln!("[{}] {error}", pod.pod);
            }
            for line in &pod.lines {
                print_line(prefix.then_some(pod.pod.as_str()), line);
            }
        }
        return Ok(());
    }
    let mut events = Box::pin(session.client.follow_logs(&app, &options).await?);
    let mut out = std::io::stdout().lock();
    while let Some(event) = events.next().await {
        match event? {
            FollowEvent::Line(line) => {
                writeln!(out, "[{}] {}", line.pod, line.line)?;
                out.flush()?;
            }
            FollowEvent::End(end) => match (end.pod, end.error) {
                (Some(pod), Some(error)) => eprintln!("[{pod}] {error}"),
                (Some(pod), None) => eprintln!("[{pod}] log ended"),
                (None, error) => {
                    eprintln!("{}", error.unwrap_or_else(|| "the stream ended".into()));
                    break;
                }
            },
        }
    }
    Ok(())
}

/// `2026-…Z text` without its timestamp, prefixed with the pod.
fn print_line(pod: Option<&str>, line: &str) {
    let text = match line.split_once(' ') {
        Some((time, rest)) if time.len() >= 20 && time.as_bytes().get(4) == Some(&b'-') => rest,
        _ => line,
    };
    match pod {
        Some(pod) => println!("[{pod}] {text}"),
        None => println!("{text}"),
    }
}

/// The revision before `current` in a newest-first history.
fn previous_revision(releases: &[ReleaseDto]) -> Option<i64> {
    let current = current_revision(releases);
    releases.iter().map(|r| r.revision).filter(|r| *r < current).max()
}

pub async fn rollback(opts: RollbackOpts) -> anyhow::Result<()> {
    let session = Session::open(opts.target)?;
    let app = session.app(&opts.app)?;
    let revision = match opts.to {
        Some(revision) => revision,
        None => previous_revision(&session.client.releases(&app).await?)
            .with_context(|| format!("{app} has no earlier revision"))?,
    };
    let dto = session.client.rollback(&app, revision).await?;
    println!(
        "{app}: back to revision {revision} ({})",
        dto.image.as_deref().unwrap_or("image unknown")
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn release(revision: i64, current: bool) -> ReleaseDto {
        ReleaseDto {
            revision,
            image: None,
            reason: "deploy".into(),
            note: None,
            actor: None,
            created_at: 0,
            current,
        }
    }

    #[test]
    fn contexts_are_written_for_their_owner_only_and_read_back() {
        let dir = std::env::temp_dir().join(format!("kuben-contexts-{}", uuid::Uuid::now_v7()));
        let file = dir.join("kuben").join("contexts.json");
        let mut contexts = Contexts::default();
        contexts.contexts.insert(
            "prod".into(),
            Context {
                url: "https://kuben.example.com".into(),
                token: "kbn_x".into(),
                project: Some("shop".into()),
                environment: None,
            },
        );
        contexts.current = Some("prod".into());
        contexts.save(&file).expect("saved");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&file).expect("file").permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        let read = Contexts::load(&file).expect("read");
        assert_eq!(read, contexts);
        assert_eq!(
            read.resolve(None).expect("current").project.as_deref(),
            Some("shop")
        );
        assert!(read.resolve(Some("staging")).is_err());
        assert_eq!(
            Contexts::load(&dir.join("none.json")).expect("empty"),
            Contexts::default()
        );
        std::fs::remove_dir_all(dir).ok();
    }

    #[test]
    fn revisions_are_found_in_the_history() {
        let history = [
            release(5, false),
            release(4, true),
            release(3, false),
            release(1, false),
        ];
        assert_eq!(current_revision(&history), 4);
        assert_eq!(previous_revision(&history), Some(3));
        assert_eq!(current_revision(&[]), 0);
        assert_eq!(previous_revision(&[release(1, true)]), None);
    }

    #[test]
    fn hosts_name_contexts() {
        assert_eq!(host_of("https://kuben.example.com/"), "kuben.example.com");
        assert_eq!(host_of("http://127.0.0.1:3000"), "127.0.0.1");
    }
}

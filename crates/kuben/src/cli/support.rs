//! `kuben support-bundle` (M4.11; plan §19.3 S09): a local file for a
//! support conversation.
//!
//! The bundle is one JSON file written with mode 0600 under
//! `<state dir>/support`. Nothing is uploaded. What goes in is allowlisted:
//! - versions;
//! - the configuration keys in [`CONFIG_ALLOWLIST`];
//! - the output of `kuben doctor`;
//! - database counts and error codes ([`kuben_store::Store::support_summary`]);
//! - the state of the cluster and of Kuben's own pods;
//! - and, only with `--logs`, the newest lines of Kuben's own logs.
//!
//! Every string passes [`redact_credentials`], and the file is bounded
//! ([`MAX_BUNDLE_BYTES`]; logs are cut first). `--preview` shows what would
//! go in and writes nothing. Older bundles beyond `--keep` are removed.
//! Making a bundle is audited. Redaction is best effort: read the file
//! before you share it.

use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::Context as _;
use k8s_openapi::api::core::v1::{Event, Node, Pod};
use kube::{
    Api, ResourceExt,
    api::{ListParams, LogParams},
};
use kuben_core::{
    config::{Config, in_cluster},
    time::now_ms,
};
use kuben_platform::registry::{ClusterRegistry, own_namespace, redact_credentials};
use kuben_store::{Store, repo::NewAudit};
use serde_json::{Map, Value, json};
use sha2::{Digest as _, Sha256};

use crate::cli::{VERSION, version_string};

/// The largest bundle written.
pub const MAX_BUNDLE_BYTES: usize = 8 << 20;
/// Log lines per container (or of the host service) at most.
pub const MAX_LOG_LINES: u32 = 5_000;
const PREFIX: &str = "kuben-support-";
/// Warning events of Kuben's namespace at most.
const MAX_EVENTS: usize = 200;
const DOCTOR_TIMEOUT: Duration = Duration::from_mins(2);

/// The configuration a bundle may show: a key is kept when its dotted path
/// is listed or lies under a listed path. Credentials, URLs with
/// credentials and personal data are never listed.
pub const CONFIG_ALLOWLIST: &[&str] = &[
    "server.bind",
    "server.metrics_bind",
    "server.activator_bind",
    "server.public_url",
    "server.roles",
    "server.request_timeout_secs",
    "server.max_body_bytes",
    "database.max_connections",
    "runtime",
    "kube.watch_namespace",
    "kube.required",
    "kube.namespace",
    "kube.leader_election",
    "security.session_ttl_hours",
    "security.cookie_secure",
    "security.trust_forwarded_for",
    "security.insecure_setup",
    "security.password_min_length",
    "telemetry.log_format",
    "telemetry.log_level",
    "bootstrap.org_slug",
    "agent.bind",
    "agent.hub_name",
    "agent.certificate_hours",
    "agent.heartbeat_secs",
    "agent.local",
    "agent.namespace",
    "git.github_app_id",
    "git.github_api_url",
    "build.enabled",
    "build.namespace",
    "build.buildkit_image",
    "build.fetch_image",
    "build.railpack_image",
    "build.scanner_image",
    "build.deadline_secs",
    "build.max_concurrent",
    "build.max_concurrent_per_org",
    "build.insecure_registry",
    "build.rescan_hours",
    "ci.github_actions",
    "ci.github_oidc_issuer",
    "sso.enabled",
    "sso.issuer",
    "sso.scopes",
    "sso.group_claim",
    "sso.default_role",
    "sso.require_verified_email",
    "sso.disable_password_for_linked",
    "quota",
    "backup",
    "notify",
    "retention",
    "domains",
];

/// Options of `kuben support-bundle`.
#[derive(Debug, Default, clap::Args)]
pub struct SupportOpts {
    /// Directory the bundle goes to [default: `support` in the state dir].
    #[arg(long)]
    pub out: Option<PathBuf>,
    /// Show what the bundle would hold, and write nothing.
    #[arg(long)]
    pub preview: bool,
    /// Also the newest lines of Kuben's own logs (they may name apps and
    /// people: read them before sharing).
    #[arg(long)]
    pub logs: bool,
    /// Log lines per container with --logs.
    #[arg(long, default_value_t = 500, value_parser = clap::value_parser!(u32).range(1..=i64::from(MAX_LOG_LINES)))]
    pub log_lines: u32,
    /// Bundles kept in the directory; older ones are removed.
    #[arg(long, default_value_t = 3)]
    pub keep: u32,
}

/// `cfg` as the bundle shows it: only [`CONFIG_ALLOWLIST`] paths, with the
/// dropped keys listed by name.
#[must_use]
pub fn allowed_config(cfg: &Config) -> Value {
    let full = serde_json::to_value(cfg).unwrap_or(Value::Null);
    let mut omitted = Vec::new();
    let kept = keep_allowed(&full, "", &mut omitted);
    json!({ "values": kept, "omitted": omitted })
}

fn keep_allowed(value: &Value, path: &str, omitted: &mut Vec<String>) -> Value {
    let Value::Object(map) = value else {
        return Value::Null;
    };
    let mut out = Map::new();
    for (key, child) in map {
        let here = if path.is_empty() {
            key.clone()
        } else {
            format!("{path}.{key}")
        };
        if CONFIG_ALLOWLIST
            .iter()
            .any(|a| *a == here || here.starts_with(&format!("{a}.")))
        {
            out.insert(key.clone(), redacted(child.clone()));
        } else if CONFIG_ALLOWLIST
            .iter()
            .any(|a| a.starts_with(&format!("{here}.")))
        {
            out.insert(key.clone(), keep_allowed(child, &here, omitted));
        } else {
            omitted.push(here);
        }
    }
    Value::Object(out)
}

/// `value` with every string passed through [`redact_credentials`].
#[must_use]
pub fn redacted(value: Value) -> Value {
    match value {
        Value::String(s) => Value::String(redact_credentials(&s)),
        Value::Array(items) => Value::Array(items.into_iter().map(redacted).collect()),
        Value::Object(map) => Value::Object(map.into_iter().map(|(k, v)| (k, redacted(v))).collect()),
        other => other,
    }
}

/// `sections` as the bundle's bytes, with log lines cut (newest kept) until
/// it fits in `max` bytes.
pub fn encode(mut sections: Map<String, Value>, max: usize) -> anyhow::Result<Vec<u8>> {
    loop {
        let bytes = serde_json::to_vec_pretty(&sections)?;
        if bytes.len() <= max {
            return Ok(bytes);
        }
        let Some(Value::Object(logs)) = sections.get_mut("logs") else {
            anyhow::bail!(
                "the bundle takes {} bytes, more than {max}, without logs to cut",
                bytes.len()
            );
        };
        let longest = logs
            .values_mut()
            .filter_map(|v| v.as_array_mut())
            .max_by_key(|lines| lines.len())
            .filter(|lines| !lines.is_empty());
        match longest {
            Some(lines) => {
                let cut = lines.len().div_ceil(2);
                lines.drain(..cut);
            }
            None => {
                sections.remove("logs");
            }
        }
    }
}

/// `kuben support-bundle`.
pub async fn run(cfg: Config, opts: SupportOpts, config_path: Option<&Path>) -> anyhow::Result<()> {
    let mut sections = Map::new();
    sections.insert(
        "about".into(),
        json!({
            "kuben": VERSION,
            "build": version_string(),
            "generated_at": now_ms(),
            "in_cluster": in_cluster(),
            "format": 1,
            "support_envelope": kuben_core::support::envelope(),
        }),
    );
    sections.insert("config".into(), allowed_config(&cfg));
    sections.insert("doctor".into(), doctor(config_path).await);
    let store = Store::connect_unmigrated(&cfg.database).await;
    sections.insert("database".into(), database(store.as_ref().ok()).await);
    let client = ClusterRegistry::connect(&cfg.kube).await.map(|r| r.primary());
    let namespace = own_namespace(cfg.kube.namespace.as_deref()).unwrap_or_else(|| "kuben-system".into());
    sections.insert(
        "cluster".into(),
        match &client {
            Ok(client) => cluster(client, &namespace).await,
            Err(e) => json!({ "error": redact_credentials(&e.to_string()) }),
        },
    );
    if opts.logs {
        let logs = match &client {
            Ok(client) if in_cluster() => pod_logs(client, &namespace, opts.log_lines).await,
            _ => host_logs(opts.log_lines).await,
        };
        sections.insert("logs".into(), logs);
    }
    let sections = sections.into_iter().map(|(k, v)| (k, redacted(v))).collect();
    let bytes = encode(sections, MAX_BUNDLE_BYTES)?;
    if opts.preview {
        print_preview(&bytes)?;
        return Ok(());
    }
    let dir = opts.out.unwrap_or_else(|| cfg.state_dir().join("support"));
    let (path, sha) = write_bundle(&dir, &bytes)?;
    let removed = prune(&dir, opts.keep)?;
    if let Ok(store) = &store {
        audit(&cfg, store, &sha, bytes.len(), opts.logs).await;
    }
    println!(
        "support bundle written to {} ({} KiB, sha256 {sha}){}",
        path.display(),
        bytes.len() >> 10,
        if removed > 0 {
            format!("; {removed} older bundle(s) removed")
        } else {
            String::new()
        }
    );
    println!("nothing was uploaded; read the file before you share it");
    Ok(())
}

fn print_preview(bytes: &[u8]) -> anyhow::Result<()> {
    let sections: Map<String, Value> = serde_json::from_slice(bytes)?;
    println!("the bundle would hold {} KiB:", bytes.len() >> 10);
    for (name, value) in &sections {
        let size = serde_json::to_vec(value).map_or(0, |v| v.len());
        println!("  {name:<10} {size:>7} bytes");
    }
    if let Some(omitted) = sections
        .get("config")
        .and_then(|c| c.get("omitted"))
        .and_then(Value::as_array)
    {
        let names: Vec<&str> = omitted.iter().filter_map(Value::as_str).collect();
        println!("configuration left out: {}", names.join(", "));
    }
    println!("nothing was written (drop --preview to write it)");
    Ok(())
}

/// The output of `kuben doctor` with the same configuration.
async fn doctor(config_path: Option<&Path>) -> Value {
    let run = async {
        let mut cmd = tokio::process::Command::new(std::env::current_exe()?);
        if let Some(path) = config_path {
            cmd.arg("--config").arg(path);
        }
        cmd.arg("doctor").kill_on_drop(true);
        let out = tokio::time::timeout(DOCTOR_TIMEOUT, cmd.output())
            .await
            .context("doctor timed out")??;
        let text = String::from_utf8_lossy(&out.stdout);
        anyhow::Ok(json!({
            "passed": out.status.success(),
            "lines": text.lines().take(500).collect::<Vec<_>>(),
        }))
    };
    run.await.unwrap_or_else(|e| json!({ "error": format!("{e:#}") }))
}

async fn database(store: Option<&Store>) -> Value {
    let Some(store) = store else {
        return json!({ "error": "the database is not reachable" });
    };
    let err = |e: &dyn std::fmt::Display| json!({ "error": e.to_string() });
    let schema = store
        .schema_state()
        .await
        .map_or_else(|e| err(&e), |s| json!({ "applied": s.applied, "dirty": s.dirty }));
    let facts = store.database_facts().await.map_or_else(
        |e| err(&e),
        |f| {
            json!({
                "major": f.major(), "tls": f.tls, "wal_level": f.wal_level,
                "archive_mode": f.archive_mode, "in_recovery": f.in_recovery,
            })
        },
    );
    let backup = store.last_backup().await.map_or_else(
        |e| err(&e),
        |b| json!(b.map(|b| json!({ "finished_at": b.finished_at, "bytes": b.bytes }))),
    );
    json!({
        "schema": schema,
        "latest_known_migration": kuben_store::Store::latest_migration(),
        "server": facts,
        "last_backup": backup,
        "server_versions": store.server_versions().await.map_or_else(|e| err(&e), |v| json!(v)),
        "agent_protocols": store.agent_protocols().await.map_or_else(|e| err(&e), |v| json!(v)),
        "summary": store.support_summary().await.map_or_else(|e| err(&e), |s| json!(s)),
    })
}

async fn cluster(client: &kube::Client, namespace: &str) -> Value {
    let err = |e: &dyn std::fmt::Display| json!({ "error": e.to_string() });
    let version = client
        .apiserver_version()
        .await
        .map_or_else(|e| err(&e), |v| json!(v.git_version));
    let nodes = Api::<Node>::all(client.clone())
        .list(&ListParams::default())
        .await
        .map_or_else(
            |e| err(&e),
            |list| json!(list.items.iter().map(node).collect::<Vec<_>>()),
        );
    let pods = Api::<Pod>::namespaced(client.clone(), namespace)
        .list(&ListParams::default())
        .await
        .map_or_else(
            |e| err(&e),
            |list| json!(list.items.iter().map(pod).collect::<Vec<_>>()),
        );
    let events = Api::<Event>::namespaced(client.clone(), namespace)
        .list(&ListParams::default().fields("type=Warning"))
        .await
        .map_or_else(|e| err(&e), |list| warnings(list.items));
    let configs = Api::<kuben_crd::KubenConfig>::all(client.clone())
        .list(&ListParams::default())
        .await
        .map_or_else(
            |e| err(&e),
            |list| {
                json!(
                    list.items
                        .iter()
                        .map(|c| json!({ "name": c.name_any(), "status": c.status }))
                        .collect::<Vec<_>>()
                )
            },
        );
    json!({
        "apiserver": version,
        "namespace": namespace,
        "nodes": nodes,
        "pods": pods,
        "warning_events": events,
        "kuben_configs": configs,
    })
}

fn node(n: &Node) -> Value {
    let status = n.status.as_ref();
    json!({
        "kubelet": status.and_then(|s| s.node_info.as_ref()).map(|i| &i.kubelet_version),
        "os_image": status.and_then(|s| s.node_info.as_ref()).map(|i| &i.os_image),
        "allocatable": status.and_then(|s| s.allocatable.as_ref()),
        "conditions": status.and_then(|s| s.conditions.as_ref()).map(|c| {
            c.iter().map(|c| json!({ "type": c.type_, "status": c.status, "reason": c.reason })).collect::<Vec<_>>()
        }),
        "unschedulable": n.spec.as_ref().and_then(|s| s.unschedulable),
    })
}

fn pod(p: &Pod) -> Value {
    let status = p.status.as_ref();
    let containers = status.and_then(|s| s.container_statuses.as_ref()).map(|cs| {
        cs.iter()
            .map(|c| {
                json!({
                    "name": c.name, "image": c.image, "ready": c.ready,
                    "restarts": c.restart_count,
                    "last_termination": c.last_state.as_ref()
                        .and_then(|s| s.terminated.as_ref())
                        .map(|t| json!({ "reason": t.reason, "exit_code": t.exit_code })),
                })
            })
            .collect::<Vec<_>>()
    });
    json!({
        "name": p.name_any(),
        "phase": status.and_then(|s| s.phase.as_ref()),
        "node": p.spec.as_ref().and_then(|s| s.node_name.as_ref()),
        "containers": containers,
    })
}

fn warnings(mut events: Vec<Event>) -> Value {
    events.sort_by_key(|e| std::cmp::Reverse(e.last_timestamp.as_ref().map(|t| t.0)));
    json!(events
        .iter()
        .take(MAX_EVENTS)
        .map(|e| json!({
            "object": format!("{}/{}", e.involved_object.kind.as_deref().unwrap_or("?"), e.involved_object.name.as_deref().unwrap_or("?")),
            "reason": e.reason,
            "message": e.message,
            "count": e.count,
            "last": e.last_timestamp.as_ref().map(|t| t.0.to_string()),
        }))
        .collect::<Vec<_>>())
}

/// The newest lines of every container of Kuben's own pods.
async fn pod_logs(client: &kube::Client, namespace: &str, lines: u32) -> Value {
    let pods = Api::<Pod>::namespaced(client.clone(), namespace);
    let list = match pods
        .list(&ListParams::default().labels("app.kubernetes.io/name=kuben"))
        .await
    {
        Ok(list) => list,
        Err(e) => return json!({ "error": e.to_string() }),
    };
    let mut out = Map::new();
    for p in &list.items {
        let containers = p
            .spec
            .iter()
            .flat_map(|s| s.containers.iter().map(|c| c.name.clone()));
        for container in containers {
            let params = LogParams {
                container: Some(container.clone()),
                tail_lines: Some(i64::from(lines)),
                timestamps: true,
                ..LogParams::default()
            };
            let text = pods
                .logs(&p.name_any(), &params)
                .await
                .unwrap_or_else(|e| format!("(no logs: {e})"));
            out.insert(
                format!("{}/{container}", p.name_any()),
                json!(text.lines().collect::<Vec<_>>()),
            );
        }
    }
    Value::Object(out)
}

/// The newest lines of the host's `kuben` service.
async fn host_logs(lines: u32) -> Value {
    let out = tokio::process::Command::new("journalctl")
        .args(["-u", "kuben", "--no-pager", "-o", "short-iso", "-n"])
        .arg(lines.to_string())
        .kill_on_drop(true)
        .output()
        .await;
    match out {
        Ok(out) if out.status.success() => {
            let text = String::from_utf8_lossy(&out.stdout);
            json!({ "kuben.service": text.lines().collect::<Vec<_>>() })
        }
        Ok(out) => json!({ "error": String::from_utf8_lossy(&out.stderr).trim().to_owned() }),
        Err(e) => json!({ "error": format!("journalctl: {e}") }),
    }
}

fn write_bundle(dir: &Path, bytes: &[u8]) -> anyhow::Result<(PathBuf, String)> {
    std::fs::create_dir_all(dir).with_context(|| format!("creating {}", dir.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
    }
    let stamp = k8s_openapi::jiff::Timestamp::now().strftime("%Y%m%dT%H%M%SZ");
    let path = dir.join(format!("{PREFIX}{stamp}.json"));
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    std::io::Write::write_all(
        &mut options
            .open(&path)
            .with_context(|| format!("creating {}", path.display()))?,
        bytes,
    )?;
    let sha = Sha256::digest(bytes)
        .iter()
        .fold(String::with_capacity(64), |mut s, b| {
            use std::fmt::Write as _;
            let _ = write!(s, "{b:02x}");
            s
        });
    Ok((path, sha))
}

/// Remove all but the newest `keep` bundles in `dir`.
pub fn prune(dir: &Path, keep: u32) -> anyhow::Result<usize> {
    let mut bundles: Vec<PathBuf> = std::fs::read_dir(dir)?
        .filter_map(Result::ok)
        .map(|e| e.path())
        .filter(|p| {
            p.extension().is_some_and(|e| e == "json")
                && p.file_name()
                    .and_then(|n| n.to_str())
                    .is_some_and(|n| n.starts_with(PREFIX))
        })
        .collect();
    // The names sort by time.
    bundles.sort();
    let excess = bundles.len().saturating_sub(keep.max(1) as usize);
    for old in &bundles[..excess] {
        std::fs::remove_file(old).with_context(|| format!("removing {}", old.display()))?;
    }
    Ok(excess)
}

async fn audit(cfg: &Config, store: &Store, sha: &str, bytes: usize, logs: bool) {
    let org = store
        .installation_org(&cfg.bootstrap.org_slug)
        .await
        .ok()
        .flatten();
    let actor = std::env::var("SUDO_USER")
        .or_else(|_| std::env::var("USER"))
        .unwrap_or_else(|_| "unknown".into());
    let record = NewAudit {
        org_id: org,
        actor_kind: "host".into(),
        actor_id: Some(actor),
        action: "support.bundle.created".into(),
        target_kind: Some("installation".into()),
        target_ref: None,
        outcome: "success".into(),
        ip: None,
        request_id: None,
        data: Some(json!({ "sha256": sha, "bytes": bytes, "logs": logs })),
    };
    if let Err(e) = store.append_audit(record).await {
        eprintln!("warning: the bundle was not audited: {e}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_allowlisted_configuration_is_kept() {
        let mut cfg = Config::default();
        cfg.database.url = "postgres://kuben:hunter22@db.example.com/kuben".into();
        cfg.bootstrap.admin_password = Some("admin-secret".into());
        cfg.bootstrap.admin_email = "ops@example.com".into();
        cfg.sso.client_secret = Some("sso-secret".into());
        cfg.git.github_webhook_secret = Some("hook-secret".into());
        cfg.telemetry.otlp_endpoint = Some("https://u:otlp-secret@otel.example.com".into());
        cfg.server.public_url = Some("https://user:pw-secret@kuben.example.com".into());
        cfg.notify.allow_http = true;
        let shown = allowed_config(&cfg).to_string();
        for secret in [
            "hunter22",
            "admin-secret",
            "ops@example.com",
            "sso-secret",
            "hook-secret",
            "otlp-secret",
            "pw-secret",
        ] {
            assert!(!shown.contains(secret), "{secret} leaked: {shown}");
        }
        let view = allowed_config(&cfg);
        assert_eq!(view["values"]["notify"]["allow_http"], json!(true));
        assert_eq!(view["values"]["backup"]["keep"], json!(7));
        let omitted: Vec<&str> = view["omitted"]
            .as_array()
            .expect("list")
            .iter()
            .filter_map(Value::as_str)
            .collect();
        assert!(omitted.contains(&"database.url"));
        assert!(omitted.contains(&"bootstrap.admin_password"));
    }

    #[test]
    fn bundles_are_bounded_by_cutting_logs() {
        let mut sections = Map::new();
        sections.insert("about".into(), json!({ "kuben": VERSION }));
        let lines: Vec<String> = (0..1000)
            .map(|i| format!("line {i:04} {}", "x".repeat(80)))
            .collect();
        sections.insert("logs".into(), json!({ "a": lines }));
        let bytes = encode(sections.clone(), 20_000).expect("fits");
        assert!(bytes.len() <= 20_000);
        let kept: Map<String, Value> = serde_json::from_slice(&bytes).expect("json");
        let a = kept["logs"]["a"].as_array().expect("lines");
        assert!(!a.is_empty() && a.len() < 1000);
        assert!(
            a.last()
                .and_then(Value::as_str)
                .is_some_and(|l| l.starts_with("line 0999")),
            "newest kept"
        );
        sections.insert("about".into(), json!({ "big": "y".repeat(30_000) }));
        assert!(encode(sections, 20_000).is_err(), "cannot fit without logs");
    }

    #[test]
    fn old_bundles_are_pruned() {
        let dir = std::env::temp_dir().join(format!("kuben-support-test-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir_all(&dir).expect("dir");
        for stamp in ["20260101T000000Z", "20260102T000000Z", "20260103T000000Z"] {
            std::fs::write(dir.join(format!("{PREFIX}{stamp}.json")), b"{}").expect("write");
        }
        std::fs::write(dir.join("other.json"), b"{}").expect("write");
        assert_eq!(prune(&dir, 2).expect("prune"), 1);
        assert!(!dir.join(format!("{PREFIX}20260101T000000Z.json")).exists());
        assert!(dir.join("other.json").exists());
        assert_eq!(prune(&dir, 0).expect("prune"), 1, "one is always kept");
        std::fs::remove_dir_all(&dir).expect("cleanup");
    }

    #[test]
    fn strings_are_redacted_everywhere() {
        let v = redacted(json!({ "a": ["see postgres://u:secret@h/db"], "b": { "c": "https://x:y@z" } }));
        assert!(!v.to_string().contains("secret"));
        assert!(!v.to_string().contains("x:y@"));
    }
}

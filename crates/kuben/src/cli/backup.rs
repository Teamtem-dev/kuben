//! `kuben backup` and `kuben restore` (M4.7; plan §17, S03).
//!
//! A backup is a directory `kuben-<UTC time>` holding the database as a
//! `pg_dump` custom archive, a manifest (versions, schema, checksum, the
//! fingerprints of the secret keys) and, only when asked, the secret keyring.
//! Kubernetes objects are not part of it: SQL is the only desired state, and
//! a restore writes every app again.
//!
//! A restore needs an empty database. It checks the archive, loads it with
//! `pg_restore`, migrates it forward, checks (or installs) the keyring, and
//! then fences the time after the backup: sessions end, API tokens are
//! revoked, every generation jumps ahead, and every app gets a run that
//! writes it again. `pg_dump`/`pg_restore` come from PostgreSQL's client
//! tools and must be at least as new as the server.

use std::{
    fmt::Write as _,
    io::Read as _,
    path::{Path, PathBuf},
    process::Command,
};

use anyhow::{Context, bail, ensure};
use kuben_core::{config::Config, time::now_ms};
use kuben_platform::secrets::Keyring;
use kuben_store::{Store, repo::BackupRecord};
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

use crate::cli::{BackupOpts, RestoreOpts, VERSION};

/// The manifest format this binary writes and reads.
const FORMAT: u32 = 1;
const MANIFEST: &str = "manifest.json";
const DUMP: &str = "database.dump";
const KEYRING: &str = "secrets.keyring";
const PREFIX: &str = "kuben-";

#[derive(Debug, Serialize, Deserialize)]
struct Manifest {
    format: u32,
    kuben: String,
    created_at: i64,
    schema: i64,
    database_version: i32,
    dump: FileSum,
    keyring_included: bool,
    /// `version:sha256-hex` of every key the installation used.
    key_fingerprints: Vec<String>,
}

#[derive(Debug, Serialize, Deserialize)]
struct FileSum {
    file: String,
    bytes: u64,
    sha256: String,
}

/// `url` without its password, and the password: the password travels to
/// the PostgreSQL tools in their environment, never on a command line.
fn split_password(url: &str) -> (String, Option<String>) {
    let Some((scheme, rest)) = url.split_once("://") else {
        return (url.to_owned(), None);
    };
    let authority_end = rest.find(['/', '?']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(authority_end);
    let Some((userinfo, host)) = authority.rsplit_once('@') else {
        return (url.to_owned(), None);
    };
    let Some((user, password)) = userinfo.split_once(':') else {
        return (url.to_owned(), None);
    };
    (
        format!("{scheme}://{user}@{host}{tail}"),
        Some(percent_decode(password)),
    )
}

fn percent_decode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%'
            && let Some(byte) = s.get(i + 1..i + 3).and_then(|h| u8::from_str_radix(h, 16).ok())
        {
            out.push(byte);
            i += 3;
            continue;
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// A PostgreSQL client tool for `url`.
fn pg_tool(tool: &str, url: &str) -> (Command, String) {
    let (bare, password) = split_password(url);
    let mut cmd = Command::new(tool);
    cmd.env("PGAPPNAME", format!("kuben-{tool}"));
    if let Some(password) = password {
        cmd.env("PGPASSWORD", password);
    }
    (cmd, bare)
}

/// Whether PostgreSQL's client tools are on the `PATH`.
#[must_use]
pub fn client_tools_installed() -> bool {
    Command::new("pg_dump")
        .arg("--version")
        .output()
        .is_ok_and(|o| o.status.success())
}

/// The major version of `tool`, e.g. 17 for `pg_dump (PostgreSQL) 17.4`.
fn tool_major(tool: &str) -> anyhow::Result<i32> {
    let out = Command::new(tool)
        .arg("--version")
        .output()
        .with_context(|| format!("{tool} is not installed: install PostgreSQL's client tools"))?;
    parse_major(&String::from_utf8_lossy(&out.stdout))
        .with_context(|| format!("unexpected `{tool} --version`"))
}

fn parse_major(version: &str) -> Option<i32> {
    version
        .split_whitespace()
        .find_map(|word| word.split('.').next()?.parse().ok())
}

fn execute(mut cmd: Command, what: &str) -> anyhow::Result<()> {
    let out = cmd.output().with_context(|| format!("{what} could not start"))?;
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        bail!("{what} failed: {}", err.trim());
    }
    Ok(())
}

fn sha256_file(path: &Path) -> anyhow::Result<(u64, String)> {
    let mut file = std::fs::File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 1 << 16];
    let mut bytes = 0u64;
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
        bytes += n as u64;
    }
    let hex = hasher.finalize().iter().fold(String::new(), |mut s, b| {
        let _ = write!(s, "{b:02x}");
        s
    });
    Ok((bytes, hex))
}

fn fingerprints(keyring: &Keyring) -> Vec<String> {
    keyring
        .fingerprints()
        .iter()
        .map(|(version, fp)| {
            let hex = fp.iter().fold(String::new(), |mut s, b| {
                let _ = write!(s, "{b:02x}");
                s
            });
            format!("{version}:{hex}")
        })
        .collect()
}

fn write_private(path: &Path, bytes: &[u8]) -> anyhow::Result<()> {
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    std::io::Write::write_all(&mut options.open(path)?, bytes)?;
    Ok(())
}

/// Whether the newest good backup (finished at `last`) is fresh at `now`,
/// with `max_age_hours` as the bound (0: not watched). The message says why.
pub fn freshness(last: Option<i64>, now: i64, max_age_hours: u32) -> Result<String, String> {
    let limit = i64::from(max_age_hours) * 3_600_000;
    match last {
        None if max_age_hours == 0 => Ok("no backup yet (not watched: backup.max_age_hours = 0)".into()),
        None => Err("no backup has been made: run `kuben backup` (or schedule it)".into()),
        Some(at) => {
            let hours = (now - at).max(0) / 3_600_000;
            if max_age_hours == 0 || now - at <= limit {
                Ok(format!("the newest good backup is {hours} h old"))
            } else {
                Err(format!(
                    "the newest good backup is {hours} h old, over backup.max_age_hours = {max_age_hours}"
                ))
            }
        }
    }
}

pub async fn run(cfg: Config, opts: BackupOpts) -> anyhow::Result<()> {
    let root = opts.out.clone().unwrap_or_else(|| cfg.backup_dir());
    let started_at = now_ms();
    let store = Store::connect_unmigrated(&cfg.database).await?;
    let schema = store.schema_version().await?;
    let outcome = write_backup(&cfg, &store, &root, opts.include_keyring).await;
    let record = BackupRecord {
        scheduled: opts.scheduled,
        succeeded: outcome.is_ok(),
        location: outcome.as_ref().map_or_else(
            |_| root.display().to_string(),
            |(dir, _)| dir.display().to_string(),
        ),
        bytes: outcome.as_ref().ok().map(|(_, m)| m.dump.bytes),
        sha256: outcome.as_ref().ok().map(|(_, m)| m.dump.sha256.clone()),
        kuben: VERSION.to_owned(),
        schema,
        detail: outcome
            .as_ref()
            .err()
            .map(|e| format!("{e:#}").chars().take(2048).collect()),
        started_at,
        finished_at: now_ms(),
    };
    if let Err(e) = store.record_backup(&record).await {
        eprintln!("warning: the backup was not recorded in the database: {e}");
    }
    let (dir, manifest) = outcome?;
    let removed = prune(&root, opts.keep.unwrap_or(cfg.backup.keep))?;
    println!(
        "backup written to {} ({} MiB, schema {}, sha256 {}){}",
        dir.display(),
        manifest.dump.bytes >> 20,
        manifest.schema,
        manifest.dump.sha256,
        if removed > 0 {
            format!("; {removed} older backup(s) removed")
        } else {
            String::new()
        }
    );
    if manifest.keyring_included {
        println!(
            "the backup holds the secret keyring: whoever has it can read every secret; keep it encrypted"
        );
    } else {
        println!(
            "the secret keyring ({}) is not in the backup: back it up separately, a restore needs it",
            cfg.secret_keyring_file().display()
        );
    }
    Ok(())
}

async fn write_backup(
    cfg: &Config,
    store: &Store,
    root: &Path,
    include_keyring: bool,
) -> anyhow::Result<(PathBuf, Manifest)> {
    let facts = store.database_facts().await?;
    let major = tool_major("pg_dump")?;
    ensure!(
        major >= facts.major(),
        "pg_dump {major} is older than the PostgreSQL {} server; install client tools {} or newer",
        facts.major(),
        facts.major()
    );
    let keyring = Keyring::load(&cfg.secret_keyring_file()).context("the secret keyring")?;
    let stamp = k8s_openapi::jiff::Timestamp::now().strftime("%Y%m%dT%H%M%SZ");
    let dir = root.join(format!("{PREFIX}{stamp}"));
    std::fs::create_dir_all(&dir).with_context(|| format!("creating {}", dir.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))?;
    }
    let dump = dir.join(DUMP);
    let (mut cmd, url) = pg_tool("pg_dump", &cfg.database.url);
    cmd.args(["--format=custom", "--no-owner", "--no-privileges", "--file"])
        .arg(&dump)
        .arg("--dbname")
        .arg(url);
    execute(cmd, "pg_dump")?;
    let (bytes, sha256) = sha256_file(&dump)?;
    if include_keyring {
        let text = std::fs::read(cfg.secret_keyring_file())?;
        write_private(&dir.join(KEYRING), &text)?;
    }
    let manifest = Manifest {
        format: FORMAT,
        kuben: VERSION.to_owned(),
        created_at: now_ms(),
        schema: store.schema_version().await?,
        database_version: facts.version_num,
        dump: FileSum {
            file: DUMP.to_owned(),
            bytes,
            sha256,
        },
        keyring_included: include_keyring,
        key_fingerprints: fingerprints(&keyring),
    };
    write_private(&dir.join(MANIFEST), &serde_json::to_vec_pretty(&manifest)?)?;
    Ok((dir, manifest))
}

/// Remove all but the newest `keep` complete backups under `root`.
fn prune(root: &Path, keep: u32) -> anyhow::Result<usize> {
    if keep == 0 {
        return Ok(0);
    }
    let mut backups: Vec<PathBuf> = std::fs::read_dir(root)?
        .filter_map(Result::ok)
        .map(|e| e.path())
        .filter(|p| {
            p.file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with(PREFIX))
                && p.join(MANIFEST).is_file()
        })
        .collect();
    // The names sort by time.
    backups.sort();
    let excess = backups.len().saturating_sub(keep as usize);
    for old in &backups[..excess] {
        std::fs::remove_dir_all(old).with_context(|| format!("removing {}", old.display()))?;
    }
    Ok(excess)
}

/// The manifest of the backup in `dir`, checked against its dump.
fn checked_manifest(dir: &Path) -> anyhow::Result<Manifest> {
    let text = std::fs::read(dir.join(MANIFEST))
        .with_context(|| format!("{} is not a kuben backup (no {MANIFEST})", dir.display()))?;
    let manifest: Manifest = serde_json::from_slice(&text).context("the manifest does not parse")?;
    ensure!(
        manifest.format == FORMAT,
        "backup format {} is not supported by this kuben",
        manifest.format
    );
    ensure!(
        manifest.dump.file == DUMP,
        "unexpected dump file {}",
        manifest.dump.file
    );
    let (bytes, sha256) = sha256_file(&dir.join(DUMP))?;
    ensure!(
        bytes == manifest.dump.bytes && sha256 == manifest.dump.sha256,
        "the dump does not match its manifest: the backup is damaged"
    );
    Ok(manifest)
}

pub async fn restore(cfg: Config, opts: RestoreOpts) -> anyhow::Result<()> {
    let manifest = checked_manifest(&opts.from)?;
    ensure!(
        manifest.schema <= Store::latest_migration(),
        "the backup is from a newer Kuben ({}, schema {}); restore it with that version or newer",
        manifest.kuben,
        manifest.schema
    );
    let dump = opts.from.join(DUMP);
    let (mut list, _) = pg_tool("pg_restore", "");
    list.arg("--list").arg(&dump);
    execute(list, "pg_restore --list")?;
    println!(
        "backup of {} (Kuben {}, schema {}) is intact",
        k8s_openapi::jiff::Timestamp::from_millisecond(manifest.created_at)
            .map_or_else(|_| "?".into(), |t| t.to_string()),
        manifest.kuben,
        manifest.schema
    );
    if opts.check {
        return Ok(());
    }
    let keyring = restore_keyring(&cfg, &opts.from, &manifest)?;
    let url = cfg.database.url.trim();
    ensure!(
        Store::database_is_empty(url).await?,
        "the database is not empty: restore into a new, empty database"
    );
    let (mut cmd, bare) = pg_tool("pg_restore", url);
    cmd.args([
        "--no-owner",
        "--no-privileges",
        "--exit-on-error",
        "--single-transaction",
        "--dbname",
    ])
    .arg(bare)
    .arg(&dump);
    execute(cmd, "pg_restore")?;
    let store = Store::connect(&cfg.database).await?;
    kuben_platform::secrets::prepare(&store, &keyring).await?;
    let fenced = store
        .after_restore(manifest.created_at, &manifest.kuben, VERSION)
        .await?;
    let runs = store.redeliver_all("system:restore").await?;
    let age_min = (now_ms() - manifest.created_at).max(0) / 60_000;
    println!(
        "restored: schema {} → {}; {} session(s) ended and {} API token(s) revoked (sign in again, issue new tokens); \
         {} app(s) raised to a new generation, {runs} run(s) started to write them again",
        manifest.schema,
        store.schema_version().await?,
        fenced.sessions_ended,
        fenced.tokens_revoked,
        fenced.targets_raised,
    );
    println!(
        "data written in the {age_min} minute(s) between the backup and now is not in the restored database"
    );
    Ok(())
}

/// The keyring the restored secrets need: the configured one, else the one
/// in the backup (installed in its place). Its keys must be the backup's.
fn restore_keyring(cfg: &Config, from: &Path, manifest: &Manifest) -> anyhow::Result<Keyring> {
    let path = cfg.secret_keyring_file();
    let keyring = if path.exists() {
        Keyring::load(&path)?
    } else if manifest.keyring_included {
        let text = std::fs::read_to_string(from.join(KEYRING)).context("the keyring in the backup")?;
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        let keyring = Keyring::install(&path, &text)?;
        println!("installed the backup's secret keyring at {}", path.display());
        keyring
    } else {
        bail!(
            "no secret keyring at {} and none in the backup: put the installation's keyring there first",
            path.display()
        );
    };
    let ours = fingerprints(&keyring);
    let missing: Vec<&String> = manifest
        .key_fingerprints
        .iter()
        .filter(|f| !ours.contains(f))
        .collect();
    ensure!(
        missing.is_empty(),
        "the keyring at {} lacks keys the backup was sealed with ({} of {}): it is not this installation's",
        path.display(),
        missing.len(),
        manifest.key_fingerprints.len()
    );
    Ok(keyring)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn passwords_leave_the_url() {
        assert_eq!(
            split_password("postgres://kuben:s%40cret@db:5432/kuben?sslmode=verify-full"),
            (
                "postgres://kuben@db:5432/kuben?sslmode=verify-full".to_owned(),
                Some("s@cret".to_owned())
            )
        );
        assert_eq!(
            split_password("postgres://kuben@db/kuben"),
            ("postgres://kuben@db/kuben".to_owned(), None)
        );
        assert_eq!(
            split_password("postgresql:///kuben?host=/run/postgresql"),
            ("postgresql:///kuben?host=/run/postgresql".to_owned(), None)
        );
        assert_eq!(percent_decode("a%2Fb%zz"), "a/b%zz");
    }

    #[test]
    fn stale_backups_are_reported() {
        let hour = 3_600_000;
        assert!(freshness(None, 0, 26).is_err());
        assert!(freshness(None, 0, 0).is_ok());
        assert_eq!(
            freshness(Some(0), 2 * hour, 26),
            Ok("the newest good backup is 2 h old".into())
        );
        assert!(freshness(Some(0), 27 * hour, 26).is_err());
        assert!(freshness(Some(0), 27 * hour, 0).is_ok());
    }

    #[test]
    fn tool_versions_are_read() {
        assert_eq!(parse_major("pg_dump (PostgreSQL) 17.4"), Some(17));
        assert_eq!(
            parse_major("pg_restore (PostgreSQL) 16.9 (Debian 16.9-1)"),
            Some(16)
        );
        assert_eq!(parse_major("nothing here"), None);
    }

    fn scratch() -> PathBuf {
        let dir = std::env::temp_dir().join(format!("kuben-backup-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir_all(&dir).expect("dir");
        dir
    }

    fn fake_backup(root: &Path, stamp: &str, dump: &[u8]) -> PathBuf {
        let dir = root.join(format!("{PREFIX}{stamp}"));
        std::fs::create_dir_all(&dir).expect("dir");
        std::fs::write(dir.join(DUMP), dump).expect("dump");
        let (bytes, sha256) = sha256_file(&dir.join(DUMP)).expect("sum");
        let manifest = Manifest {
            format: FORMAT,
            kuben: "2.0.0".into(),
            created_at: 1,
            schema: 25,
            database_version: 170_004,
            dump: FileSum {
                file: DUMP.into(),
                bytes,
                sha256,
            },
            keyring_included: false,
            key_fingerprints: vec![],
        };
        std::fs::write(dir.join(MANIFEST), serde_json::to_vec(&manifest).expect("json")).expect("manifest");
        dir
    }

    #[test]
    fn damaged_backups_are_refused() {
        let root = scratch();
        let dir = fake_backup(&root, "20260917T000000Z", b"PGDMP archive");
        assert_eq!(checked_manifest(&dir).expect("intact").schema, 25);
        std::fs::write(dir.join(DUMP), b"PGDMP changed").expect("tamper");
        assert!(checked_manifest(&dir).is_err());
        assert!(checked_manifest(&root).is_err(), "not a backup");
        std::fs::remove_dir_all(root).expect("cleanup");
    }

    #[test]
    fn only_the_newest_complete_backups_are_kept() {
        let root = scratch();
        for stamp in ["20260915T000000Z", "20260916T000000Z", "20260917T000000Z"] {
            fake_backup(&root, stamp, b"x");
        }
        std::fs::create_dir_all(root.join("kuben-incomplete")).expect("dir");
        std::fs::create_dir_all(root.join("unrelated")).expect("dir");
        assert_eq!(prune(&root, 2).expect("prune"), 1);
        assert!(!root.join("kuben-20260915T000000Z").exists());
        assert!(root.join("kuben-20260917T000000Z").exists());
        assert!(root.join("kuben-incomplete").exists() && root.join("unrelated").exists());
        assert_eq!(prune(&root, 0).expect("prune"), 0, "0 keeps everything");
        std::fs::remove_dir_all(root).expect("cleanup");
    }
}

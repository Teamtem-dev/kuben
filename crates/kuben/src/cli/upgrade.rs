//! Upgrades (M4.8): `kuben upgrade-check`, run with the *new* binary before
//! it replaces the old one, and the journaled migration every start and
//! `kuben migrate` go through, with a backup first when migrations are
//! pending.

use std::{path::Path, process::Command};

use kuben_core::{
    config::Config,
    time::now_ms,
    upgrade::{Facts, Level, preflight},
};
use kuben_store::Store;

use crate::cli::{UpgradeCheckOpts, VERSION};

/// Free bytes of the file system holding `dir` (its nearest existing
/// ancestor), from `df`.
fn free_bytes(dir: &Path) -> Option<u64> {
    let existing = dir.ancestors().find(|p| p.exists())?;
    let out = Command::new("df").arg("-Pk").arg(existing).output().ok()?;
    parse_df(&String::from_utf8_lossy(&out.stdout))
}

/// The available KiB of the last line of `df -Pk`, in bytes.
fn parse_df(output: &str) -> Option<u64> {
    let fields: Vec<&str> = output.lines().last()?.split_whitespace().collect();
    fields.get(3)?.parse::<u64>().ok().map(|kib| kib << 10)
}

/// What the preflight of this binary against `store` sees.
async fn facts(cfg: &Config, store: &Store, major: bool) -> anyhow::Result<Facts> {
    let schema = store.schema_state().await?;
    let versions = store.server_versions().await?;
    let last_backup = if schema.applied >= 25 {
        store.last_backup().await?.map(|b| b.finished_at)
    } else {
        None
    };
    Ok(Facts {
        running: versions.into_iter().next(),
        target: VERSION.to_owned(),
        major,
        schema: schema.applied,
        target_schema: Store::latest_migration(),
        dirty_schema: schema.dirty,
        last_backup,
        now: now_ms(),
        active_operations: if schema.applied > 0 {
            store.active_operations().await?
        } else {
            0
        },
        agents: if schema.applied > 0 {
            store.agent_protocols().await?
        } else {
            Vec::new()
        },
        target_protocols: kuben_agent::protocol::SUPPORTED_VERSIONS,
        free_bytes: free_bytes(&cfg.backup_dir()),
    })
}

/// `kuben upgrade-check`: every finding; fails when any does.
pub async fn check(cfg: Config, opts: UpgradeCheckOpts) -> anyhow::Result<()> {
    let store = Store::connect_unmigrated(&cfg.database).await?;
    let findings = preflight(&facts(&cfg, &store, opts.major).await?);
    for f in &findings {
        let mark = match f.level {
            Level::Ok => "OK  ",
            Level::Warn => "WARN",
            Level::Fail => "FAIL",
        };
        println!("{mark}  {:<11} {}", f.check, f.detail);
    }
    store.close().await?;
    if findings.iter().any(|f| f.level == Level::Fail) {
        anyhow::bail!("this upgrade is not safe yet: fix the failures above, nothing has changed");
    }
    println!("ready to upgrade to {VERSION}");
    Ok(())
}

/// Migrate `store` as this version, journaled; when migrations are pending
/// on a database with data, take a backup first where PostgreSQL's client
/// tools are installed. A failed backup stops the migration.
pub async fn migrate(cfg: &Config, store: &Store) -> anyhow::Result<()> {
    let state = store.guard_schema().await?;
    let pending = Store::latest_migration() - state.applied;
    if pending > 0 && state.applied > 0 && cfg.backup.before_upgrade {
        if crate::cli::backup::client_tools_installed() {
            let opts = crate::cli::BackupOpts {
                out: None,
                include_keyring: false,
                keep: None,
                scheduled: true,
            };
            tracing::info!(pending, "taking a backup before the migrations");
            crate::cli::backup::run(cfg.clone(), opts).await?;
        } else {
            tracing::warn!(
                pending,
                "migrations are pending and pg_dump is not installed here: no backup is taken first \
                 (the Helm chart takes one in its pre-upgrade hook)"
            );
        }
    }
    let migrated = store.migrate_journaled(VERSION).await?;
    if migrated.from_schema != migrated.to_schema || migrated.resumed {
        tracing::info!(
            from = migrated.from_schema,
            to = migrated.to_schema,
            resumed = migrated.resumed,
            version = VERSION,
            "database upgraded"
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn free_space_is_read_from_df() {
        let out = "Filesystem 1024-blocks Used Available Capacity Mounted on\n\
                   /dev/sda1 102400 2048 100352 2% /\n";
        assert_eq!(parse_df(out), Some(100_352 << 10));
        assert_eq!(parse_df(""), None);
        assert_eq!(parse_df("header only"), None);
        assert!(free_bytes(Path::new("/definitely/not/there/at/all")).is_some() || cfg!(windows));
    }
}

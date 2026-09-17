//! The upgrade journal and the facts an upgrade preflight reads (M4.8,
//! migration 0026).
//!
//! [`Store::migrate_journaled`] refuses a schema newer than this binary or a
//! migration that failed half-way, writes a `running` journal row before the
//! migrations when the journal exists, applies them, and settles the row.

use kuben_core::time::now_ms;
use uuid::Uuid;

use super::product::counter;
use crate::{Store, StoreError};

const HAS_MIGRATIONS: &str = "SELECT to_regclass('_sqlx_migrations') IS NOT NULL";
const APPLIED: &str = "SELECT COALESCE(max(version) FILTER (WHERE success), 0), bool_or(NOT success) IS TRUE \
     FROM _sqlx_migrations";
const HAS_JOURNAL: &str = "SELECT to_regclass('upgrade_runs') IS NOT NULL";
const VERSIONS: &str = "SELECT version FROM server_versions";
const BEGIN_RUN: &str = "INSERT INTO upgrade_runs \
     (id, from_version, to_version, from_schema, to_schema, status, started_at) \
     VALUES ($1, $2, $3, $4, $5, 'running', $6)";
const SETTLE_RUN: &str = "UPDATE upgrade_runs SET status = $2, detail = $3, finished_at = $4, to_schema = $5 \
     WHERE id = $1";
const INTERRUPTED: &str = "SELECT id, to_version FROM upgrade_runs WHERE status = 'running'";
const SEEN: &str = "INSERT INTO server_versions (version, schema, first_started_at, last_started_at) \
     VALUES ($1, $2, $3, $3) ON CONFLICT (version) DO UPDATE \
     SET schema = EXCLUDED.schema, last_started_at = EXCLUDED.last_started_at, \
         starts = server_versions.starts + 1";
const ACTIVE: &str = "SELECT count(*) FROM operations WHERE NOT done";
const AGENTS: &str = "SELECT cluster_id::text, protocol_version FROM cluster_agents \
     WHERE revoked_at IS NULL AND linked_at IS NOT NULL ORDER BY cluster_id";

/// The migrations a database has.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SchemaState {
    /// The newest migration applied; 0 for an empty database.
    pub applied: i64,
    /// A migration failed half-way.
    pub dirty: bool,
}

/// What [`Store::migrate_journaled`] did.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Migrated {
    pub from_schema: i64,
    pub to_schema: i64,
    /// A run an earlier start left unfinished was completed.
    pub resumed: bool,
}

impl Store {
    /// The migrations this database has.
    pub async fn schema_state(&self) -> Result<SchemaState, StoreError> {
        let exists: bool = sqlx::query_scalar(HAS_MIGRATIONS).fetch_one(self.pool()).await?;
        if !exists {
            return Ok(SchemaState {
                applied: 0,
                dirty: false,
            });
        }
        let (applied, dirty): (i64, bool) = sqlx::query_as(APPLIED).fetch_one(self.pool()).await?;
        Ok(SchemaState { applied, dirty })
    }

    /// Refuse a database this binary must not migrate.
    pub async fn guard_schema(&self) -> Result<SchemaState, StoreError> {
        let state = self.schema_state().await?;
        let binary = Self::latest_migration();
        if state.dirty {
            return Err(StoreError::DirtySchema);
        }
        if state.applied > binary {
            return Err(StoreError::SchemaAhead {
                database: state.applied,
                binary,
            });
        }
        Ok(state)
    }

    /// Every server version that started here, newest first.
    pub async fn server_versions(&self) -> Result<Vec<String>, StoreError> {
        let exists: bool = sqlx::query_scalar("SELECT to_regclass('server_versions') IS NOT NULL")
            .fetch_one(self.pool())
            .await?;
        if !exists {
            return Ok(Vec::new());
        }
        let mut versions: Vec<String> = sqlx::query_scalar(VERSIONS).fetch_all(self.pool()).await?;
        versions.sort_by(|a, b| {
            let parse = kuben_core::upgrade::Version::parse;
            parse(b).cmp(&parse(a))
        });
        Ok(versions)
    }

    /// Migrate as `version`, journaled: see the module.
    pub async fn migrate_journaled(&self, version: &str) -> Result<Migrated, StoreError> {
        let before = self.guard_schema().await?;
        let target = Self::latest_migration();
        let journal: bool = sqlx::query_scalar(HAS_JOURNAL).fetch_one(self.pool()).await?;
        let mut resumed = false;
        let mut run = None;
        if journal {
            let open: Vec<(Uuid, String)> = sqlx::query_as(INTERRUPTED).fetch_all(self.pool()).await?;
            for (id, to) in open {
                resumed = true;
                let detail = format!("interrupted; resumed by {version}");
                self.settle(id, "failed", Some(&detail), before.applied).await?;
                tracing::warn!(run = %id, interrupted = %to, %version, "resuming an interrupted upgrade");
            }
            if before.applied < target {
                let id = Uuid::now_v7();
                let from = self.server_versions().await?.into_iter().next();
                sqlx::query(BEGIN_RUN)
                    .bind(id)
                    .bind(from)
                    .bind(version)
                    .bind(before.applied)
                    .bind(target)
                    .bind(now_ms())
                    .execute(self.pool())
                    .await?;
                run = Some(id);
            }
        }
        if let Err(e) = self.migrate().await {
            if let Some(id) = run {
                let after = self.schema_state().await.map_or(before.applied, |s| s.applied);
                let detail = e.to_string().chars().take(2048).collect::<String>();
                self.settle(id, "failed", Some(&detail), after).await.ok();
            }
            return Err(e);
        }
        let now = now_ms();
        if run.is_none() && before.applied < target {
            // The journal arrived with these migrations: record them now.
            let id = Uuid::now_v7();
            sqlx::query(BEGIN_RUN)
                .bind(id)
                .bind(None::<String>)
                .bind(version)
                .bind(before.applied)
                .bind(target)
                .bind(now)
                .execute(self.pool())
                .await?;
            run = Some(id);
        }
        if let Some(id) = run {
            self.settle(id, "succeeded", None, target).await?;
        }
        sqlx::query(SEEN)
            .bind(version)
            .bind(target)
            .bind(now)
            .execute(self.pool())
            .await?;
        Ok(Migrated {
            from_schema: before.applied,
            to_schema: target,
            resumed,
        })
    }

    async fn settle(
        &self,
        id: Uuid,
        status: &str,
        detail: Option<&str>,
        schema: i64,
    ) -> Result<(), StoreError> {
        sqlx::query(SETTLE_RUN)
            .bind(id)
            .bind(status)
            .bind(detail)
            .bind(now_ms())
            .bind(schema)
            .execute(self.pool())
            .await?;
        Ok(())
    }

    /// Operations not settled yet.
    pub async fn active_operations(&self) -> Result<u64, StoreError> {
        let n: i64 = sqlx::query_scalar(ACTIVE).fetch_one(self.pool()).await?;
        Ok(counter(n)?)
    }

    /// `(cluster id, protocol version)` of every linked, unrevoked agent.
    pub async fn agent_protocols(&self) -> Result<Vec<(String, Option<u32>)>, StoreError> {
        let rows: Vec<(String, Option<i32>)> = sqlx::query_as(AGENTS).fetch_all(self.pool()).await?;
        Ok(rows
            .into_iter()
            .map(|(cluster, p)| (cluster, p.and_then(|p| u32::try_from(p).ok())))
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{pg_store, skip};

    #[tokio::test]
    async fn upgrades_are_journaled_and_newer_schemas_refused() {
        let Some(store) = pg_store().await else {
            return skip("upgrades_are_journaled_and_newer_schemas_refused");
        };
        let latest = Store::latest_migration();
        assert_eq!(
            store.schema_state().await.expect("state"),
            SchemaState {
                applied: latest,
                dirty: false
            }
        );
        let again = store.migrate_journaled("2.0.0").await.expect("migrate");
        assert_eq!(
            (again.from_schema, again.to_schema, again.resumed),
            (latest, latest, false)
        );
        store.migrate_journaled("2.0.1").await.expect("migrate");
        assert_eq!(store.server_versions().await.expect("read"), ["2.0.1", "2.0.0"]);

        // An upgrade a crash left `running` is closed and resumed.
        sqlx::query(BEGIN_RUN)
            .bind(Uuid::now_v7())
            .bind("2.0.1")
            .bind("2.1.0")
            .bind(latest)
            .bind(latest)
            .bind(now_ms())
            .execute(store.pool())
            .await
            .expect("run");
        assert!(store.migrate_journaled("2.1.0").await.expect("migrate").resumed);
        let open: i64 = sqlx::query_scalar("SELECT count(*) FROM upgrade_runs WHERE status = 'running'")
            .fetch_one(store.pool())
            .await
            .expect("count");
        assert_eq!(open, 0);

        // A migration only a newer Kuben knows.
        sqlx::query(
            "INSERT INTO _sqlx_migrations (version, description, installed_on, success, checksum, execution_time) \
             VALUES ($1, 'future', now(), true, '\\x00', 0)",
        )
        .bind(latest + 1)
        .execute(store.pool())
        .await
        .expect("future");
        assert!(matches!(
            store.guard_schema().await,
            Err(StoreError::SchemaAhead { database, binary }) if database == latest + 1 && binary == latest
        ));
        assert_eq!(store.active_operations().await.expect("count"), 0);
        assert!(store.agent_protocols().await.expect("agents").is_empty());
    }
}

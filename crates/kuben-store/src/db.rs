use std::{path::Path, str::FromStr, sync::Arc, time::Duration};

use kuben_core::config::DatabaseCfg;
use sqlx::{
    PgPool, SqlitePool,
    postgres::PgPoolOptions,
    sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous},
};

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("database error: {0}")]
    Sqlx(#[from] sqlx::Error),
    #[error("migration error: {0}")]
    Migrate(#[from] sqlx::migrate::MigrateError),
    #[error("unsupported database url: {0}")]
    UnsupportedUrl(String),
    #[error(
        "cannot create the database directory {path}: {source}; point KUBEN_DATABASE__URL at a directory this user can write, e.g. sqlite://$HOME/.local/state/kuben/kuben.db"
    )]
    Directory {
        path: String,
        #[source]
        source: std::io::Error,
    },
}

impl From<StoreError> for kuben_core::Error {
    fn from(e: StoreError) -> Self {
        Self::Internal(e.to_string())
    }
}

/// Backend-specific pools.
#[derive(Debug)]
pub(crate) enum Db {
    Sqlite { writer: SqlitePool, reader: SqlitePool },
    Postgres(PgPool),
}

/// Cheap-to-clone handle to the database.
#[derive(Clone, Debug)]
pub struct Store {
    pub(crate) db: Arc<Db>,
}

impl Store {
    /// Connect according to the URL scheme and run embedded migrations.
    pub async fn connect(cfg: &DatabaseCfg) -> Result<Self, StoreError> {
        let db = if cfg.url.starts_with("sqlite:") {
            connect_sqlite(&cfg.url, cfg.max_connections).await?
        } else if cfg.url.starts_with("postgres:") || cfg.url.starts_with("postgresql:") {
            connect_postgres(&cfg.url, cfg.max_connections).await?
        } else {
            return Err(StoreError::UnsupportedUrl(cfg.url.clone()));
        };
        let store = Self { db: Arc::new(db) };
        store.migrate().await?;
        Ok(store)
    }

    /// In-memory SQLite, for tests and `--dev` mode.
    pub async fn memory() -> Result<Self, StoreError> {
        Self::connect(&DatabaseCfg {
            url: "sqlite::memory:".into(),
            max_connections: 1,
        })
        .await
    }

    /// Apply pending migrations (idempotent; safe to call on every boot).
    pub async fn migrate(&self) -> Result<(), StoreError> {
        match &*self.db {
            Db::Sqlite { writer, .. } => sqlx::migrate!("./migrations/sqlite").run(writer).await?,
            Db::Postgres(pool) => sqlx::migrate!("./migrations/postgres").run(pool).await?,
        }
        Ok(())
    }

    /// Lightweight liveness probe.
    pub async fn ping(&self) -> Result<(), StoreError> {
        match &*self.db {
            Db::Sqlite { reader, .. } => {
                sqlx::query("SELECT 1").execute(reader).await?;
            }
            Db::Postgres(pool) => {
                sqlx::query("SELECT 1").execute(pool).await?;
            }
        }
        Ok(())
    }

    /// Backend name for logs and `/healthz/details`.
    #[must_use]
    pub fn backend(&self) -> &'static str {
        match &*self.db {
            Db::Sqlite { .. } => "sqlite",
            Db::Postgres(_) => "postgres",
        }
    }

    /// Flush WAL and close pools. Part of the ordered shutdown (Invariant I-15).
    pub async fn checkpoint_and_close(&self) -> Result<(), StoreError> {
        match &*self.db {
            Db::Sqlite { writer, reader } => {
                sqlx::query("PRAGMA wal_checkpoint(TRUNCATE)")
                    .execute(writer)
                    .await
                    .ok();
                reader.close().await;
                writer.close().await;
            }
            Db::Postgres(pool) => pool.close().await,
        }
        Ok(())
    }
}

async fn connect_sqlite(url: &str, max_readers: u32) -> Result<Db, StoreError> {
    let in_memory = url.contains(":memory:") || url.contains("mode=memory");
    let base = SqliteConnectOptions::from_str(url)?
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .synchronous(SqliteSynchronous::Normal)
        .busy_timeout(Duration::from_secs(5))
        .foreign_keys(true) // Invariant I-16: never OFF
        .pragma("temp_store", "memory")
        .pragma("cache_size", "-2000");
    if !in_memory {
        ensure_parent_dir(base.get_filename())?;
    }

    let writer = SqlitePoolOptions::new()
        .max_connections(1)
        .connect_with(base.clone())
        .await?;
    let reader = if in_memory {
        // A second connection to `:memory:` would be a different database.
        writer.clone()
    } else {
        SqlitePoolOptions::new()
            .max_connections(max_readers.max(1))
            .connect_with(base.read_only(true))
            .await?
    };
    Ok(Db::Sqlite { writer, reader })
}

async fn connect_postgres(url: &str, max_connections: u32) -> Result<Db, StoreError> {
    let pool = PgPoolOptions::new()
        .max_connections(max_connections.max(2))
        .acquire_timeout(Duration::from_secs(10))
        .connect(url)
        .await?;
    Ok(Db::Postgres(pool))
}

/// Run `$body` against the write pool of whichever backend is active.
macro_rules! with_writer {
    ($store:expr, |$pool:ident| $body:expr) => {
        match &*$store.db {
            $crate::db::Db::Sqlite { writer, .. } => {
                let $pool = writer;
                $body
            }
            $crate::db::Db::Postgres(pool) => {
                let $pool = pool;
                $body
            }
        }
    };
}

/// Run `$body` against the read pool of whichever backend is active.
macro_rules! with_reader {
    ($store:expr, |$pool:ident| $body:expr) => {
        match &*$store.db {
            $crate::db::Db::Sqlite { reader, .. } => {
                let $pool = reader;
                $body
            }
            $crate::db::Db::Postgres(pool) => {
                let $pool = pool;
                $body
            }
        }
    };
}

pub(crate) use {with_reader, with_writer};

/// SQLite creates the database file but not its directory, and the default
/// location (`~/.local/state/kuben`, systemd's state directory) may not exist
/// yet, so create it (owner-only) before opening.
fn ensure_parent_dir(file: &Path) -> Result<(), StoreError> {
    let Some(dir) = file.parent().filter(|d| !d.as_os_str().is_empty()) else {
        return Ok(());
    };
    if dir.is_dir() {
        return Ok(());
    }
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(dir).map_err(|source| StoreError::Directory {
        path: dir.display().to_string(),
        source,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn sqlite_creates_missing_parent_directories() {
        let root = std::env::temp_dir().join(format!("kuben-db-{}", uuid::Uuid::now_v7()));
        let file = root.join("nested").join("kuben.db");
        let store = Store::connect(&DatabaseCfg {
            url: format!("sqlite://{}", file.display()),
            max_connections: 1,
        })
        .await
        .expect("connect creates the directory");
        store.ping().await.expect("ping");
        store.checkpoint_and_close().await.expect("close");
        assert!(file.is_file());
        std::fs::remove_dir_all(&root).ok();
    }

    #[cfg(unix)]
    #[test]
    fn unwritable_location_names_the_directory_and_the_fix() {
        let err = ensure_parent_dir(Path::new("/proc/kuben-cannot-exist/kuben.db"))
            .expect_err("/proc is not writable");
        let msg = err.to_string();
        assert!(msg.contains("/proc/kuben-cannot-exist"), "{msg}");
        assert!(msg.contains("KUBEN_DATABASE__URL"), "{msg}");
    }
}

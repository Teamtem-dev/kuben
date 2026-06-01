use std::{str::FromStr, sync::Arc, time::Duration};

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

    let writer = SqlitePoolOptions::new()
        .max_connections(1)

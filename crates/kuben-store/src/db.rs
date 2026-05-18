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

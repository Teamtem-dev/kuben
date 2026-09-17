use std::time::Duration;

use kuben_core::config::DatabaseCfg;
use sqlx::{PgPool, postgres::PgPoolOptions};

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("database error: {0}")]
    Sqlx(#[from] sqlx::Error),
    #[error("migration error: {0}")]
    Migrate(#[from] sqlx::migrate::MigrateError),
    #[error("unsupported database url {0}: Kuben keeps its data in PostgreSQL (postgres://…)")]
    UnsupportedUrl(String),
    #[error(
        "SQLite is no longer supported: Kuben keeps its data in PostgreSQL (ADR-025) and does not carry the data of a SQLite (1.x) installation over. Point database.url at an empty PostgreSQL server"
    )]
    Sqlite,
    #[error(
        "database.url is not set: Kuben keeps its data in PostgreSQL. For a local server run `docker run -d --name kuben-postgres -e POSTGRES_PASSWORD=kuben -p 5432:5432 postgres:17-alpine` and set KUBEN_DATABASE__URL=postgres://postgres:kuben@localhost:5432/postgres"
    )]
    NotConfigured,
}

impl From<StoreError> for kuben_core::Error {
    fn from(e: StoreError) -> Self {
        Self::Internal(e.to_string())
    }
}

impl StoreError {
    /// A unique constraint refused the write: what it adds exists already.
    #[must_use]
    pub fn is_unique_violation(&self) -> bool {
        matches!(self, Self::Sqlx(sqlx::Error::Database(db)) if db.is_unique_violation())
    }
}

/// Cheap-to-clone handle to the PostgreSQL pool (ADR-025).
#[derive(Clone, Debug)]
pub struct Store {
    pool: PgPool,
}

impl Store {
    /// Connect to PostgreSQL and run the embedded migrations.
    pub async fn connect(cfg: &DatabaseCfg) -> Result<Self, StoreError> {
        Self::migrated(Self::pool_of(cfg).await?).await
    }

    /// Connect without migrating: for tools that must not change the
    /// schema (a backup). The schema may be older than this binary's.
    pub async fn connect_unmigrated(cfg: &DatabaseCfg) -> Result<Self, StoreError> {
        Ok(Self {
            pool: Self::pool_of(cfg).await?,
        })
    }

    /// The newest migration this binary carries.
    #[must_use]
    pub fn latest_migration() -> i64 {
        sqlx::migrate!("./migrations/postgres")
            .iter()
            .map(|m| m.version)
            .max()
            .unwrap_or(0)
    }

    async fn pool_of(cfg: &DatabaseCfg) -> Result<PgPool, StoreError> {
        let url = cfg.url.trim();
        if url.is_empty() {
            return Err(StoreError::NotConfigured);
        }
        if url.starts_with("sqlite:") {
            return Err(StoreError::Sqlite);
        }
        if !(url.starts_with("postgres:") || url.starts_with("postgresql:")) {
            return Err(StoreError::UnsupportedUrl(cfg.url.clone()));
        }
        Ok(PgPoolOptions::new()
            .max_connections(cfg.max_connections.max(2))
            .acquire_timeout(Duration::from_secs(10))
            .connect(url)
            .await?)
    }

    /// Connect with explicit options (for example a `search_path` per test)
    /// and run the migrations.
    #[cfg(any(test, feature = "testing"))]
    pub(crate) async fn connect_postgres(
        options: sqlx::postgres::PgConnectOptions,
        max_connections: u32,
    ) -> Result<Self, StoreError> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections.max(2))
            .acquire_timeout(Duration::from_secs(10))
            .connect_with(options)
            .await?;
        Self::migrated(pool).await
    }

    async fn migrated(pool: PgPool) -> Result<Self, StoreError> {
        let store = Self { pool };
        store.migrate().await?;
        Ok(store)
    }

    /// The pool every repository runs on.
    pub(crate) const fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// Apply pending migrations (idempotent; safe to call on every boot).
    pub async fn migrate(&self) -> Result<(), StoreError> {
        sqlx::migrate!("./migrations/postgres").run(&self.pool).await?;
        Ok(())
    }

    /// Lightweight liveness probe.
    pub async fn ping(&self) -> Result<(), StoreError> {
        sqlx::query("SELECT 1").execute(&self.pool).await?;
        Ok(())
    }

    /// Backend name for logs and `/healthz/details`.
    #[must_use]
    pub const fn backend(&self) -> &'static str {
        "postgres"
    }

    /// The current role, when it bypasses row-level security: a superuser
    /// or a `BYPASSRLS` role. Tenant isolation then rests on the queries
    /// alone (migration 0004). `None` for an ordinary role, as production
    /// should use.
    pub async fn role_bypassing_row_security(&self) -> Result<Option<String>, StoreError> {
        let row: Option<(String, bool)> = sqlx::query_as(
            "SELECT rolname::text, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user",
        )
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.and_then(|(role, bypasses)| bypasses.then_some(role)))
    }

    /// Close the pool. Part of the ordered shutdown (Invariant I-15).
    pub async fn close(&self) -> Result<(), StoreError> {
        self.pool.close().await;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    #[tokio::test]
    async fn a_superuser_is_reported_as_bypassing_row_security() {
        let Some(store) = crate::testing::pg_store().await else {
            crate::testing::skip("database role");
            return;
        };
        // The test server connects as a superuser, as a quick local setup does.
        assert!(store.role_bypassing_row_security().await.expect("role").is_some());
    }

    use super::*;

    fn cfg(url: &str) -> DatabaseCfg {
        DatabaseCfg {
            url: url.into(),
            max_connections: 1,
        }
    }

    #[tokio::test]
    async fn only_postgres_urls_are_accepted() {
        assert!(matches!(
            Store::connect(&cfg(" ")).await,
            Err(StoreError::NotConfigured)
        ));
        assert!(matches!(
            Store::connect(&cfg("sqlite:///data/kuben.db")).await,
            Err(StoreError::Sqlite)
        ));
        assert!(matches!(
            Store::connect(&cfg("mysql://db/kuben")).await,
            Err(StoreError::UnsupportedUrl(_))
        ));
    }
}

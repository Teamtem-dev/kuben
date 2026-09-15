//! Read-only access to the SQLite database of an installation from before
//! PostgreSQL (ADR-025), for an import. Nothing here writes or creates a
//! file, and the server itself never opens SQLite.

use std::{collections::BTreeMap, path::Path};

use sqlx::{
    AssertSqlSafe, SqlitePool,
    sqlite::{SqliteConnectOptions, SqlitePoolOptions},
};

use crate::StoreError;

/// A legacy SQLite database, opened read-only.
#[derive(Debug)]
pub struct LegacySqlite {
    pool: SqlitePool,
}

impl LegacySqlite {
    /// Open `path` read-only. A missing file is an error, never created.
    pub async fn open(path: &Path) -> Result<Self, StoreError> {
        let options = SqliteConnectOptions::new()
            .filename(path)
            .read_only(true)
            .create_if_missing(false);
        let pool = SqlitePoolOptions::new()
            .max_connections(1)
            .connect_with(options)
            .await?;
        Ok(Self { pool })
    }

    /// The row count of every table, which an import compares with what it
    /// wrote.
    pub async fn table_counts(&self) -> Result<BTreeMap<String, i64>, StoreError> {
        let names: Vec<String> = sqlx::query_scalar(
            "SELECT name FROM sqlite_master WHERE type = 'table' \
             AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_sqlx_%' ORDER BY name",
        )
        .fetch_all(&self.pool)
        .await?;
        let mut counts = BTreeMap::new();
        for name in names {
            // The name comes from this file's own catalogue and is quoted as
            // an identifier.
            let sql = format!("SELECT COUNT(*) FROM \"{}\"", name.replace('"', "\"\""));
            let rows: i64 = sqlx::query_scalar(AssertSqlSafe(sql))
                .fetch_one(&self.pool)
                .await?;
            counts.insert(name, rows);
        }
        Ok(counts)
    }

    pub async fn close(self) {
        self.pool.close().await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn counts_rows_and_never_writes() {
        let dir = std::env::temp_dir().join(format!("kuben-legacy-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir_all(&dir).expect("dir");
        let file = dir.join("kuben.db");
        let writer = SqlitePoolOptions::new()
            .max_connections(1)
            .connect_with(
                SqliteConnectOptions::new()
                    .filename(&file)
                    .create_if_missing(true),
            )
            .await
            .expect("create");
        sqlx::query("CREATE TABLE users (id TEXT PRIMARY KEY)")
            .execute(&writer)
            .await
            .expect("table");
        sqlx::query("INSERT INTO users (id) VALUES ('a'), ('b')")
            .execute(&writer)
            .await
            .expect("rows");
        writer.close().await;

        let legacy = LegacySqlite::open(&file).await.expect("open");
        assert_eq!(
            legacy.table_counts().await.expect("counts"),
            BTreeMap::from([("users".to_owned(), 2)])
        );
        assert!(
            sqlx::query("DELETE FROM users")
                .execute(&legacy.pool)
                .await
                .is_err(),
            "opened read-only"
        );
        legacy.close().await;
        assert!(
            LegacySqlite::open(&dir.join("missing.db")).await.is_err(),
            "never creates a file"
        );
        std::fs::remove_dir_all(&dir).ok();
    }
}

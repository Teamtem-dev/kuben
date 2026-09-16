//! The installer's journal, copied from the host once the server runs (M2.6).

use serde_json::Value;

use crate::{Store, StoreError};

const RECORD: &str = "INSERT INTO install_journals (host, journal, recorded_at) \
     VALUES ($1, $2::jsonb, kuben_now_ms()) \
     ON CONFLICT (host) DO UPDATE SET journal = EXCLUDED.journal, recorded_at = EXCLUDED.recorded_at \
     WHERE install_journals.journal IS DISTINCT FROM EXCLUDED.journal";
const READ: &str = "SELECT journal::text, recorded_at FROM install_journals WHERE host = $1";

impl Store {
    /// Keep `journal` as the latest install journal of `host`. False when it
    /// was recorded already.
    pub async fn record_install_journal(&self, host: &str, journal: &Value) -> Result<bool, StoreError> {
        let rows = sqlx::query(RECORD)
            .bind(host)
            .bind(journal.to_string())
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The install journal recorded for `host`, with when it was recorded.
    pub async fn install_journal(&self, host: &str) -> Result<Option<(Value, i64)>, StoreError> {
        let row: Option<(String, i64)> = sqlx::query_as(READ)
            .bind(host)
            .fetch_optional(self.pool())
            .await?;
        row.map(|(text, at)| {
            serde_json::from_str(&text)
                .map(|v| (v, at))
                .map_err(|e| StoreError::from(sqlx::Error::Decode(e.into())))
        })
        .transpose()
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use crate::testing::{pg_store, skip};

    #[tokio::test]
    async fn the_latest_journal_of_a_host_is_kept_once() {
        let Some(store) = pg_store().await else {
            skip("install journals");
            return;
        };
        let first = json!({ "format": 1, "runs": [] });
        assert!(
            store
                .record_install_journal("vps-1", &first)
                .await
                .expect("record")
        );
        assert!(
            !store
                .record_install_journal("vps-1", &first)
                .await
                .expect("again"),
            "an unchanged journal is not written again"
        );
        let second = json!({ "format": 1, "runs": [{ "kuben": "2.0.0" }] });
        assert!(
            store
                .record_install_journal("vps-1", &second)
                .await
                .expect("newer")
        );
        assert_eq!(
            store
                .install_journal("vps-1")
                .await
                .expect("read")
                .map(|(j, _)| j),
            Some(second)
        );
        assert_eq!(store.install_journal("vps-2").await.expect("read"), None);
    }
}

//! Retention budgets (M4.12; plan §17 data lifecycle): rows that only
//! describe the past are removed once they are older than their budget, so
//! the hot tables stay bounded. Audit events, runs, releases and evidence
//! are never removed here.

use kuben_core::{config::RetentionCfg, time::now_ms};
use serde::Serialize;

use crate::{Store, StoreError};

const DAY_MS: i64 = 24 * 3_600_000;
/// Rows removed per statement, so a first run on a large table never holds
/// long locks; the janitor comes back for the rest.
const BATCH: i64 = 5_000;

const SESSIONS: &str = "DELETE FROM sessions WHERE expires_at < $1";
const OUTBOX: &str = "DELETE FROM outbox WHERE id IN (SELECT id FROM outbox \
     WHERE delivered_at IS NOT NULL AND delivered_at < $1 LIMIT $2)";
const DELIVERIES: &str = "DELETE FROM webhook_deliveries WHERE id IN (SELECT id FROM webhook_deliveries \
     WHERE status <> 'pending' AND finished_at < $1 LIMIT $2)";
const INCIDENTS: &str = "DELETE FROM incidents WHERE id IN (SELECT id FROM incidents \
     WHERE org_id = $3 AND resolved_at IS NOT NULL AND resolved_at < $1 LIMIT $2)";

/// What one retention pass removed.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize)]
pub struct Retained {
    pub sessions: u64,
    pub outbox: u64,
    pub webhook_deliveries: u64,
    pub incidents: u64,
    /// A batch was full: more rows are due.
    pub more: bool,
}

impl Retained {
    #[must_use]
    pub const fn total(&self) -> u64 {
        self.sessions + self.outbox + self.webhook_deliveries + self.incidents
    }
}

fn before(now: i64, days: u32) -> i64 {
    now - i64::from(days.max(1)) * DAY_MS
}

impl Store {
    /// Remove what is older than the budgets of `cfg` at `now`: expired
    /// sessions, delivered outbox messages, finished webhook deliveries and
    /// resolved incidents. At most one batch of each per call.
    pub async fn apply_retention(&self, cfg: &RetentionCfg, now: i64) -> Result<Retained, StoreError> {
        let mut done = Retained {
            sessions: sqlx::query(SESSIONS)
                .bind(now)
                .execute(self.pool())
                .await?
                .rows_affected(),
            outbox: sqlx::query(OUTBOX)
                .bind(before(now, cfg.outbox_days))
                .bind(BATCH)
                .execute(self.pool())
                .await?
                .rows_affected(),
            webhook_deliveries: sqlx::query(DELIVERIES)
                .bind(before(now, cfg.webhook_delivery_days))
                .bind(BATCH)
                .execute(self.pool())
                .await?
                .rows_affected(),
            incidents: 0,
            more: false,
        };
        let full = u64::try_from(BATCH).unwrap_or(u64::MAX);
        done.more = done.outbox >= full || done.webhook_deliveries >= full;
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            let removed = sqlx::query(INCIDENTS)
                .bind(before(now, cfg.resolved_incident_days))
                .bind(BATCH)
                .bind(org.to_string())
                .execute(&mut *tenant.tx)
                .await?
                .rows_affected();
            tenant.commit().await?;
            done.incidents += removed;
            done.more |= removed >= full;
        }
        Ok(done)
    }

    /// [`Store::apply_retention`] now.
    pub async fn apply_retention_now(&self, cfg: &RetentionCfg) -> Result<Retained, StoreError> {
        self.apply_retention(cfg, now_ms()).await
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;
    use uuid::Uuid;

    use super::*;
    use crate::{
        repo::{NewIncident, SealedBytes},
        testing::{pg_store, skip},
    };

    #[test]
    fn budgets_are_at_least_a_day() {
        assert_eq!(before(10 * DAY_MS, 0), 9 * DAY_MS);
        assert_eq!(before(10 * DAY_MS, 3), 7 * DAY_MS);
    }

    #[tokio::test]
    async fn old_rows_go_and_recent_ones_stay() {
        let Some(store) = pg_store().await else {
            return skip("old_rows_go_and_recent_ones_stay");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let endpoint = Uuid::now_v7();
        let mut t = store.tenant(org).await.expect("tenant");
        let incident = |key: &str| NewIncident {
            project: None,
            environment: None,
            target: None,
            kind: "backup.stale".into(),
            severity: "critical",
            dedupe_key: key.into(),
            title: "t".into(),
            detail: None,
        };
        let (old, _) = t.open_incident(&incident("old")).await.expect("open");
        let (open, _) = t.open_incident(&incident("open")).await.expect("open");
        assert!(t.resolve_incident(old, "u").await.expect("resolve"));
        let sealed = SealedBytes {
            ciphertext: vec![1],
            wrapped_key: vec![2],
            key_version: 1,
        };
        t.create_endpoint(endpoint, "e", "https://a.example", &["*".into()], &sealed, "u")
            .await
            .expect("endpoint");
        t.enqueue_event(Uuid::now_v7(), "ping", &json!({}))
            .await
            .expect("queue");
        t.enqueue_event(Uuid::now_v7(), "ping", &json!({}))
            .await
            .expect("queue");
        t.commit().await.expect("commit");
        let taken = store.take_deliveries(10, 1).await.expect("take");
        store.delivery_succeeded(taken[0].id, 200).await.expect("done");

        let cfg = RetentionCfg::default();
        let now = now_ms();
        let soon = store.apply_retention(&cfg, now).await.expect("retain");
        assert_eq!(
            (soon.incidents, soon.webhook_deliveries),
            (0, 0),
            "nothing is old yet"
        );
        let later = now + i64::from(cfg.resolved_incident_days + 1) * DAY_MS;
        let gone = store.apply_retention(&cfg, later).await.expect("retain");
        assert_eq!((gone.incidents, gone.webhook_deliveries), (1, 1));
        let mut t = store.tenant(org).await.expect("tenant");
        let left = t.incidents(true, 10).await.expect("read");
        assert_eq!(
            left.iter().map(|i| i.id).collect::<Vec<_>>(),
            [open],
            "open ones stay"
        );
        let deliveries = t.deliveries(endpoint, 10).await.expect("read");
        assert_eq!(deliveries.len(), 1, "the pending one stays");
    }
}

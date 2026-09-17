//! What a support bundle says about the database (M4.11): counts and codes
//! only — no names, payloads, secrets or personal data.

use kuben_core::time::now_ms;
use serde::Serialize;

use crate::{Store, StoreError};

/// How far back the bundle looks at operations.
pub const SUPPORT_WINDOW_MS: i64 = 7 * 24 * 3_600_000;

const OPERATIONS: &str = "SELECT kind, phase, coalesce(error_code, '') AS code, count(*) AS n \
     FROM operations WHERE requested_at >= $1 OR NOT done \
     GROUP BY kind, phase, error_code ORDER BY kind, phase, error_code LIMIT 500";
const STUCK: &str = "SELECT count(*) FROM operations \
     WHERE NOT done AND last_progress_at IS NOT NULL AND last_progress_at < $1";
const OUTBOX: &str = "SELECT count(*) FILTER (WHERE delivered_at IS NULL), \
     coalesce(min(created_at) FILTER (WHERE delivered_at IS NULL), 0) FROM outbox";
const DELIVERIES: &str = "SELECT status, count(*) FROM webhook_deliveries \
     WHERE created_at >= $1 GROUP BY status ORDER BY status";
const TOTALS: &str = "SELECT (SELECT count(*) FROM organizations), (SELECT count(*) FROM users), \
     (SELECT count(*) FROM deployment_runs), (SELECT count(*) FROM releases)";

/// Operations of one kind, phase and error code.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, sqlx::FromRow)]
pub struct OperationCount {
    pub kind: String,
    pub phase: String,
    pub code: String,
    pub n: i64,
}

/// The database part of a support bundle.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct SupportSummary {
    pub organizations: i64,
    pub users: i64,
    pub deployment_runs: i64,
    pub releases: i64,
    pub projects: i64,
    pub environments: i64,
    pub apps: i64,
    pub paused_apps: i64,
    /// Operations requested in the window, and every unfinished one.
    pub operations: Vec<OperationCount>,
    /// Unfinished operations without progress for an hour.
    pub stuck_operations: i64,
    pub outbox_pending: i64,
    /// When the oldest undelivered outbox message was written (0: none).
    pub outbox_oldest_at: i64,
    /// Webhook deliveries of the window by status.
    pub webhook_deliveries: Vec<(String, i64)>,
    /// Open incidents by kind and severity.
    pub open_incidents: Vec<(String, String, i64)>,
    pub disabled_webhooks: i64,
}

const TENANT_COUNTS: &str = "SELECT (SELECT count(*) FROM projects WHERE org_id = $1), \
     (SELECT count(*) FROM environments WHERE org_id = $1), \
     (SELECT count(*) FROM application_targets WHERE org_id = $1), \
     (SELECT count(*) FROM application_targets WHERE org_id = $1 AND paused_at IS NOT NULL), \
     (SELECT count(*) FROM webhook_endpoints WHERE org_id = $1 AND disabled_at IS NOT NULL)";
const TENANT_INCIDENTS: &str = "SELECT kind, severity, count(*) FROM incidents \
     WHERE org_id = $1 AND resolved_at IS NULL GROUP BY kind, severity";

impl Store {
    /// Counts for a support bundle.
    pub async fn support_summary(&self) -> Result<SupportSummary, StoreError> {
        let now = now_ms();
        let (organizations, users, deployment_runs, releases): (i64, i64, i64, i64) =
            sqlx::query_as(TOTALS).fetch_one(self.pool()).await?;
        let (outbox_pending, outbox_oldest_at): (i64, i64) =
            sqlx::query_as(OUTBOX).fetch_one(self.pool()).await?;
        let mut summary = SupportSummary {
            organizations,
            users,
            deployment_runs,
            releases,
            operations: sqlx::query_as(OPERATIONS)
                .bind(now - SUPPORT_WINDOW_MS)
                .fetch_all(self.pool())
                .await?,
            stuck_operations: sqlx::query_scalar(STUCK)
                .bind(now - 3_600_000)
                .fetch_one(self.pool())
                .await?,
            outbox_pending,
            outbox_oldest_at,
            webhook_deliveries: sqlx::query_as(DELIVERIES)
                .bind(now - SUPPORT_WINDOW_MS)
                .fetch_all(self.pool())
                .await?,
            ..SupportSummary::default()
        };
        let mut incidents = std::collections::BTreeMap::<(String, String), i64>::new();
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            let (projects, environments, apps, paused, disabled): (i64, i64, i64, i64, i64) =
                sqlx::query_as(TENANT_COUNTS)
                    .bind(org.to_string())
                    .fetch_one(&mut *tenant.tx)
                    .await?;
            summary.projects += projects;
            summary.environments += environments;
            summary.apps += apps;
            summary.paused_apps += paused;
            summary.disabled_webhooks += disabled;
            let open: Vec<(String, String, i64)> = sqlx::query_as(TENANT_INCIDENTS)
                .bind(org.to_string())
                .fetch_all(&mut *tenant.tx)
                .await?;
            for (kind, severity, n) in open {
                *incidents.entry((kind, severity)).or_default() += n;
            }
        }
        summary.open_incidents = incidents.into_iter().map(|((k, s), n)| (k, s, n)).collect();
        Ok(summary)
    }
}

#[cfg(test)]
mod tests {
    use crate::{
        repo::NewIncident,
        testing::{pg_store, skip},
    };

    #[tokio::test]
    async fn the_summary_counts_across_organizations() {
        let Some(store) = pg_store().await else {
            return skip("the_summary_counts_across_organizations");
        };
        for slug in ["a", "b"] {
            let org = store.create_org(slug, slug).await.expect("org").id;
            let mut t = store.tenant(org).await.expect("tenant");
            t.create_project("shop", "Shop").await.expect("project");
            let incident = NewIncident {
                project: None,
                environment: None,
                target: None,
                kind: "backup.stale".into(),
                severity: "critical",
                dedupe_key: "k".into(),
                title: "t".into(),
                detail: None,
            };
            t.open_incident(&incident).await.expect("incident");
            t.commit().await.expect("commit");
        }
        let summary = store.support_summary().await.expect("summary");
        assert_eq!((summary.organizations, summary.projects), (2, 2));
        assert_eq!(
            summary.open_incidents,
            [("backup.stale".to_owned(), "critical".to_owned(), 2)]
        );
        assert_eq!(summary.outbox_pending, 0);
        let text = serde_json::to_string(&summary).expect("json");
        assert!(!text.contains("shop"), "no names: {text}");
    }
}

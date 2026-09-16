//! Cluster capabilities (M2.1, ADR-031): the facts Kuben last discovered
//! about a cluster, kept per organization like the cluster row they belong
//! to. The facts are opaque JSON here; `kuben_platform::discovery` defines
//! them.

use kuben_core::ids::OrgId;
use serde_json::Value;

use super::Tenant;
use crate::{Store, StoreError};

const RECORD: &str = "INSERT INTO cluster_capabilities (cluster_id, org_id, facts, observed_at) \
     SELECT c.id, c.org_id, $3::jsonb, $4 FROM clusters c WHERE c.org_id = $1 AND c.name = $2 \
     ON CONFLICT (cluster_id) DO UPDATE SET facts = EXCLUDED.facts, observed_at = EXCLUDED.observed_at \
     WHERE cluster_capabilities.observed_at <= EXCLUDED.observed_at";
const READ: &str = "SELECT k.facts::text AS facts, k.observed_at FROM cluster_capabilities k \
     JOIN clusters c ON c.id = k.cluster_id AND c.org_id = k.org_id \
     WHERE c.org_id = $1 AND c.name = $2";
const ORGS: &str = "SELECT id FROM organizations ORDER BY id";

/// The capabilities recorded for a cluster.
#[derive(Clone, Debug, PartialEq)]
pub struct CapabilityRecord {
    pub facts: Value,
    /// When they were observed, in milliseconds since the epoch.
    pub observed_at: i64,
}

impl Tenant {
    /// Record `facts` observed at `observed_at` for this organization's
    /// cluster `cluster`. False when the organization has no such cluster or
    /// a newer observation is recorded already.
    pub async fn record_cluster_capabilities(
        &mut self,
        cluster: &str,
        facts: &Value,
        observed_at: i64,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(RECORD)
            .bind(self.org.to_string())
            .bind(cluster)
            .bind(facts.to_string())
            .bind(observed_at)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The capabilities recorded for this organization's cluster `cluster`.
    pub async fn cluster_capabilities(
        &mut self,
        cluster: &str,
    ) -> Result<Option<CapabilityRecord>, StoreError> {
        let row: Option<(String, i64)> = sqlx::query_as(READ)
            .bind(self.org.to_string())
            .bind(cluster)
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|(facts, observed_at)| {
            Ok(CapabilityRecord {
                facts: serde_json::from_str(&facts).map_err(|e| sqlx::Error::Decode(e.into()))?,
                observed_at,
            })
        })
        .transpose()
    }
}

impl Store {
    /// Every organization, for workers that act on each in turn.
    pub async fn org_ids(&self) -> Result<Vec<OrgId>, StoreError> {
        let ids: Vec<String> = sqlx::query_scalar(ORGS).fetch_all(self.pool()).await?;
        ids.iter()
            .map(|id| {
                id.parse()
                    .map_err(|e: uuid::Error| StoreError::from(sqlx::Error::Decode(e.into())))
            })
            .collect()
    }

    /// Record `facts` for the cluster named `cluster` in every organization
    /// that has one (the installation's own cluster is `primary` in each).
    /// Returns how many rows changed.
    pub async fn record_capabilities_everywhere(
        &self,
        cluster: &str,
        facts: &Value,
        observed_at: i64,
    ) -> Result<usize, StoreError> {
        let mut recorded = 0;
        for org in self.org_ids().await? {
            let mut tenant = self.tenant(org).await?;
            if tenant
                .record_cluster_capabilities(cluster, facts, observed_at)
                .await?
            {
                recorded += 1;
            }
            tenant.commit().await?;
        }
        Ok(recorded)
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::testing::{pg_store, skip};

    #[tokio::test]
    async fn capabilities_are_recorded_per_organization_and_never_backwards() {
        let Some(store) = pg_store().await else {
            skip("cluster capabilities");
            return;
        };
        let a = store.create_org("cap-a", "A").await.expect("org a").id;
        let b = store.create_org("cap-b", "B").await.expect("org b").id;
        let mut t = store.tenant(a).await.expect("tenant a");
        t.create_cluster("primary").await.expect("cluster");
        t.commit().await.expect("commit");

        let first = json!({ "certManager": true });
        // Only A has a `primary` cluster.
        assert_eq!(
            store
                .record_capabilities_everywhere("primary", &first, 10)
                .await
                .expect("record"),
            1
        );
        let mut t = store.tenant(a).await.expect("tenant a");
        let older = json!({ "certManager": false });
        assert!(
            !t.record_cluster_capabilities("primary", &older, 5)
                .await
                .expect("older"),
            "an older observation never replaces a newer one"
        );
        assert!(
            !t.record_cluster_capabilities("edge", &first, 20)
                .await
                .expect("unknown cluster")
        );
        assert_eq!(
            t.cluster_capabilities("primary").await.expect("read"),
            Some(CapabilityRecord {
                facts: first.clone(),
                observed_at: 10
            })
        );
        let newer = json!({ "certManager": true, "metricsApi": true });
        assert!(
            t.record_cluster_capabilities("primary", &newer, 30)
                .await
                .expect("newer")
        );
        assert_eq!(
            t.cluster_capabilities("primary")
                .await
                .expect("read")
                .map(|r| r.facts),
            Some(newer)
        );
        t.commit().await.expect("commit");

        // Row-level security: B sees nothing of A's cluster.
        let mut t = store.tenant(b).await.expect("tenant b");
        assert_eq!(t.cluster_capabilities("primary").await.expect("read b"), None);
        t.commit().await.expect("commit");
        let orgs = store.org_ids().await.expect("orgs");
        assert!(orgs.contains(&a) && orgs.contains(&b));
    }
}

//! Hourly usage of apps (M5.5, migration 0034).

use kuben_core::ids::TargetId;
use uuid::Uuid;

use super::Tenant;
use crate::StoreError;

const TARGET_BY_NAME: &str = "SELECT t.id FROM application_targets t \
     JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id \
     JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id \
     WHERE t.org_id = $1 AND pl.namespace = $2 AND a.slug = $3 AND t.deleted_at IS NULL \
     LIMIT 1";
const KEEP: &str = "INSERT INTO usage_rollups \
     (target_id, org_id, hour, cpu_avg, cpu_max, memory_avg, memory_max, samples) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8) \
     ON CONFLICT (target_id, hour) DO UPDATE SET \
       cpu_avg = CASE WHEN EXCLUDED.samples > usage_rollups.samples THEN EXCLUDED.cpu_avg ELSE usage_rollups.cpu_avg END, \
       memory_avg = CASE WHEN EXCLUDED.samples > usage_rollups.samples THEN EXCLUDED.memory_avg ELSE usage_rollups.memory_avg END, \
       cpu_max = GREATEST(usage_rollups.cpu_max, EXCLUDED.cpu_max), \
       memory_max = GREATEST(usage_rollups.memory_max, EXCLUDED.memory_max), \
       samples = GREATEST(usage_rollups.samples, EXCLUDED.samples)";
const SINCE: &str = "SELECT hour, cpu_avg, cpu_max, memory_avg, memory_max, samples FROM usage_rollups \
     WHERE org_id = $1 AND target_id = $2 AND hour >= $3 ORDER BY hour";

/// An hour of an app's usage, as stored.
#[derive(Clone, Copy, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct UsageHour {
    pub hour: i64,
    pub cpu_avg: i64,
    pub cpu_max: i64,
    pub memory_avg: i64,
    pub memory_max: i64,
    pub samples: i32,
}

fn signed(value: u64) -> Result<i64, sqlx::Error> {
    i64::try_from(value).map_err(|e| sqlx::Error::Encode(e.into()))
}

impl Tenant {
    /// The live app `slug` in `namespace`.
    pub async fn target_by_name(
        &mut self,
        namespace: &str,
        slug: &str,
    ) -> Result<Option<TargetId>, StoreError> {
        let id: Option<Uuid> = sqlx::query_scalar(TARGET_BY_NAME)
            .bind(self.org.to_string())
            .bind(namespace)
            .bind(slug)
            .fetch_optional(&mut *self.tx)
            .await?;
        Ok(id.map(TargetId::from_uuid))
    }

    /// Keep an hour of `target`'s usage: `(hour, cpu avg, cpu max, memory
    /// avg, memory max, samples)`.
    pub async fn keep_usage(
        &mut self,
        target: TargetId,
        (hour, cpu_avg, cpu_max, memory_avg, memory_max, samples): (i64, u64, u64, u64, u64, u32),
    ) -> Result<(), StoreError> {
        sqlx::query(KEEP)
            .bind(*target.as_uuid())
            .bind(self.org.to_string())
            .bind(hour)
            .bind(signed(cpu_avg)?)
            .bind(signed(cpu_max)?)
            .bind(signed(memory_avg)?)
            .bind(signed(memory_max)?)
            .bind(i32::try_from(samples).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// `target`'s hours since `since`, oldest first.
    pub async fn usage_since(&mut self, target: TargetId, since: i64) -> Result<Vec<UsageHour>, StoreError> {
        Ok(sqlx::query_as(SINCE)
            .bind(self.org.to_string())
            .bind(*target.as_uuid())
            .bind(since)
            .fetch_all(&mut *self.tx)
            .await?)
    }
}

#[cfg(test)]
mod tests {
    use crate::testing::{pg_store, skip};

    const HOUR: i64 = 3_600_000;

    #[tokio::test]
    async fn hours_merge_and_are_found_by_name() {
        let Some(store) = pg_store().await else {
            return skip("hours_merge_and_are_found_by_name");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "dev", "Dev", false)
            .await
            .expect("env");
        let cluster = t.create_cluster("primary").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, "kb-shop-dev")
            .await
            .expect("placement");
        let application = t.create_application(project, "web", "Web").await.expect("app");
        let target = t
            .create_target(project, application, placement)
            .await
            .expect("target");
        assert_eq!(
            t.target_by_name("kb-shop-dev", "web").await.expect("find"),
            Some(target)
        );
        assert_eq!(t.target_by_name("kb-shop-dev", "api").await.expect("find"), None);
        t.keep_usage(target, (HOUR, 100, 200, 1_000, 2_000, 10))
            .await
            .expect("keep");
        t.keep_usage(target, (HOUR, 50, 300, 500, 1_500, 5))
            .await
            .expect("keep");
        t.keep_usage(target, (2 * HOUR, 1, 1, 1, 1, 1))
            .await
            .expect("keep");
        let hours = t.usage_since(target, 0).await.expect("read");
        assert_eq!(hours.len(), 2);
        assert_eq!(
            (
                hours[0].cpu_avg,
                hours[0].cpu_max,
                hours[0].memory_max,
                hours[0].samples
            ),
            (100, 300, 2_000, 10),
            "the fuller sample's averages, the higher peaks"
        );
        assert_eq!(t.usage_since(target, 2 * HOUR).await.expect("read").len(), 1);
    }
}

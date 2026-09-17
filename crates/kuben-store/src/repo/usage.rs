//! What an organization's apps may request, for admission (M4.5).
//!
//! Admission reads the newest configuration of every live target: that is
//! what the next run of each renders. Resources are computed from it with
//! the platform's size presets outside the store.

use kuben_core::ids::{EnvironmentId, TargetId};
use serde_json::Value;
use uuid::Uuid;

use super::{Tenant, product::counter};
use crate::StoreError;

const LIVE_CONFIGS: &str = "SELECT t.id, p.environment_id, c.config::text AS config \
     FROM application_targets t \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id \
     LEFT JOIN LATERAL (SELECT r.config FROM target_config_revisions r \
       WHERE r.target_id = t.id AND r.org_id = t.org_id ORDER BY r.revision DESC LIMIT 1) c ON TRUE \
     WHERE t.org_id = $1 AND NOT t.deleting";
const LIVE_ENVIRONMENTS: &str = "SELECT count(*) FROM environments \
     WHERE org_id = $1 AND deleted_at IS NULL AND NOT deleting";

/// A live target and its newest configuration, if it has one.
#[derive(Clone, Debug, PartialEq)]
pub struct LiveConfig {
    pub target: TargetId,
    pub environment: EnvironmentId,
    pub config: Option<Value>,
}

impl Tenant {
    /// Every live target of the organization with its newest configuration.
    pub async fn live_configs(&mut self) -> Result<Vec<LiveConfig>, StoreError> {
        let rows: Vec<(Uuid, Uuid, Option<String>)> = sqlx::query_as(LIVE_CONFIGS)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?;
        rows.into_iter()
            .map(|(target, environment, config)| {
                Ok(LiveConfig {
                    target: TargetId::from_uuid(target),
                    environment: EnvironmentId::from_uuid(environment),
                    config: config
                        .as_deref()
                        .map(serde_json::from_str)
                        .transpose()
                        .map_err(|e| sqlx::Error::Decode(e.into()))?,
                })
            })
            .collect()
    }

    /// The organization's environments that are not being deleted.
    pub async fn live_environment_count(&mut self) -> Result<u64, StoreError> {
        let count: i64 = sqlx::query_scalar(LIVE_ENVIRONMENTS)
            .bind(self.org.to_string())
            .fetch_one(&mut *self.tx)
            .await?;
        Ok(counter(count)?)
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::testing::{pg_store, skip};

    #[tokio::test]
    async fn live_targets_and_environments_are_counted_per_organization() {
        let Some(store) = pg_store().await else {
            return skip("live_targets_and_environments_are_counted_per_organization");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let other = store.create_org("b", "B").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let environment = t
            .create_environment(project, "prod", "Prod", false)
            .await
            .expect("environment");
        t.create_environment(project, "staging", "Staging", false)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, environment, cluster, "a-shop")
            .await
            .expect("placement");
        let mut targets = Vec::new();
        for slug in ["web", "api"] {
            let application = t.create_application(project, slug, slug).await.expect("app");
            targets.push(
                t.create_target(project, application, placement)
                    .await
                    .expect("target"),
            );
        }
        for config in [
            json!({ "env": [] }),
            json!({ "env": [{ "name": "A", "value": "1" }] }),
        ] {
            t.create_config_revision(project, targets[0], &config, "user:a")
                .await
                .expect("revision")
                .expect("target");
        }
        assert!(t.mark_target_deleting(targets[1]).await.expect("delete"));
        let live = t.live_configs().await.expect("read");
        assert_eq!(
            live,
            [LiveConfig {
                target: targets[0],
                environment,
                config: Some(json!({ "env": [{ "name": "A", "value": "1" }] })),
            }],
            "the newest configuration of live targets only"
        );
        assert_eq!(t.live_environment_count().await.expect("count"), 2);
        t.commit().await.expect("commit");

        let mut t = store.tenant(other).await.expect("tenant");
        assert!(t.live_configs().await.expect("read").is_empty());
        assert_eq!(t.live_environment_count().await.expect("count"), 0);
    }
}

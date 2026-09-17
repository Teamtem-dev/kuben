//! Incidents, webhook endpoints and deliveries, and the context of settled
//! operations (M4.10, migration 0028).

use kuben_core::{
    ids::{EnvironmentId, OperationId, OrgId, ProjectId, TargetId},
    time::now_ms,
};
use serde_json::Value;
use uuid::Uuid;

use super::{SealedBytes, Tenant};
use crate::{Store, StoreError};

const OPEN_INCIDENT: &str = "INSERT INTO incidents \
     (id, org_id, project_id, environment_id, target_id, kind, severity, dedupe_key, title, detail, \
      opened_at, last_seen_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11) \
     ON CONFLICT (org_id, dedupe_key) WHERE resolved_at IS NULL DO UPDATE \
     SET last_seen_at = EXCLUDED.last_seen_at, occurrences = incidents.occurrences + 1, \
         detail = EXCLUDED.detail, severity = EXCLUDED.severity \
     RETURNING id, (xmax = 0) AS opened";
const RESOLVE_KEY: &str = "UPDATE incidents SET resolved_at = $3, resolved_by = $4 \
     WHERE org_id = $1 AND dedupe_key = $2 AND resolved_at IS NULL RETURNING id";
const RESOLVE_ID: &str = "UPDATE incidents SET resolved_at = $3, resolved_by = $4 \
     WHERE org_id = $1 AND id = $2 AND resolved_at IS NULL";
const ACKNOWLEDGE: &str = "UPDATE incidents SET acknowledged_at = $3, acknowledged_by = $4 \
     WHERE org_id = $1 AND id = $2 AND resolved_at IS NULL AND acknowledged_at IS NULL";
const INCIDENTS: &str = "SELECT id, project_id, environment_id, target_id, kind, severity, dedupe_key, title, \
     detail, opened_at, last_seen_at, occurrences, acknowledged_at, acknowledged_by, resolved_at, resolved_by \
     FROM incidents WHERE org_id = $1 AND ($2 OR resolved_at IS NULL) ORDER BY opened_at DESC LIMIT $3";
const INSERT_ENDPOINT: &str = "INSERT INTO webhook_endpoints \
     (id, org_id, name, url, events, secret, wrapped_key, key_version, created_by, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)";
const ENDPOINTS: &str = "SELECT id, name, url, events, created_by, created_at, disabled_at, failures \
     FROM webhook_endpoints WHERE org_id = $1 ORDER BY name";
const DISABLE_ENDPOINT: &str = "UPDATE webhook_endpoints SET disabled_at = $3 \
     WHERE org_id = $1 AND id = $2 AND disabled_at IS NULL";
const SUBSCRIBED: &str = "SELECT id FROM webhook_endpoints \
     WHERE org_id = $1 AND disabled_at IS NULL AND ($2 = ANY(events) OR '*' = ANY(events))";
const ENDPOINT_SECRET: &str = "SELECT url, secret, wrapped_key, key_version FROM webhook_endpoints \
     WHERE org_id = $1 AND id = $2 AND disabled_at IS NULL";
const ENQUEUE: &str = "INSERT INTO webhook_deliveries \
     (id, org_id, endpoint_id, event_id, event, payload, next_attempt_at, created_at) \
     VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $7) ON CONFLICT (endpoint_id, event_id) DO NOTHING";
const TAKE_DELIVERIES: &str = "UPDATE webhook_deliveries SET next_attempt_at = $2 + $3, attempts = attempts + 1 \
     WHERE id IN (SELECT id FROM webhook_deliveries \
                  WHERE status = 'pending' AND next_attempt_at <= $2 \
                  ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT $1) \
     RETURNING id, org_id, endpoint_id, event_id, event, payload::text AS payload, attempts, created_at";
const DELIVERED: &str = "UPDATE webhook_deliveries \
     SET status = 'delivered', last_status = $2, last_error = NULL, finished_at = $3 WHERE id = $1";
const RESET_FAILURES: &str = "UPDATE webhook_endpoints SET failures = 0 \
     WHERE id = (SELECT endpoint_id FROM webhook_deliveries WHERE id = $1)";
const RETRY_DELIVERY: &str = "UPDATE webhook_deliveries \
     SET next_attempt_at = $2, last_status = $3, last_error = $4 WHERE id = $1 AND status = 'pending'";
const GIVE_UP: &str = "UPDATE webhook_deliveries \
     SET status = 'failed', last_status = $2, last_error = $3, finished_at = $4 WHERE id = $1";
const COUNT_FAILURE: &str = "UPDATE webhook_endpoints SET failures = failures + 1, \
     disabled_at = CASE WHEN failures + 1 >= $2 THEN $3 ELSE disabled_at END \
     WHERE id = (SELECT endpoint_id FROM webhook_deliveries WHERE id = $1)";
const RETRY_FAILED: &str = "UPDATE webhook_deliveries d SET status = 'pending', next_attempt_at = $4, \
     attempts = 0, finished_at = NULL, last_error = NULL \
     FROM webhook_endpoints e \
     WHERE d.id = $3 AND d.endpoint_id = $2 AND d.org_id = $1 AND d.status = 'failed' \
       AND e.id = d.endpoint_id AND e.org_id = d.org_id AND e.disabled_at IS NULL";
const DELIVERIES: &str = "SELECT id, event, status, attempts, last_status, last_error, created_at, finished_at \
     FROM webhook_deliveries WHERE org_id = $1 AND endpoint_id = $2 ORDER BY created_at DESC LIMIT $3";
const RUN_CONTEXT: &str = "SELECT r.project_id, p.environment_id, r.target_id, pr.slug AS project, \
     e.slug AS environment, a.slug AS app, r.reason, r.generation, rel.source::text AS source, \
     b.installation_id \
     FROM deployment_runs r \
     JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = r.org_id \
     JOIN environments e ON e.id = p.environment_id AND e.org_id = r.org_id \
     JOIN projects pr ON pr.id = r.project_id AND pr.org_id = r.org_id \
     JOIN applications a ON a.id = t.application_id AND a.org_id = r.org_id \
     JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id \
     LEFT JOIN source_bindings b ON b.target_id = r.target_id AND b.org_id = r.org_id \
     WHERE r.operation_id = $1 AND r.org_id = $2";
const BUILD_CONTEXT: &str = "SELECT ba.project_id, p.environment_id, ba.target_id, pr.slug AS project, \
     e.slug AS environment, a.slug AS app, ba.phase AS reason, 0::bigint AS generation, \
     jsonb_build_object('repository', ba.repository, 'commit', ba.commit_sha)::text AS source, \
     b.installation_id \
     FROM build_attempts ba \
     JOIN application_targets t ON t.id = ba.target_id AND t.org_id = ba.org_id \
     JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = ba.org_id \
     JOIN environments e ON e.id = p.environment_id AND e.org_id = ba.org_id \
     JOIN projects pr ON pr.id = ba.project_id AND pr.org_id = ba.org_id \
     JOIN applications a ON a.id = ba.application_id AND a.org_id = ba.org_id \
     JOIN source_bindings b ON b.id = ba.binding_id AND b.org_id = ba.org_id \
     WHERE ba.operation_id = $1 AND ba.org_id = $2";

/// Consecutive given-up deliveries after which an endpoint is disabled.
pub const MAX_ENDPOINT_FAILURES: i32 = 20;

/// An incident to open (or to count again).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NewIncident {
    pub project: Option<ProjectId>,
    pub environment: Option<EnvironmentId>,
    pub target: Option<TargetId>,
    pub kind: String,
    /// `critical`, `warning` or `info`.
    pub severity: &'static str,
    pub dedupe_key: String,
    pub title: String,
    pub detail: Option<String>,
}

/// An incident.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct Incident {
    pub id: Uuid,
    pub project_id: Option<Uuid>,
    pub environment_id: Option<Uuid>,
    pub target_id: Option<Uuid>,
    pub kind: String,
    pub severity: String,
    pub dedupe_key: String,
    pub title: String,
    pub detail: Option<String>,
    pub opened_at: i64,
    pub last_seen_at: i64,
    pub occurrences: i64,
    pub acknowledged_at: Option<i64>,
    pub acknowledged_by: Option<String>,
    pub resolved_at: Option<i64>,
    pub resolved_by: Option<String>,
}

/// A webhook endpoint, without its secret.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct Endpoint {
    pub id: Uuid,
    pub name: String,
    pub url: String,
    pub events: Vec<String>,
    pub created_by: String,
    pub created_at: i64,
    pub disabled_at: Option<i64>,
    pub failures: i32,
}

/// A delivery handed to a worker.
#[derive(Clone, Debug, PartialEq)]
pub struct WebhookDelivery {
    pub id: Uuid,
    pub org: OrgId,
    pub endpoint: Uuid,
    pub event_id: Uuid,
    pub event: String,
    pub payload: Value,
    pub attempts: i32,
    pub created_at: i64,
}

/// A delivery as the API lists it.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct DeliveryRecord {
    pub id: Uuid,
    pub event: String,
    pub status: String,
    pub attempts: i32,
    pub last_status: Option<i32>,
    pub last_error: Option<String>,
    pub created_at: i64,
    pub finished_at: Option<i64>,
}

/// Where a settled operation happened.
#[derive(Clone, Debug, PartialEq, Eq, sqlx::FromRow)]
pub struct OperationContext {
    pub project_id: Uuid,
    pub environment_id: Uuid,
    pub target_id: Uuid,
    pub project: String,
    pub environment: String,
    pub app: String,
    /// A run's reason, or a build's phase.
    pub reason: String,
    pub generation: i64,
    /// The release's (or build's) source: repository and commit, if any.
    pub source: Option<String>,
    /// The GitHub App installation of the app's Git source, if any.
    pub installation_id: Option<i64>,
}

#[derive(sqlx::FromRow)]
struct DeliveryRow {
    id: Uuid,
    org_id: String,
    endpoint_id: Uuid,
    event_id: Uuid,
    event: String,
    payload: String,
    attempts: i32,
    created_at: i64,
}

impl Tenant {
    /// Open an incident, or count it again while one with its key is open.
    /// Its id, and whether it is new.
    pub async fn open_incident(&mut self, i: &NewIncident) -> Result<(Uuid, bool), StoreError> {
        let now = now_ms();
        Ok(sqlx::query_as(OPEN_INCIDENT)
            .bind(Uuid::now_v7())
            .bind(self.org.to_string())
            .bind(i.project.map(|p| *p.as_uuid()))
            .bind(i.environment.map(|e| *e.as_uuid()))
            .bind(i.target.map(|t| *t.as_uuid()))
            .bind(&i.kind)
            .bind(i.severity)
            .bind(&i.dedupe_key)
            .bind(&i.title)
            .bind(i.detail.as_deref())
            .bind(now)
            .fetch_one(&mut *self.tx)
            .await?)
    }

    /// Resolve the open incident with `dedupe_key`, if there is one.
    pub async fn resolve_incident_key(
        &mut self,
        dedupe_key: &str,
        by: &str,
    ) -> Result<Option<Uuid>, StoreError> {
        Ok(sqlx::query_scalar(RESOLVE_KEY)
            .bind(self.org.to_string())
            .bind(dedupe_key)
            .bind(now_ms())
            .bind(by)
            .fetch_optional(&mut *self.tx)
            .await?)
    }

    /// Resolve incident `id` by hand. False when it is resolved already.
    pub async fn resolve_incident(&mut self, id: Uuid, by: &str) -> Result<bool, StoreError> {
        self.touch_incident(RESOLVE_ID, id, by).await
    }

    /// Acknowledge incident `id`. False when it is resolved or acknowledged.
    pub async fn acknowledge_incident(&mut self, id: Uuid, by: &str) -> Result<bool, StoreError> {
        self.touch_incident(ACKNOWLEDGE, id, by).await
    }

    async fn touch_incident(&mut self, sql: &'static str, id: Uuid, by: &str) -> Result<bool, StoreError> {
        let rows = sqlx::query(sql)
            .bind(self.org.to_string())
            .bind(id)
            .bind(now_ms())
            .bind(by)
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// The newest `limit` incidents, open ones only unless `all`.
    pub async fn incidents(&mut self, all: bool, limit: i64) -> Result<Vec<Incident>, StoreError> {
        Ok(sqlx::query_as(INCIDENTS)
            .bind(self.org.to_string())
            .bind(all)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Add a webhook endpoint whose signing secret is `secret` (sealed for
    /// `id`, revision 0).
    pub async fn create_endpoint(
        &mut self,
        id: Uuid,
        name: &str,
        url: &str,
        events: &[String],
        secret: &SealedBytes,
        by: &str,
    ) -> Result<(), StoreError> {
        sqlx::query(INSERT_ENDPOINT)
            .bind(id)
            .bind(self.org.to_string())
            .bind(name)
            .bind(url)
            .bind(events)
            .bind(&secret.ciphertext)
            .bind(&secret.wrapped_key)
            .bind(i32::try_from(secret.key_version).map_err(|e| sqlx::Error::Encode(e.into()))?)
            .bind(by)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(())
    }

    /// The organization's endpoints.
    pub async fn endpoints(&mut self) -> Result<Vec<Endpoint>, StoreError> {
        Ok(sqlx::query_as(ENDPOINTS)
            .bind(self.org.to_string())
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Disable endpoint `id` for good. False when there is no enabled one.
    pub async fn disable_endpoint(&mut self, id: Uuid) -> Result<bool, StoreError> {
        let rows = sqlx::query(DISABLE_ENDPOINT)
            .bind(self.org.to_string())
            .bind(id)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Queue `event` for every enabled endpoint subscribed to it. The number
    /// queued (a repeated event id is queued once).
    pub async fn enqueue_event(
        &mut self,
        event_id: Uuid,
        event: &str,
        payload: &Value,
    ) -> Result<u64, StoreError> {
        let org = self.org.to_string();
        let endpoints: Vec<Uuid> = sqlx::query_scalar(SUBSCRIBED)
            .bind(&org)
            .bind(event)
            .fetch_all(&mut *self.tx)
            .await?;
        let mut queued = 0;
        let text = payload.to_string();
        for endpoint in endpoints {
            queued += sqlx::query(ENQUEUE)
                .bind(Uuid::now_v7())
                .bind(&org)
                .bind(endpoint)
                .bind(event_id)
                .bind(event)
                .bind(&text)
                .bind(now_ms())
                .execute(&mut *self.tx)
                .await?
                .rows_affected();
        }
        Ok(queued)
    }

    /// Queue `event` for the enabled endpoint `endpoint` alone (a ping).
    /// False when it is not enabled.
    pub async fn enqueue_to(
        &mut self,
        endpoint: Uuid,
        event: &str,
        payload: &Value,
    ) -> Result<bool, StoreError> {
        if self.endpoint_secret(endpoint).await?.is_none() {
            return Ok(false);
        }
        sqlx::query(ENQUEUE)
            .bind(Uuid::now_v7())
            .bind(self.org.to_string())
            .bind(endpoint)
            .bind(Uuid::now_v7())
            .bind(event)
            .bind(payload.to_string())
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?;
        Ok(true)
    }

    /// The URL and sealed secret of the enabled endpoint `id`.
    pub async fn endpoint_secret(&mut self, id: Uuid) -> Result<Option<(String, SealedBytes)>, StoreError> {
        let row: Option<(String, Vec<u8>, Vec<u8>, i32)> = sqlx::query_as(ENDPOINT_SECRET)
            .bind(self.org.to_string())
            .bind(id)
            .fetch_optional(&mut *self.tx)
            .await?;
        row.map(|(url, ciphertext, wrapped_key, version)| {
            Ok((
                url,
                SealedBytes {
                    ciphertext,
                    wrapped_key,
                    key_version: u32::try_from(version).map_err(|e| sqlx::Error::Decode(e.into()))?,
                },
            ))
        })
        .transpose()
    }

    /// The newest `limit` deliveries to endpoint `id`.
    pub async fn deliveries(&mut self, id: Uuid, limit: i64) -> Result<Vec<DeliveryRecord>, StoreError> {
        Ok(sqlx::query_as(DELIVERIES)
            .bind(self.org.to_string())
            .bind(id)
            .bind(limit)
            .fetch_all(&mut *self.tx)
            .await?)
    }

    /// Try failed delivery `delivery` of enabled endpoint `endpoint` again,
    /// now. False when it is not a failed delivery of an enabled endpoint.
    pub async fn retry_delivery(&mut self, endpoint: Uuid, delivery: Uuid) -> Result<bool, StoreError> {
        let rows = sqlx::query(RETRY_FAILED)
            .bind(self.org.to_string())
            .bind(endpoint)
            .bind(delivery)
            .bind(now_ms())
            .execute(&mut *self.tx)
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Where the settled operation `operation` happened: a deployment run's
    /// or (with `build`) a build's target, names and source.
    pub async fn operation_context(
        &mut self,
        operation: OperationId,
        build: bool,
    ) -> Result<Option<OperationContext>, StoreError> {
        Ok(sqlx::query_as(if build { BUILD_CONTEXT } else { RUN_CONTEXT })
            .bind(*operation.as_uuid())
            .bind(self.org.to_string())
            .fetch_optional(&mut *self.tx)
            .await?)
    }
}

impl Store {
    /// Hand out up to `limit` due deliveries, hidden for `lease_ms`.
    pub async fn take_deliveries(
        &self,
        limit: i64,
        lease_ms: i64,
    ) -> Result<Vec<WebhookDelivery>, StoreError> {
        let rows: Vec<DeliveryRow> = sqlx::query_as(TAKE_DELIVERIES)
            .bind(limit)
            .bind(now_ms())
            .bind(lease_ms)
            .fetch_all(self.pool())
            .await?;
        rows.into_iter()
            .map(|r| {
                Ok(WebhookDelivery {
                    id: r.id,
                    org: r
                        .org_id
                        .parse()
                        .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))?,
                    endpoint: r.endpoint_id,
                    event_id: r.event_id,
                    event: r.event,
                    payload: serde_json::from_str(&r.payload).map_err(|e| sqlx::Error::Decode(e.into()))?,
                    attempts: r.attempts,
                    created_at: r.created_at,
                })
            })
            .collect()
    }

    /// Delivery `id` was accepted with HTTP `status`.
    pub async fn delivery_succeeded(&self, id: Uuid, status: i32) -> Result<(), StoreError> {
        let mut tx = self.pool().begin().await?;
        sqlx::query(DELIVERED)
            .bind(id)
            .bind(status)
            .bind(now_ms())
            .execute(&mut *tx)
            .await?;
        sqlx::query(RESET_FAILURES).bind(id).execute(&mut *tx).await?;
        tx.commit().await?;
        Ok(())
    }

    /// Delivery `id` failed: try again at `retry_at`, or, without one, give
    /// it up and count the failure against its endpoint.
    pub async fn delivery_failed(
        &self,
        id: Uuid,
        status: Option<i32>,
        error: &str,
        retry_at: Option<i64>,
    ) -> Result<(), StoreError> {
        let error: String = error.chars().take(1024).collect();
        if let Some(at) = retry_at {
            sqlx::query(RETRY_DELIVERY)
                .bind(id)
                .bind(at)
                .bind(status)
                .bind(&error)
                .execute(self.pool())
                .await?;
            return Ok(());
        }
        let now = now_ms();
        let mut tx = self.pool().begin().await?;
        sqlx::query(GIVE_UP)
            .bind(id)
            .bind(status)
            .bind(&error)
            .bind(now)
            .execute(&mut *tx)
            .await?;
        sqlx::query(COUNT_FAILURE)
            .bind(id)
            .bind(MAX_ENDPOINT_FAILURES)
            .bind(now)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::testing::{pg_store, skip};

    const BY: &str = "user:a";

    fn incident(key: &str, detail: &str) -> NewIncident {
        NewIncident {
            project: None,
            environment: None,
            target: None,
            kind: "backup.stale".into(),
            severity: "critical",
            dedupe_key: key.into(),
            title: "Backups are stale".into(),
            detail: Some(detail.into()),
        }
    }

    fn sealed() -> SealedBytes {
        SealedBytes {
            ciphertext: vec![1, 2, 3],
            wrapped_key: vec![4, 5],
            key_version: 1,
        }
    }

    #[tokio::test]
    async fn incidents_are_deduplicated_acknowledged_and_resolved() {
        let Some(store) = pg_store().await else {
            return skip("incidents_are_deduplicated_acknowledged_and_resolved");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let other = store.create_org("b", "B").await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let (first, opened) = t.open_incident(&incident("k", "one")).await.expect("open");
        assert!(opened);
        let (again, opened) = t.open_incident(&incident("k", "two")).await.expect("open");
        assert_eq!((again, opened), (first, false), "counted again");
        let open = t.incidents(false, 10).await.expect("read");
        assert_eq!(open.len(), 1);
        assert_eq!((open[0].occurrences, open[0].detail.as_deref()), (2, Some("two")));
        assert!(t.acknowledge_incident(first, BY).await.expect("ack"));
        assert!(!t.acknowledge_incident(first, BY).await.expect("ack"), "once");
        assert_eq!(
            t.resolve_incident_key("k", "system").await.expect("resolve"),
            Some(first)
        );
        assert_eq!(
            t.resolve_incident_key("k", "system").await.expect("resolve"),
            None
        );
        assert!(
            !t.resolve_incident(first, BY).await.expect("resolve"),
            "resolved already"
        );
        assert!(t.incidents(false, 10).await.expect("read").is_empty());
        let (reopened, opened) = t.open_incident(&incident("k", "three")).await.expect("open");
        assert!(opened && reopened != first, "a new incident after resolution");
        assert!(t.resolve_incident(reopened, BY).await.expect("resolve"));
        let all = t.incidents(true, 10).await.expect("read");
        assert_eq!(all.len(), 2);
        assert!(all.iter().all(|i| i.resolved_at.is_some()));
        t.commit().await.expect("commit");
        let mut t = store.tenant(other).await.expect("tenant");
        assert!(t.incidents(true, 10).await.expect("read").is_empty(), "isolated");
        assert!(
            !t.acknowledge_incident(first, BY).await.expect("ack"),
            "not theirs"
        );
    }

    #[tokio::test]
    async fn deliveries_are_queued_once_retried_and_given_up() {
        let Some(store) = pg_store().await else {
            return skip("deliveries_are_queued_once_retried_and_given_up");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let (all, deploys) = (Uuid::now_v7(), Uuid::now_v7());
        let mut t = store.tenant(org).await.expect("tenant");
        t.create_endpoint(all, "all", "https://a.example/hook", &["*".into()], &sealed(), BY)
            .await
            .expect("create");
        let events = ["deployment.succeeded".to_owned()];
        t.create_endpoint(
            deploys,
            "deploys",
            "https://b.example/hook",
            &events,
            &sealed(),
            BY,
        )
        .await
        .expect("create");
        let event = Uuid::now_v7();
        let payload = json!({"hello": "world"});
        assert_eq!(
            t.enqueue_event(event, "deployment.succeeded", &payload)
                .await
                .expect("queue"),
            2
        );
        assert_eq!(
            t.enqueue_event(event, "deployment.succeeded", &payload)
                .await
                .expect("queue"),
            0
        );
        assert_eq!(
            t.enqueue_event(Uuid::now_v7(), "build.failed", &payload)
                .await
                .expect("queue"),
            1
        );
        let (url, secret) = t.endpoint_secret(deploys).await.expect("read").expect("enabled");
        assert_eq!((url.as_str(), secret), ("https://b.example/hook", sealed()));
        t.commit().await.expect("commit");

        let taken = store.take_deliveries(10, 60_000).await.expect("take");
        assert_eq!(taken.len(), 3);
        assert!(taken.iter().all(|d| d.org == org && d.attempts == 1));
        assert!(
            store.take_deliveries(10, 60_000).await.expect("take").is_empty(),
            "leased"
        );
        let to = |endpoint: Uuid| taken.iter().filter(move |d| d.endpoint == endpoint);
        let ok = to(deploys).next().expect("one");
        assert_eq!(ok.payload, payload);
        store.delivery_succeeded(ok.id, 204).await.expect("record");
        let mut rest = to(all);
        let (retried, failed) = (rest.next().expect("one"), rest.next().expect("two"));
        store
            .delivery_failed(retried.id, Some(503), "unavailable", Some(0))
            .await
            .expect("record");
        store
            .delivery_failed(failed.id, None, "refused", None)
            .await
            .expect("record");
        let again = store.take_deliveries(10, 60_000).await.expect("take");
        assert_eq!(
            again.iter().map(|d| (d.id, d.attempts)).collect::<Vec<_>>(),
            [(retried.id, 2)]
        );

        let mut t = store.tenant(org).await.expect("tenant");
        let records = t.deliveries(all, 10).await.expect("read");
        let failed = records.iter().find(|r| r.id == failed.id).expect("listed");
        assert_eq!(
            (failed.status.as_str(), failed.last_error.as_deref()),
            ("failed", Some("refused"))
        );
        let endpoints = t.endpoints().await.expect("read");
        assert_eq!(
            endpoints
                .iter()
                .map(|e| (e.name.as_str(), e.failures))
                .collect::<Vec<_>>(),
            [("all", 1), ("deploys", 0)]
        );
        assert!(t.disable_endpoint(all).await.expect("disable"));
        assert!(!t.disable_endpoint(all).await.expect("disable"), "once");
        assert!(
            !t.enqueue_to(all, "ping", &payload).await.expect("ping"),
            "disabled"
        );
        assert!(t.enqueue_to(deploys, "ping", &payload).await.expect("ping"));
        assert_eq!(
            t.enqueue_event(Uuid::now_v7(), "build.failed", &payload)
                .await
                .expect("queue"),
            0
        );
        t.commit().await.expect("commit");
    }

    #[tokio::test]
    async fn endpoints_are_disabled_after_repeated_failures() {
        let Some(store) = pg_store().await else {
            return skip("endpoints_are_disabled_after_repeated_failures");
        };
        let org = store.create_org("a", "A").await.expect("org").id;
        let id = Uuid::now_v7();
        let mut t = store.tenant(org).await.expect("tenant");
        t.create_endpoint(
            id,
            "flaky",
            "https://a.example/hook",
            &["*".into()],
            &sealed(),
            BY,
        )
        .await
        .expect("create");
        for _ in 0..MAX_ENDPOINT_FAILURES {
            t.enqueue_event(Uuid::now_v7(), "build.failed", &json!({}))
                .await
                .expect("queue");
        }
        t.commit().await.expect("commit");
        let taken = store.take_deliveries(100, 60_000).await.expect("take");
        assert_eq!(taken.len(), usize::try_from(MAX_ENDPOINT_FAILURES).expect("size"));
        store
            .delivery_failed(taken[0].id, Some(500), "boom", None)
            .await
            .expect("record");
        let mut t = store.tenant(org).await.expect("tenant");
        assert!(
            t.retry_delivery(id, taken[0].id).await.expect("retry"),
            "a failed delivery goes again"
        );
        assert!(!t.retry_delivery(id, taken[0].id).await.expect("retry"), "once");
        t.commit().await.expect("commit");
        let again = store.take_deliveries(100, 60_000).await.expect("take");
        assert_eq!(
            again.iter().map(|d| (d.id, d.attempts)).collect::<Vec<_>>(),
            [(taken[0].id, 1)]
        );
        for d in &taken {
            store
                .delivery_failed(d.id, Some(500), "boom", None)
                .await
                .expect("record");
        }
        let mut t = store.tenant(org).await.expect("tenant");
        let endpoint = t.endpoints().await.expect("read").pop().expect("one");
        assert!(endpoint.disabled_at.is_some());
        assert_eq!(endpoint.failures, MAX_ENDPOINT_FAILURES);
        assert!(t.endpoint_secret(id).await.expect("read").is_none());
    }
}

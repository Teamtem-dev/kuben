//! Durable operations (plan §8.2, §9.2, §9.3): transactional acceptance with
//! idempotency receipts, claims with a lease and a fence, the outbox and the
//! inbox.
//!
//! Acceptance runs in the caller's [`Tenant`] transaction. Workers are not
//! bound to one organization: their claims and writes go through [`Store`]
//! and are conditional on the fence of their claim (I06).

use std::time::Duration;

use kuben_core::{
    ids::{OperationId, OrgId, ProjectId, TargetId},
    time::now_ms,
};
use serde_json::json;
use uuid::Uuid;

use super::{NewAudit, Tenant};
use crate::{Store, StoreError};

const UPSERT_RECEIPT: &str = "INSERT INTO idempotency_receipts \
     (org_id, actor, operation, key, request_hash, operation_id, created_at, expires_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8) \
     ON CONFLICT (org_id, actor, operation, key) DO UPDATE \
     SET request_hash = EXCLUDED.request_hash, operation_id = EXCLUDED.operation_id, \
         created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at \
     WHERE idempotency_receipts.expires_at <= EXCLUDED.created_at";
const SELECT_RECEIPT: &str = "SELECT request_hash, operation_id FROM idempotency_receipts \
     WHERE org_id = $1 AND actor = $2 AND operation = $3 AND key = $4";
const INSERT_OPERATION: &str = "INSERT INTO operations \
     (id, org_id, project_id, target_id, kind, lifecycle_uid, generation, input_hash, payload, \
      requested_by, requested_at, deadline_at, next_attempt_at) \
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, kuben_now_ms())";
const INSERT_OUTBOX: &str = "INSERT INTO outbox (id, org_id, operation_id, topic, payload, created_at, available_at) \
     VALUES ($1, $2, $3, $4, $5::jsonb, $6, kuben_now_ms())";
const CLAIM: &str = "UPDATE operations \
     SET lease_owner = $1, lease_until = kuben_now_ms() + $2, fence = fence + 1, attempt = attempt + 1 \
     WHERE id = (SELECT id FROM operations \
                 WHERE NOT done AND kind = ANY($3) AND next_attempt_at <= kuben_now_ms() \
                   AND (lease_until IS NULL OR lease_until < kuben_now_ms()) \
                 ORDER BY next_attempt_at, id \
                 FOR UPDATE SKIP LOCKED \
                 LIMIT 1) \
     RETURNING id, org_id, kind, attempt, fence";
/// Settle the operation and leave a `<kind>.settled` message (M4.10) in the
/// same statement.
const FINISH: &str = "WITH settled AS (UPDATE operations \
       SET phase = $3, done = TRUE, error_code = $4, lease_owner = NULL, lease_until = NULL, \
           last_progress_at = kuben_now_ms() \
       WHERE id = $1 AND fence = $2 AND NOT done \
       RETURNING id, org_id, kind) \
     INSERT INTO outbox (id, org_id, operation_id, topic, payload, created_at, available_at) \
     SELECT gen_random_uuid(), org_id, id, kind || '.settled', \
            jsonb_build_object('phase', $3::text, 'code', $4::text), kuben_now_ms(), kuben_now_ms() \
     FROM settled";
const RETRY: &str = "UPDATE operations \
     SET next_attempt_at = kuben_now_ms() + $3, error_code = $4, lease_owner = NULL, lease_until = NULL, \
         last_progress_at = kuben_now_ms() \
     WHERE id = $1 AND fence = $2 AND NOT done";
const RENEW: &str = "UPDATE operations \
     SET lease_until = kuben_now_ms() + $3, last_progress_at = kuben_now_ms() \
     WHERE id = $1 AND fence = $2 AND NOT done";
const NEXT_EVENT: &str = "UPDATE operations \
     SET event_seq = event_seq + 1, last_progress_at = kuben_now_ms() \
     WHERE id = $1 AND fence = $2 RETURNING event_seq";
const INSERT_EVENT: &str = "INSERT INTO operation_events (operation_id, seq, kind, data, at) \
     VALUES ($1, $2, $3, $4::jsonb, $5)";
const TAKE_OUTBOX: &str = "UPDATE outbox SET available_at = kuben_now_ms() + $2, attempts = attempts + 1 \
     WHERE id IN (SELECT id FROM outbox \
                  WHERE delivered_at IS NULL AND available_at <= kuben_now_ms() \
                  ORDER BY available_at, id \
                  FOR UPDATE SKIP LOCKED \
                  LIMIT $1) \
     RETURNING id, org_id, operation_id, topic, payload::text AS payload, attempts, created_at";
const OUTBOX_DELIVERED: &str =
    "UPDATE outbox SET delivered_at = kuben_now_ms() WHERE id = $1 AND delivered_at IS NULL";
const INSERT_INBOX: &str = "INSERT INTO inbox (provider, delivery_id, org_id, body_sha256, receipt, received_at) \
     VALUES ($1, $2, $3, sha256($4), $5, $6) \
     ON CONFLICT (provider, delivery_id) DO NOTHING \
     RETURNING receipt";
const SELECT_INBOX: &str = "SELECT receipt, org_id = $3 AND body_sha256 = sha256($4) FROM inbox \
     WHERE provider = $1 AND delivery_id = $2";

/// A request to accept as one durable operation.
#[derive(Clone, Debug)]
pub struct NewOperation {
    pub kind: String,
    /// The project and target the operation acts on, if any.
    pub target: Option<(ProjectId, TargetId)>,
    /// The target's lifecycle UID and generation the request was made for.
    pub lifecycle_uid: Option<Uuid>,
    pub generation: Option<u64>,
    /// Hash of the canonical request; idempotency compares it.
    pub input_hash: Vec<u8>,
    pub payload: serde_json::Value,
    pub requested_by: String,
    pub deadline_at: Option<i64>,
    /// Outbox topic that tells the executors about it.
    pub topic: String,
}

/// An `Idempotency-Key` and the caller that sent it (plan §8.2).
#[derive(Clone, Debug)]
pub struct IdempotencyKey {
    pub actor: String,
    pub key: String,
    /// How long the receipt is kept. The operation stays regardless.
    pub ttl: Duration,
}

/// The outcome of [`Tenant::accept`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Accepted {
    /// A new operation, committed with its audit record and outbox message.
    New(OperationId),
    /// The same key and request were accepted before: the same operation.
    Replayed(OperationId),
    /// The key was used for another request (HTTP 409); nothing was written.
    KeyReused(OperationId),
}

/// A claimed operation. Every write about it is conditional on `fence`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Claim {
    pub id: OperationId,
    pub org: OrgId,
    pub kind: String,
    pub attempt: i32,
    pub fence: i64,
}

/// A message handed out by [`Store::take_outbox`]; it comes back until it is
/// marked delivered.
#[derive(Clone, Debug, PartialEq)]
pub struct OutboxMessage {
    pub id: Uuid,
    pub org: OrgId,
    pub operation: Option<OperationId>,
    pub topic: String,
    pub payload: serde_json::Value,
    pub attempts: i32,
    /// When the message was written (Unix milliseconds).
    pub created_at: i64,
}

/// The outcome of [`Store::receive`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Received {
    New(Uuid),
    /// A redelivery: the receipt of the first delivery.
    Duplicate(Uuid),
    /// The same delivery id with another body or organization: an integrity
    /// error, never processed.
    Changed,
}

#[derive(sqlx::FromRow)]
struct ClaimRow {
    id: Uuid,
    org_id: String,
    kind: String,
    attempt: i32,
    fence: i64,
}

#[derive(sqlx::FromRow)]
struct OutboxRow {
    id: Uuid,
    org_id: String,
    operation_id: Option<Uuid>,
    topic: String,
    payload: String,
    attempts: i32,
    created_at: i64,
}

fn millis(d: Duration) -> i64 {
    i64::try_from(d.as_millis()).unwrap_or(i64::MAX)
}

fn org_id(value: &str) -> Result<OrgId, sqlx::Error> {
    value
        .parse()
        .map_err(|e: uuid::Error| sqlx::Error::Decode(e.into()))
}

impl Tenant {
    /// Accept `op` for this organization: its idempotency receipt, the
    /// operation, `audit` and an outbox message are written in this
    /// transaction and commit together (I01). Nothing reaches a cluster here.
    pub async fn accept(
        &mut self,
        op: &NewOperation,
        audit: NewAudit,
        idempotency: Option<&IdempotencyKey>,
    ) -> Result<Accepted, StoreError> {
        let id = OperationId::new();
        let now = now_ms();
        let org = self.org.to_string();
        if let Some(k) = idempotency {
            let written = sqlx::query(UPSERT_RECEIPT)
                .bind(&org)
                .bind(&k.actor)
                .bind(&op.kind)
                .bind(&k.key)
                .bind(&op.input_hash)
                .bind(*id.as_uuid())
                .bind(now)
                .bind(now.saturating_add(millis(k.ttl)))
                .execute(&mut *self.tx)
                .await?
                .rows_affected();
            if written == 0 {
                let (hash, existing): (Vec<u8>, Uuid) = sqlx::query_as(SELECT_RECEIPT)
                    .bind(&org)
                    .bind(&k.actor)
                    .bind(&op.kind)
                    .bind(&k.key)
                    .fetch_one(&mut *self.tx)
                    .await?;
                let existing = OperationId::from_uuid(existing);
                return Ok(if hash == op.input_hash {
                    Accepted::Replayed(existing)
                } else {
                    Accepted::KeyReused(existing)
                });
            }
        }
        let generation = op
            .generation
            .map(i64::try_from)
            .transpose()
            .map_err(|e| sqlx::Error::Encode(e.into()))?;
        sqlx::query(INSERT_OPERATION)
            .bind(*id.as_uuid())
            .bind(&org)
            .bind(op.target.map(|(project, _)| *project.as_uuid()))
            .bind(op.target.map(|(_, target)| *target.as_uuid()))
            .bind(&op.kind)
            .bind(op.lifecycle_uid)
            .bind(generation)
            .bind(&op.input_hash)
            .bind(op.payload.to_string())
            .bind(&op.requested_by)
            .bind(now)
            .bind(op.deadline_at)
            .execute(&mut *self.tx)
            .await?;
        NewAudit {
            org_id: Some(self.org),
            ..audit
        }
        .insert(&mut *self.tx)
        .await?;
        sqlx::query(INSERT_OUTBOX)
            .bind(Uuid::now_v7())
            .bind(&org)
            .bind(*id.as_uuid())
            .bind(&op.topic)
            .bind(json!({ "operation_id": id, "kind": op.kind }).to_string())
            .bind(now)
            .execute(&mut *self.tx)
            .await?;
        Ok(Accepted::New(id))
    }
}

impl Store {
    /// Claim the next due, unleased operation of one of `kinds` for `worker`,
    /// leased for `lease` by the database clock. The claim raises the fence.
    pub async fn claim_operation(
        &self,
        worker: &str,
        kinds: &[&str],
        lease: Duration,
    ) -> Result<Option<Claim>, StoreError> {
        let kinds: Vec<String> = kinds.iter().map(|k| (*k).to_owned()).collect();
        let row: Option<ClaimRow> = sqlx::query_as(CLAIM)
            .bind(worker)
            .bind(millis(lease))
            .bind(kinds)
            .fetch_optional(self.pool())
            .await?;
        row.map(|r| {
            Ok(Claim {
                id: OperationId::from_uuid(r.id),
                org: org_id(&r.org_id)?,
                kind: r.kind,
                attempt: r.attempt,
                fence: r.fence,
            })
        })
        .transpose()
    }

    /// Settle `claim` in `phase`. False when the fence moved on: another
    /// worker owns the operation now and this result must be dropped.
    pub async fn finish_operation(
        &self,
        claim: &Claim,
        phase: &str,
        error_code: Option<&str>,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(FINISH)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .bind(phase)
            .bind(error_code)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Give `claim` back to be retried after `after`. False when fenced off.
    pub async fn retry_operation(
        &self,
        claim: &Claim,
        after: Duration,
        error_code: &str,
    ) -> Result<bool, StoreError> {
        let rows = sqlx::query(RETRY)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .bind(millis(after))
            .bind(error_code)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Extend the lease of `claim`. False when fenced off: stop working.
    pub async fn renew_lease(&self, claim: &Claim, lease: Duration) -> Result<bool, StoreError> {
        let rows = sqlx::query(RENEW)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .bind(millis(lease))
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Append an event to the history of `claim`'s operation and return its
    /// sequence number, or `None` when fenced off.
    pub async fn record_event(
        &self,
        claim: &Claim,
        kind: &str,
        data: Option<&serde_json::Value>,
    ) -> Result<Option<i64>, StoreError> {
        let mut tx = self.pool().begin().await?;
        let seq: Option<i64> = sqlx::query_scalar(NEXT_EVENT)
            .bind(*claim.id.as_uuid())
            .bind(claim.fence)
            .fetch_optional(&mut *tx)
            .await?;
        let Some(seq) = seq else {
            return Ok(None);
        };
        sqlx::query(INSERT_EVENT)
            .bind(*claim.id.as_uuid())
            .bind(seq)
            .bind(kind)
            .bind(data.map(serde_json::Value::to_string))
            .bind(now_ms())
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;
        Ok(Some(seq))
    }

    /// Hand out up to `limit` pending outbox messages, hidden from other
    /// callers for `visibility`. A message comes back until
    /// [`Store::outbox_delivered`]: delivery is at-least-once.
    pub async fn take_outbox(
        &self,
        limit: i64,
        visibility: Duration,
    ) -> Result<Vec<OutboxMessage>, StoreError> {
        let rows: Vec<OutboxRow> = sqlx::query_as(TAKE_OUTBOX)
            .bind(limit)
            .bind(millis(visibility))
            .fetch_all(self.pool())
            .await?;
        rows.into_iter()
            .map(|r| {
                Ok(OutboxMessage {
                    id: r.id,
                    org: org_id(&r.org_id)?,
                    operation: r.operation_id.map(OperationId::from_uuid),
                    topic: r.topic,
                    payload: serde_json::from_str(&r.payload).map_err(|e| sqlx::Error::Decode(e.into()))?,
                    attempts: r.attempts,
                    created_at: r.created_at,
                })
            })
            .collect()
    }

    /// Mark an outbox message delivered. False when it already was.
    pub async fn outbox_delivered(&self, id: Uuid) -> Result<bool, StoreError> {
        let rows = sqlx::query(OUTBOX_DELIVERED)
            .bind(id)
            .execute(self.pool())
            .await?
            .rows_affected();
        Ok(rows == 1)
    }

    /// Record a provider delivery for `org`. The body hash is computed by the
    /// database; only the hash is kept.
    pub async fn receive(
        &self,
        org: OrgId,
        provider: &str,
        delivery: &str,
        body: &[u8],
    ) -> Result<Received, StoreError> {
        let inserted: Option<Uuid> = sqlx::query_scalar(INSERT_INBOX)
            .bind(provider)
            .bind(delivery)
            .bind(org.to_string())
            .bind(body)
            .bind(Uuid::now_v7())
            .bind(now_ms())
            .fetch_optional(self.pool())
            .await?;
        if let Some(receipt) = inserted {
            return Ok(Received::New(receipt));
        }
        let (receipt, same): (Uuid, bool) = sqlx::query_as(SELECT_INBOX)
            .bind(provider)
            .bind(delivery)
            .bind(org.to_string())
            .bind(body)
            .fetch_one(self.pool())
            .await?;
        Ok(if same {
            Received::Duplicate(receipt)
        } else {
            Received::Changed
        })
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashSet;

    use super::*;
    use crate::testing::{pg_store, skip};

    const KIND: &str = "deploy";

    async fn org_with_target(store: &Store, slug: &str) -> (OrgId, ProjectId, TargetId) {
        let org = store.create_org(slug, slug).await.expect("org").id;
        let mut t = store.tenant(org).await.expect("tenant");
        let project = t.create_project("shop", "Shop").await.expect("project");
        let env = t
            .create_environment(project, "production", "Production", true)
            .await
            .expect("environment");
        let cluster = t.create_cluster("eu-1").await.expect("cluster");
        let placement = t
            .create_placement(project, env, cluster, &format!("{slug}-shop"))
            .await
            .expect("placement");
        let app = t.create_application(project, "web", "Web").await.expect("app");
        let target = t.create_target(project, app, placement).await.expect("target");
        t.commit().await.expect("commit");
        (org, project, target)
    }

    fn deploy(project: ProjectId, target: TargetId, body: &str) -> NewOperation {
        NewOperation {
            kind: KIND.into(),
            target: Some((project, target)),
            lifecycle_uid: None,
            generation: Some(1),
            input_hash: body.as_bytes().to_vec(),
            payload: json!({ "release": body }),
            requested_by: "user:alice".into(),
            deadline_at: None,
            topic: "deploy.accepted".into(),
        }
    }

    fn audit() -> NewAudit {
        NewAudit {
            actor_kind: "user".into(),
            actor_id: Some("alice".into()),
            action: "deploy.accepted".into(),
            outcome: "accepted".into(),
            ..NewAudit::default()
        }
    }

    async fn count(store: &Store, sql: &'static str, id: OperationId) -> i64 {
        sqlx::query_scalar(sql)
            .bind(*id.as_uuid())
            .fetch_one(store.pool())
            .await
            .expect("count")
    }

    #[tokio::test]
    async fn acceptance_is_one_transaction_and_idempotent() {
        let Some(store) = pg_store().await else {
            skip("acceptance");
            return;
        };
        let (org, project, target) = org_with_target(&store, "a").await;
        let key = IdempotencyKey {
            actor: "user:alice".into(),
            key: "k-1".into(),
            ttl: Duration::from_hours(1),
        };

        // Not committed: nothing at all, not even the receipt.
        let mut t = store.tenant(org).await.expect("tenant");
        let Accepted::New(lost) = t
            .accept(&deploy(project, target, "r1"), audit(), Some(&key))
            .await
            .expect("accept")
        else {
            panic!("a first request is new");
        };
        drop(t);
        assert_eq!(
            count(&store, "SELECT count(*) FROM operations WHERE id = $1", lost).await,
            0
        );

        let mut t = store.tenant(org).await.expect("tenant");
        let accepted = t
            .accept(&deploy(project, target, "r1"), audit(), Some(&key))
            .await
            .expect("accept");
        t.commit().await.expect("commit");
        let Accepted::New(id) = accepted else {
            panic!("the rolled-back receipt does not count: {accepted:?}");
        };
        assert_eq!(
            count(&store, "SELECT count(*) FROM operations WHERE id = $1", id).await,
            1
        );
        assert_eq!(
            count(&store, "SELECT count(*) FROM outbox WHERE operation_id = $1", id).await,
            1
        );
        let audits: i64 = sqlx::query_scalar("SELECT count(*) FROM audit_events WHERE org_id = $1")
            .bind(org.to_string())
            .fetch_one(store.pool())
            .await
            .expect("audits");
        assert_eq!(audits, 1, "the audit record committed with the operation");

        let mut t = store.tenant(org).await.expect("tenant");
        assert_eq!(
            t.accept(&deploy(project, target, "r1"), audit(), Some(&key))
                .await
                .expect("again"),
            Accepted::Replayed(id),
            "same key, same request: same operation"
        );
        assert_eq!(
            t.accept(&deploy(project, target, "r2"), audit(), Some(&key))
                .await
                .expect("reused"),
            Accepted::KeyReused(id),
            "same key, another request: conflict"
        );
        t.commit().await.expect("commit");
        let total: i64 = sqlx::query_scalar("SELECT count(*) FROM operations")
            .fetch_one(store.pool())
            .await
            .expect("total");
        assert_eq!(total, 1, "neither replay wrote an operation");
    }

    #[tokio::test]
    async fn claims_are_exclusive() {
        let Some(store) = pg_store().await else {
            skip("claims");
            return;
        };
        let (org, project, target) = org_with_target(&store, "a").await;
        let mut t = store.tenant(org).await.expect("tenant");
        for n in 0..40 {
            t.accept(&deploy(project, target, &format!("r{n}")), audit(), None)
                .await
                .expect("accept");
        }
        t.commit().await.expect("commit");

        let mut workers = Vec::new();
        for w in 0..8 {
            let store = store.clone();
            workers.push(tokio::spawn(async move {
                let mut done = Vec::new();
                while let Some(claim) = store
                    .claim_operation(&format!("w{w}"), &[KIND], Duration::from_secs(30))
                    .await
                    .expect("claim")
                {
                    assert!(
                        store
                            .finish_operation(&claim, "succeeded", None)
                            .await
                            .expect("finish"),
                        "the lease holder can finish"
                    );
                    done.push(claim.id);
                }
                done
            }));
        }
        let mut all = Vec::new();
        for w in workers {
            all.extend(w.await.expect("worker"));
        }
        assert_eq!(all.len(), 40, "every operation claimed once");
        assert_eq!(all.iter().collect::<HashSet<_>>().len(), 40, "never twice");
    }

    /// A paused worker whose lease expired cannot write after a takeover.
    #[tokio::test]
    async fn a_paused_worker_is_fenced_off_after_a_takeover() {
        let Some(store) = pg_store().await else {
            skip("fencing");
            return;
        };
        let (org, project, target) = org_with_target(&store, "a").await;
        let mut t = store.tenant(org).await.expect("tenant");
        t.accept(&deploy(project, target, "late"), audit(), None)
            .await
            .expect("accept");
        t.commit().await.expect("commit");
        let slow = store
            .claim_operation("slow", &[KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("due");
        sqlx::query("UPDATE operations SET lease_until = 0 WHERE id = $1")
            .bind(*slow.id.as_uuid())
            .execute(store.pool())
            .await
            .expect("expire the lease");
        let fast = store
            .claim_operation("fast", &[KIND], Duration::from_secs(30))
            .await
            .expect("claim")
            .expect("taken over");
        assert_eq!(fast.id, slow.id);
        assert!(fast.fence > slow.fence, "a new claim raises the fence");
        assert!(
            !store
                .renew_lease(&slow, Duration::from_secs(30))
                .await
                .expect("renew")
        );
        assert_eq!(
            store.record_event(&slow, "progress", None).await.expect("event"),
            None
        );
        assert!(
            !store
                .finish_operation(&slow, "succeeded", None)
                .await
                .expect("finish")
        );
        assert_eq!(
            store.record_event(&fast, "applied", None).await.expect("event"),
            Some(1)
        );
        assert_eq!(
            store.record_event(&fast, "verified", None).await.expect("event"),
            Some(2)
        );
        assert!(
            sqlx::query("UPDATE operation_events SET kind = 'forged'")
                .execute(store.pool())
                .await
                .is_err(),
            "history is append-only"
        );
        assert!(
            store
                .finish_operation(&fast, "succeeded", None)
                .await
                .expect("finish")
        );
        assert!(
            store
                .claim_operation("any", &[KIND], Duration::from_secs(30))
                .await
                .expect("claim")
                .is_none(),
            "finished operations are never claimed again"
        );
    }

    #[tokio::test]
    async fn outbox_delivers_at_least_once() {
        let Some(store) = pg_store().await else {
            skip("outbox");
            return;
        };
        let (org, project, target) = org_with_target(&store, "a").await;
        let mut t = store.tenant(org).await.expect("tenant");
        let Accepted::New(id) = t
            .accept(&deploy(project, target, "r1"), audit(), None)
            .await
            .expect("accept")
        else {
            panic!("new");
        };
        t.commit().await.expect("commit");

        let first = store.take_outbox(10, Duration::from_mins(1)).await.expect("take");
        assert_eq!(first.len(), 1);
        assert_eq!(first[0].operation, Some(id));
        assert_eq!(first[0].payload["kind"], KIND);
        assert!(
            store
                .take_outbox(10, Duration::from_mins(1))
                .await
                .expect("take")
                .is_empty(),
            "hidden while a delivery is in progress"
        );

        // The acknowledgement was lost: the message comes back.
        sqlx::query("UPDATE outbox SET available_at = 0")
            .execute(store.pool())
            .await
            .expect("expire");
        let again = store.take_outbox(10, Duration::from_mins(1)).await.expect("take");
        assert_eq!(again.len(), 1);
        assert_eq!(again[0].attempts, 2);
        assert!(store.outbox_delivered(again[0].id).await.expect("ack"));
        assert!(!store.outbox_delivered(again[0].id).await.expect("ack twice"));
        sqlx::query("UPDATE outbox SET available_at = 0")
            .execute(store.pool())
            .await
            .expect("expire");
        assert!(
            store
                .take_outbox(10, Duration::from_mins(1))
                .await
                .expect("take")
                .is_empty()
        );
    }

    #[tokio::test]
    async fn inbox_deduplicates_deliveries() {
        let Some(store) = pg_store().await else {
            skip("inbox");
            return;
        };
        let a = store.create_org("a", "A").await.expect("org").id;
        let b = store.create_org("b", "B").await.expect("org").id;
        let Received::New(receipt) = store
            .receive(a, "github", "d-1", b"{\"ref\":\"main\"}")
            .await
            .expect("first")
        else {
            panic!("a first delivery is new");
        };
        assert_eq!(
            store
                .receive(a, "github", "d-1", b"{\"ref\":\"main\"}")
                .await
                .expect("again"),
            Received::Duplicate(receipt)
        );
        assert_eq!(
            store
                .receive(a, "github", "d-1", b"{\"ref\":\"evil\"}")
                .await
                .expect("changed"),
            Received::Changed,
            "same delivery id, another body"
        );
        assert_eq!(
            store
                .receive(b, "github", "d-1", b"{\"ref\":\"main\"}")
                .await
                .expect("other org"),
            Received::Changed,
            "same delivery id, another organization"
        );
        assert!(matches!(
            store
                .receive(a, "gitlab", "d-1", b"x")
                .await
                .expect("other provider"),
            Received::New(_)
        ));
    }
}

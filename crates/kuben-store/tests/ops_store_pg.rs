//! M0 spike (ADR-025): PostgreSQL as the durable operation store.
//!
//! Runs only when `KUBEN_TEST_PG_URL` is set (the Store matrix job in CI).
//! On a real server it shows that:
//! 1. accepting a request writes operation, audit and outbox in one
//!    transaction: a crash before commit leaves nothing, a commit leaves all;
//! 2. the inbox deduplicates provider deliveries and detects a changed body;
//! 3. `FOR UPDATE SKIP LOCKED` claims never hand one operation to two workers;
//! 4. a worker whose lease was taken over cannot write with its old fence;
//! 5. replaying an outbox message after a lost acknowledgement has one effect.
//!
//! The tables are throwaway `m0_*` tables, not the product schema.

use std::collections::HashSet;

use sqlx::{PgPool, Row, postgres::PgPoolOptions};
use uuid::Uuid;

const SCHEMA: &str = r"
DROP TABLE IF EXISTS m0_effects, m0_outbox, m0_audit, m0_inbox, m0_operations;
CREATE TABLE m0_operations (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL,
    target_id       uuid NOT NULL,
    generation      bigint NOT NULL,
    phase           text NOT NULL DEFAULT 'queued',
    fence           bigint NOT NULL DEFAULT 0,
    lease_owner     text,
    lease_until     timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX m0_operations_ready ON m0_operations (next_attempt_at, id) WHERE phase = 'queued';
CREATE TABLE m0_audit (
    id           bigserial PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES m0_operations (id),
    action       text NOT NULL
);
CREATE TABLE m0_outbox (
    id           bigserial PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES m0_operations (id),
    message      text NOT NULL,
    delivered_at timestamptz
);
CREATE TABLE m0_inbox (
    provider    text  NOT NULL,
    delivery_id text  NOT NULL,
    body_sha256 bytea NOT NULL,
    receipt     uuid  NOT NULL,
    PRIMARY KEY (provider, delivery_id)
);
CREATE TABLE m0_effects (
    operation_id uuid PRIMARY KEY,
    applied_at   timestamptz NOT NULL DEFAULT now()
);
";

const CLAIM: &str = r"
UPDATE m0_operations
   SET lease_owner = $1, lease_until = now() + interval '30 seconds', fence = fence + 1
 WHERE id = (SELECT id FROM m0_operations
              WHERE phase = 'queued' AND (lease_until IS NULL OR lease_until < now())
              ORDER BY next_attempt_at, id
              FOR UPDATE SKIP LOCKED
              LIMIT 1)
RETURNING id, fence";

const COMPLETE: &str = "UPDATE m0_operations SET phase = 'succeeded', lease_owner = NULL, lease_until = NULL WHERE id = $1 AND fence = $2";

async fn pool() -> Option<PgPool> {
    let url = std::env::var("KUBEN_TEST_PG_URL").ok()?;
    Some(
        PgPoolOptions::new()
            .max_connections(20)
            .connect(&url)
            .await
            .expect("connect to KUBEN_TEST_PG_URL"),
    )
}

/// Accept a deploy request: one transaction, or nothing (`commit = false`
/// drops the transaction, like a crash before the commit).
async fn accept(pool: &PgPool, commit: bool) -> Uuid {
    let id = Uuid::now_v7();
    let mut tx = pool.begin().await.expect("begin");
    sqlx::query("INSERT INTO m0_operations (id, kind, target_id, generation) VALUES ($1, 'deploy', $2, 1)")
        .bind(id)
        .bind(Uuid::now_v7())
        .execute(&mut *tx)
        .await
        .expect("operation");
    sqlx::query("INSERT INTO m0_audit (operation_id, action) VALUES ($1, 'deploy.accepted')")
        .bind(id)
        .execute(&mut *tx)
        .await
        .expect("audit");
    sqlx::query("INSERT INTO m0_outbox (operation_id, message) VALUES ($1, 'deliver')")
        .bind(id)
        .execute(&mut *tx)
        .await
        .expect("outbox");
    if commit {
        tx.commit().await.expect("commit");
    } else {
        drop(tx);
    }
    id
}

async fn rows_for(pool: &PgPool, id: Uuid) -> (i64, i64, i64) {
    let row = sqlx::query(
        "SELECT (SELECT count(*) FROM m0_operations WHERE id = $1),
                (SELECT count(*) FROM m0_audit WHERE operation_id = $1),
                (SELECT count(*) FROM m0_outbox WHERE operation_id = $1)",
    )
    .bind(id)
    .fetch_one(pool)
    .await
    .expect("count");
    (row.get(0), row.get(1), row.get(2))
}

async fn accept_is_atomic(pool: &PgPool) {
    let lost = accept(pool, false).await;
    assert_eq!(
        rows_for(pool, lost).await,
        (0, 0, 0),
        "a crash before commit leaves nothing"
    );
    let kept = accept(pool, true).await;
    assert_eq!(
        rows_for(pool, kept).await,
        (1, 1, 1),
        "a commit leaves operation, audit and outbox"
    );
}

#[derive(Debug, PartialEq, Eq)]
enum Received {
    New(Uuid),
    Duplicate(Uuid),
    BodyChanged,
}

/// Record a provider delivery; the hash is computed by PostgreSQL.
async fn receive(pool: &PgPool, provider: &str, delivery: &str, body: &[u8]) -> Received {
    let inserted: Option<Uuid> = sqlx::query_scalar(
        "INSERT INTO m0_inbox (provider, delivery_id, body_sha256, receipt)
         VALUES ($1, $2, sha256($3), $4)
         ON CONFLICT (provider, delivery_id) DO NOTHING
         RETURNING receipt",
    )
    .bind(provider)
    .bind(delivery)
    .bind(body)
    .bind(Uuid::now_v7())
    .fetch_optional(pool)
    .await
    .expect("inbox insert");
    if let Some(receipt) = inserted {
        return Received::New(receipt);
    }
    let row = sqlx::query(
        "SELECT receipt, body_sha256 = sha256($3) FROM m0_inbox WHERE provider = $1 AND delivery_id = $2",
    )
    .bind(provider)
    .bind(delivery)
    .bind(body)
    .fetch_one(pool)
    .await
    .expect("inbox read");
    if row.get::<bool, _>(1) {
        Received::Duplicate(row.get(0))
    } else {
        Received::BodyChanged
    }
}

async fn inbox_deduplicates(pool: &PgPool) {
    let Received::New(receipt) = receive(pool, "github", "d-1", b"{\"ref\":\"main\"}").await else {
        panic!("first delivery must be new");
    };
    assert_eq!(
        receive(pool, "github", "d-1", b"{\"ref\":\"main\"}").await,
        Received::Duplicate(receipt),
        "a redelivery returns the same receipt"
    );
    assert_eq!(
        receive(pool, "github", "d-1", b"{\"ref\":\"evil\"}").await,
        Received::BodyChanged,
        "the same delivery id with another body is an integrity error"
    );
    assert!(
        matches!(receive(pool, "gitlab", "d-1", b"x").await, Received::New(_)),
        "ids are per provider"
    );
}

async fn seed(pool: &PgPool, n: usize) {
    for _ in 0..n {
        sqlx::query(
            "INSERT INTO m0_operations (id, kind, target_id, generation) VALUES ($1, 'build', $2, 1)",
        )
        .bind(Uuid::now_v7())
        .bind(Uuid::now_v7())
        .execute(pool)
        .await
        .expect("seed");
    }
}

async fn claim(pool: &PgPool, worker: &str) -> Option<(Uuid, i64)> {
    sqlx::query(CLAIM)
        .bind(worker)
        .fetch_optional(pool)
        .await
        .expect("claim")
        .map(|row| (row.get(0), row.get(1)))
}

/// Operations seeded for the concurrent-claim scenario.
const OPS: usize = 60;

async fn skip_locked_claims_are_exclusive(pool: &PgPool) {
    sqlx::query("UPDATE m0_operations SET phase = 'done-before'")
        .execute(pool)
        .await
        .expect("reset");
    seed(pool, OPS).await;

    let mut workers = Vec::new();
    for w in 0..16 {
        let pool = pool.clone();
        workers.push(tokio::spawn(async move {
            let name = format!("worker-{w}");
            let mut done = Vec::new();
            while let Some((id, fence)) = claim(&pool, &name).await {
                let res = sqlx::query(COMPLETE)
                    .bind(id)
                    .bind(fence)
                    .execute(&pool)
                    .await
                    .expect("complete");
                assert_eq!(res.rows_affected(), 1, "the lease holder can always complete");
                done.push(id);
            }
            done
        }));
    }
    let mut all = Vec::new();
    for w in workers {
        all.extend(w.await.expect("worker"));
    }
    let unique: HashSet<Uuid> = all.iter().copied().collect();
    assert_eq!(
        all.len(),
        OPS,
        "every operation claimed exactly once (got {})",
        all.len()
    );
    assert_eq!(unique.len(), OPS, "no operation was handed to two workers");
}

async fn stale_fence_cannot_write(pool: &PgPool) {
    sqlx::query("UPDATE m0_operations SET phase = 'done-before' WHERE phase = 'queued'")
        .execute(pool)
        .await
        .expect("reset");
    seed(pool, 1).await;

    let (id, fence_a) = claim(pool, "a").await.expect("a claims");
    // A pauses; its lease expires; B takes over.
    sqlx::query("UPDATE m0_operations SET lease_until = now() - interval '1 second' WHERE id = $1")
        .bind(id)
        .execute(pool)
        .await
        .expect("expire");
    let (id_b, fence_b) = claim(pool, "b").await.expect("b claims");
    assert_eq!(id, id_b);
    assert!(fence_b > fence_a, "a new claim raises the fence");

    let late = sqlx::query(COMPLETE)
        .bind(id)
        .bind(fence_a)
        .execute(pool)
        .await
        .expect("a writes");
    assert_eq!(late.rows_affected(), 0, "the paused worker's write is fenced off");
    let current = sqlx::query(COMPLETE)
        .bind(id)
        .bind(fence_b)
        .execute(pool)
        .await
        .expect("b writes");
    assert_eq!(current.rows_affected(), 1);
}

async fn outbox_replay_has_one_effect(pool: &PgPool) {
    let id = accept(pool, true).await;
    let deliver = || async {
        sqlx::query("INSERT INTO m0_effects (operation_id) VALUES ($1) ON CONFLICT (operation_id) DO NOTHING")
            .bind(id)
            .execute(pool)
            .await
            .expect("effect")
            .rows_affected()
    };
    assert_eq!(deliver().await, 1, "first delivery applies");
    // The acknowledgement was lost; the outbox row is still undelivered.
    assert_eq!(deliver().await, 0, "the replay is a no-op");
    sqlx::query("UPDATE m0_outbox SET delivered_at = now() WHERE operation_id = $1")
        .bind(id)
        .execute(pool)
        .await
        .expect("ack");
    let effects: i64 = sqlx::query_scalar("SELECT count(*) FROM m0_effects WHERE operation_id = $1")
        .bind(id)
        .fetch_one(pool)
        .await
        .expect("count");
    assert_eq!(effects, 1);
}

#[tokio::test]
async fn postgres_operation_store_spike() {
    let Some(pool) = pool().await else {
        eprintln!("skipped: set KUBEN_TEST_PG_URL to run against PostgreSQL");
        return;
    };
    sqlx::raw_sql(SCHEMA).execute(&pool).await.expect("schema");

    accept_is_atomic(&pool).await;
    inbox_deduplicates(&pool).await;
    skip_locked_claims_are_exclusive(&pool).await;
    stale_fence_cannot_write(&pool).await;
    outbox_replay_has_one_effect(&pool).await;

    sqlx::raw_sql("DROP TABLE m0_effects, m0_outbox, m0_audit, m0_inbox, m0_operations")
        .execute(&pool)
        .await
        .expect("drop");
}

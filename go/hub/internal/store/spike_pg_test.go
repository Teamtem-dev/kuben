package store_test

// The M0 spike (ADR-025, crates/kuben-store/tests/ops_store_pg.rs):
// PostgreSQL as the durable operation store. On a real server it shows that
//  1. accepting a request writes operation, audit and outbox in one
//     transaction: a crash before commit leaves nothing, a commit leaves all;
//  2. the inbox deduplicates provider deliveries and detects a changed body;
//  3. `FOR UPDATE SKIP LOCKED` claims never hand one operation to two workers;
//  4. a worker whose lease was taken over cannot write with its old fence;
//  5. replaying an outbox message after a lost acknowledgement has one effect.
//
// The tables are throwaway `m0_*` tables in a schema of the test's own, not
// the product schema.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

const spikeSchema = `
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
`

const spikeClaim = `
UPDATE m0_operations
   SET lease_owner = $1, lease_until = now() + interval '30 seconds', fence = fence + 1
 WHERE id = (SELECT id FROM m0_operations
              WHERE phase = 'queued' AND (lease_until IS NULL OR lease_until < now())
              ORDER BY next_attempt_at, id
              FOR UPDATE SKIP LOCKED
              LIMIT 1)
RETURNING id, fence`

const spikeComplete = "UPDATE m0_operations SET phase = 'succeeded', lease_owner = NULL, lease_until = NULL WHERE id = $1 AND fence = $2"

// spikeOps is the number of operations seeded for the concurrent claims.
const spikeOps = 60

func TestPostgresOperationStoreSpike(t *testing.T) {
	pc := pgtest.Schema(t)
	pc.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, spikeSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	t.Run("accept is atomic", func(t *testing.T) { acceptIsAtomic(t, pool) })
	t.Run("inbox deduplicates", func(t *testing.T) { inboxDeduplicates(t, pool) })
	t.Run("skip locked claims are exclusive", func(t *testing.T) { skipLockedClaimsAreExclusive(t, pool) })
	t.Run("stale fence cannot write", func(t *testing.T) { staleFenceCannotWrite(t, pool) })
	t.Run("outbox replay has one effect", func(t *testing.T) { outboxReplayHasOneEffect(t, pool) })

	if _, err := pool.Exec(ctx, "DROP TABLE m0_effects, m0_outbox, m0_audit, m0_inbox, m0_operations"); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

func newV7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// spikeAccept accepts a deploy request: one transaction, or nothing
// (commit false rolls it back, like a crash before the commit).
func spikeAccept(t *testing.T, pool *pgxpool.Pool, commit bool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	id := newV7(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO m0_operations (id, kind, target_id, generation) VALUES ($1, 'deploy', $2, 1)", []any{id, newV7(t)}},
		{"INSERT INTO m0_audit (operation_id, action) VALUES ($1, 'deploy.accepted')", []any{id}},
		{"INSERT INTO m0_outbox (operation_id, message) VALUES ($1, 'deliver')", []any{id}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if commit {
		err = tx.Commit(ctx)
	} else {
		err = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatalf("end transaction: %v", err)
	}
	return id
}

func spikeRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) [3]int64 {
	t.Helper()
	var n [3]int64
	err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM m0_operations WHERE id = $1),
                (SELECT count(*) FROM m0_audit WHERE operation_id = $1),
                (SELECT count(*) FROM m0_outbox WHERE operation_id = $1)`, id).Scan(&n[0], &n[1], &n[2])
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func acceptIsAtomic(t *testing.T, pool *pgxpool.Pool) {
	if got := spikeRows(t, pool, spikeAccept(t, pool, false)); got != [3]int64{0, 0, 0} {
		t.Errorf("a crash before commit leaves nothing: %v", got)
	}
	if got := spikeRows(t, pool, spikeAccept(t, pool, true)); got != [3]int64{1, 1, 1} {
		t.Errorf("a commit leaves operation, audit and outbox: %v", got)
	}
}

type received struct {
	kind    string // new, duplicate or changed
	receipt uuid.UUID
}

// spikeReceive records a provider delivery; PostgreSQL computes the hash.
func spikeReceive(t *testing.T, pool *pgxpool.Pool, provider, delivery string, body []byte) received {
	t.Helper()
	ctx := t.Context()
	var receipt uuid.UUID
	err := pool.QueryRow(ctx, `INSERT INTO m0_inbox (provider, delivery_id, body_sha256, receipt)
         VALUES ($1, $2, sha256($3), $4)
         ON CONFLICT (provider, delivery_id) DO NOTHING
         RETURNING receipt`, provider, delivery, body, newV7(t)).Scan(&receipt)
	switch {
	case err == nil:
		return received{"new", receipt}
	case !errors.Is(err, pgx.ErrNoRows):
		t.Fatalf("inbox insert: %v", err)
	}
	var same bool
	if err := pool.QueryRow(ctx,
		"SELECT receipt, body_sha256 = sha256($3) FROM m0_inbox WHERE provider = $1 AND delivery_id = $2",
		provider, delivery, body).Scan(&receipt, &same); err != nil {
		t.Fatalf("inbox read: %v", err)
	}
	if same {
		return received{"duplicate", receipt}
	}
	return received{kind: "changed"}
}

func inboxDeduplicates(t *testing.T, pool *pgxpool.Pool) {
	first := spikeReceive(t, pool, "github", "d-1", []byte(`{"ref":"main"}`))
	if first.kind != "new" {
		t.Fatalf("first delivery must be new: %+v", first)
	}
	if got := spikeReceive(t, pool, "github", "d-1", []byte(`{"ref":"main"}`)); got != (received{"duplicate", first.receipt}) {
		t.Errorf("a redelivery returns the same receipt: %+v", got)
	}
	if got := spikeReceive(t, pool, "github", "d-1", []byte(`{"ref":"evil"}`)); got.kind != "changed" {
		t.Errorf("the same delivery id with another body is an integrity error: %+v", got)
	}
	if got := spikeReceive(t, pool, "gitlab", "d-1", []byte("x")); got.kind != "new" {
		t.Errorf("ids are per provider: %+v", got)
	}
}

func spikeSeed(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	for range n {
		if _, err := pool.Exec(t.Context(),
			"INSERT INTO m0_operations (id, kind, target_id, generation) VALUES ($1, 'build', $2, 1)",
			newV7(t), newV7(t)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// spikeClaimOne claims the next ready operation; ok is false when none is.
func spikeClaimOne(ctx context.Context, pool *pgxpool.Pool, worker string) (id uuid.UUID, fence int64, ok bool, err error) {
	err = pool.QueryRow(ctx, spikeClaim, worker).Scan(&id, &fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, 0, false, nil
	}
	return id, fence, err == nil, err
}

func skipLockedClaimsAreExclusive(t *testing.T, pool *pgxpool.Pool) {
	ctx := t.Context()
	if _, err := pool.Exec(ctx, "UPDATE m0_operations SET phase = 'done-before'"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	spikeSeed(t, pool, spikeOps)

	done := make([][]uuid.UUID, 16)
	var g errgroup.Group
	for w := range done {
		g.Go(func() error {
			name := fmt.Sprintf("worker-%d", w)
			for {
				id, fence, ok, err := spikeClaimOne(ctx, pool, name)
				if err != nil || !ok {
					return err
				}
				tag, err := pool.Exec(ctx, spikeComplete, id, fence)
				if err != nil {
					return fmt.Errorf("complete: %w", err)
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("the lease holder %s could not complete %s", name, id)
				}
				done[w] = append(done[w], id)
			}
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	all := 0
	unique := map[uuid.UUID]bool{}
	for _, ids := range done {
		all += len(ids)
		for _, id := range ids {
			unique[id] = true
		}
	}
	if all != spikeOps {
		t.Errorf("every operation claimed exactly once (got %d)", all)
	}
	if len(unique) != spikeOps {
		t.Errorf("no operation was handed to two workers: %d unique of %d", len(unique), spikeOps)
	}
}

func staleFenceCannotWrite(t *testing.T, pool *pgxpool.Pool) {
	ctx := t.Context()
	if _, err := pool.Exec(ctx, "UPDATE m0_operations SET phase = 'done-before' WHERE phase = 'queued'"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	spikeSeed(t, pool, 1)

	id, fenceA, ok, err := spikeClaimOne(ctx, pool, "a")
	if err != nil || !ok {
		t.Fatalf("a claims: %v %v", ok, err)
	}
	// A pauses; its lease expires; B takes over.
	if _, err := pool.Exec(ctx, "UPDATE m0_operations SET lease_until = now() - interval '1 second' WHERE id = $1", id); err != nil {
		t.Fatalf("expire: %v", err)
	}
	idB, fenceB, ok, err := spikeClaimOne(ctx, pool, "b")
	if err != nil || !ok {
		t.Fatalf("b claims: %v %v", ok, err)
	}
	if idB != id {
		t.Fatalf("b claimed %s, want %s", idB, id)
	}
	if fenceB <= fenceA {
		t.Errorf("a new claim raises the fence: %d then %d", fenceA, fenceB)
	}
	late, err := pool.Exec(ctx, spikeComplete, id, fenceA)
	if err != nil {
		t.Fatalf("a writes: %v", err)
	}
	if late.RowsAffected() != 0 {
		t.Error("the paused worker's write is fenced off")
	}
	current, err := pool.Exec(ctx, spikeComplete, id, fenceB)
	if err != nil {
		t.Fatalf("b writes: %v", err)
	}
	if current.RowsAffected() != 1 {
		t.Errorf("b's write affected %d rows", current.RowsAffected())
	}
}

func outboxReplayHasOneEffect(t *testing.T, pool *pgxpool.Pool) {
	ctx := t.Context()
	id := spikeAccept(t, pool, true)
	deliver := func() int64 {
		tag, err := pool.Exec(ctx, "INSERT INTO m0_effects (operation_id) VALUES ($1) ON CONFLICT (operation_id) DO NOTHING", id)
		if err != nil {
			t.Fatalf("effect: %v", err)
		}
		return tag.RowsAffected()
	}
	if n := deliver(); n != 1 {
		t.Errorf("first delivery applies: %d", n)
	}
	// The acknowledgement was lost; the outbox row is still undelivered.
	if n := deliver(); n != 0 {
		t.Errorf("the replay is a no-op: %d", n)
	}
	if _, err := pool.Exec(ctx, "UPDATE m0_outbox SET delivered_at = now() WHERE operation_id = $1", id); err != nil {
		t.Fatalf("ack: %v", err)
	}
	var effects int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM m0_effects WHERE operation_id = $1", id).Scan(&effects); err != nil {
		t.Fatalf("count: %v", err)
	}
	if effects != 1 {
		t.Errorf("effects %d", effects)
	}
}

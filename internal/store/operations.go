package store

// Durable operations (plan §8.2, §9.2, §9.3): transactional acceptance with
// idempotency receipts, claims with a lease and a fence, the outbox and the
// inbox; the port of repo/operations.rs.
//
// Acceptance runs in the caller's [Tenant] transaction. Workers are not
// bound to one organization: their claims and writes go through [Store] and
// are conditional on the fence of their claim (I06).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
)

const (
	upsertReceipt = "INSERT INTO idempotency_receipts " +
		"(org_id, actor, operation, key, request_hash, operation_id, created_at, expires_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
		"ON CONFLICT (org_id, actor, operation, key) DO UPDATE " +
		"SET request_hash = EXCLUDED.request_hash, operation_id = EXCLUDED.operation_id, " +
		"created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at " +
		"WHERE idempotency_receipts.expires_at <= EXCLUDED.created_at"
	selectReceipt = "SELECT request_hash, operation_id FROM idempotency_receipts " +
		"WHERE org_id = $1 AND actor = $2 AND operation = $3 AND key = $4"
	insertOperation = "INSERT INTO operations " +
		"(id, org_id, project_id, target_id, kind, lifecycle_uid, generation, input_hash, payload, " +
		"requested_by, requested_at, deadline_at, next_attempt_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, kuben_now_ms())"
	insertOutbox = "INSERT INTO outbox (id, org_id, operation_id, topic, payload, created_at, available_at) " +
		"VALUES ($1, $2, $3, $4, $5::jsonb, $6, kuben_now_ms())"
	claimOperation = "UPDATE operations " +
		"SET lease_owner = $1, lease_until = kuben_now_ms() + $2, fence = fence + 1, attempt = attempt + 1 " +
		"WHERE id = (SELECT id FROM operations " +
		"WHERE NOT done AND kind = ANY($3) AND next_attempt_at <= kuben_now_ms() " +
		"AND (lease_until IS NULL OR lease_until < kuben_now_ms()) " +
		"ORDER BY next_attempt_at, id " +
		"FOR UPDATE SKIP LOCKED " +
		"LIMIT 1) " +
		"RETURNING id, org_id, kind, attempt, fence"
	// finishOperation settles the operation and leaves a `<kind>.settled`
	// message (M4.10) in the same statement.
	finishOperation = "WITH settled AS (UPDATE operations " +
		"SET phase = $3, done = TRUE, error_code = $4, lease_owner = NULL, lease_until = NULL, " +
		"last_progress_at = kuben_now_ms() " +
		"WHERE id = $1 AND fence = $2 AND NOT done " +
		"RETURNING id, org_id, kind) " +
		"INSERT INTO outbox (id, org_id, operation_id, topic, payload, created_at, available_at) " +
		"SELECT gen_random_uuid(), org_id, id, kind || '.settled', " +
		"jsonb_build_object('phase', $3::text, 'code', $4::text), kuben_now_ms(), kuben_now_ms() " +
		"FROM settled"
	retryOperation = "UPDATE operations " +
		"SET next_attempt_at = kuben_now_ms() + $3, error_code = $4, lease_owner = NULL, lease_until = NULL, " +
		"last_progress_at = kuben_now_ms() " +
		"WHERE id = $1 AND fence = $2 AND NOT done"
	renewLease = "UPDATE operations " +
		"SET lease_until = kuben_now_ms() + $3, last_progress_at = kuben_now_ms() " +
		"WHERE id = $1 AND fence = $2 AND NOT done"
	nextEvent = "UPDATE operations " +
		"SET event_seq = event_seq + 1, last_progress_at = kuben_now_ms() " +
		"WHERE id = $1 AND fence = $2 RETURNING event_seq"
	insertEvent = "INSERT INTO operation_events (operation_id, seq, kind, data, at) " +
		"VALUES ($1, $2, $3, $4::jsonb, $5)"
	takeOutbox = "UPDATE outbox SET available_at = kuben_now_ms() + $2, attempts = attempts + 1 " +
		"WHERE id IN (SELECT id FROM outbox " +
		"WHERE delivered_at IS NULL AND available_at <= kuben_now_ms() " +
		"ORDER BY available_at, id " +
		"FOR UPDATE SKIP LOCKED " +
		"LIMIT $1) " +
		"RETURNING id, org_id, operation_id, topic, payload::text AS payload, attempts, created_at"
	outboxDelivered = "UPDATE outbox SET delivered_at = kuben_now_ms() WHERE id = $1 AND delivered_at IS NULL"
	insertInbox     = "INSERT INTO inbox (provider, delivery_id, org_id, body_sha256, receipt, received_at) " +
		"VALUES ($1, $2, $3, sha256($4), $5, $6) " +
		"ON CONFLICT (provider, delivery_id) DO NOTHING " +
		"RETURNING receipt"
	selectInbox = "SELECT receipt, org_id = $3 AND body_sha256 = sha256($4) FROM inbox " +
		"WHERE provider = $1 AND delivery_id = $2"
)

// OperationTarget is the project and target an operation acts on.
type OperationTarget struct {
	Project ids.ProjectID
	Target  ids.TargetID
}

// NewOperation is a request to accept as one durable operation.
type NewOperation struct {
	Kind string
	// Target is what the operation acts on, if anything.
	Target opt.Val[OperationTarget]
	// LifecycleUID and Generation are the target's, as the request saw them.
	LifecycleUID opt.Val[uuid.UUID]
	Generation   opt.Val[uint64]
	// InputHash is the hash of the canonical request; idempotency compares
	// it.
	InputHash []byte
	// Payload is any JSON value, stored as serde_json wrote it.
	Payload     any
	RequestedBy string
	DeadlineAt  opt.Val[int64]
	// Topic is the outbox topic that tells the executors about it.
	Topic string
}

// IdempotencyKey is an `Idempotency-Key` and the caller that sent it (plan
// §8.2).
type IdempotencyKey struct {
	Actor string
	Key   string
	// TTL is how long the receipt is kept. The operation stays regardless.
	TTL time.Duration
}

// Accepted is the outcome of [Tenant.Accept].
//
//sumtype:decl
type Accepted interface{ accepted() }

type (
	// AcceptedNew is a new operation, committed with its audit record and
	// outbox message.
	AcceptedNew struct{ ID ids.OperationID }
	// AcceptedReplayed means the same key and request were accepted before, as
	// this operation.
	AcceptedReplayed struct{ ID ids.OperationID }
	// AcceptedKeyReused means the key was used for another request, this one
	// (HTTP 409); nothing was written.
	AcceptedKeyReused struct{ ID ids.OperationID }
)

func (AcceptedNew) accepted()       {}
func (AcceptedReplayed) accepted()  {}
func (AcceptedKeyReused) accepted() {}

// Claim is a claimed operation. Every write about it is conditional on
// Fence.
type Claim struct {
	ID      ids.OperationID
	Org     ids.OrgID
	Kind    string
	Attempt int32
	Fence   int64
}

// OutboxMessage is a message handed out by [Store.TakeOutbox]; it comes back
// until it is marked delivered.
type OutboxMessage struct {
	ID        uuid.UUID
	Org       ids.OrgID
	Operation opt.Val[ids.OperationID]
	Topic     string
	Payload   any
	Attempts  int32
	// CreatedAt is when the message was written (unix ms).
	CreatedAt int64
}

// Received is the outcome of [Store.Receive].
//
//sumtype:decl
type Received interface{ received() }

type (
	// ReceivedNew is a first delivery and its receipt.
	ReceivedNew struct{ Receipt uuid.UUID }
	// ReceivedDuplicate is a redelivery: the receipt of the first delivery.
	ReceivedDuplicate struct{ Receipt uuid.UUID }
	// ReceivedChanged is the same delivery id with another body or
	// organization: an integrity error, never processed.
	ReceivedChanged struct{}
)

func (ReceivedNew) received()       {}
func (ReceivedDuplicate) received() {}
func (ReceivedChanged) received()   {}

// millis is a duration in milliseconds (Rust: as_millis, saturating).
func millis(d time.Duration) int64 { return d.Milliseconds() }

// signed is a counter as PostgreSQL's BIGINT; beyond it is an encode error.
func signed(op string, value uint64) (int64, error) {
	if value > 1<<63-1 {
		return 0, DatabaseError{Op: op, Err: errors.New(
			"error occurred while encoding a value: out of range integral type conversion attempted")}
	}
	return int64(value), nil //nolint:gosec // checked above
}

// orgID reads an organization id stored as text.
func orgID(op, value string) (ids.OrgID, error) {
	id, err := ids.Parse[ids.Org](value)
	if err != nil {
		return ids.OrgID{}, decodeErr(op, "%v", err)
	}
	return id, nil
}

// canonical is serde_json's `Value::to_string` of v: compact, keys sorted.
func canonical(op string, v any) (string, error) {
	text, err := wire.CanonicalValue(v)
	if err != nil {
		return "", dbErr(op, err)
	}
	return text, nil
}

// canonicalOpt is [canonical] of an optional value.
func canonicalOpt(op string, v opt.Val[any]) (opt.Val[string], error) {
	value, ok := v.Get()
	if !ok {
		return opt.None[string](), nil
	}
	text, err := canonical(op, value)
	if err != nil {
		return opt.None[string](), err
	}
	return opt.Some(text), nil
}

// jsonValue reads JSON text; invalid JSON is a decode error.
func jsonValue(op, text string) (any, error) {
	v, err := wire.DecodeAny([]byte(text))
	if err != nil {
		return nil, decodeErr(op, "%v", err)
	}
	return v, nil
}

// Accept accepts op for this organization: its idempotency receipt, the
// operation, audit and an outbox message are written in this transaction
// and commit together (I01). Nothing reaches a cluster here.
func (t *Tenant) Accept(ctx context.Context, op NewOperation, audit NewAudit, idempotency opt.Val[IdempotencyKey]) (Accepted, error) {
	const name = "accept an operation"
	id := ids.New[ids.Operation]()
	now := t.store.now()
	org := t.org.String()
	if op.InputHash == nil {
		op.InputHash = []byte{} // an empty hash, not NULL, as Rust bound it
	}
	if k, ok := idempotency.Get(); ok {
		written, err := exec(ctx, t.tx, name, upsertReceipt,
			org, k.Actor, op.Kind, k.Key, op.InputHash, id, now, clock.SaturatingAdd(now, millis(k.TTL)))
		if err != nil {
			return nil, err
		}
		if written == 0 {
			var hash []byte
			var existing ids.OperationID
			if err := queryOne(ctx, t.tx, name, selectReceipt, []any{&hash, &existing},
				org, k.Actor, op.Kind, k.Key); err != nil {
				return nil, err
			}
			if bytes.Equal(hash, op.InputHash) {
				return AcceptedReplayed{ID: existing}, nil
			}
			return AcceptedKeyReused{ID: existing}, nil
		}
	}
	if err := t.insertOperation(ctx, id, op, now); err != nil {
		return nil, err
	}
	audit.OrgID = opt.Some(t.org)
	if _, err := audit.insert(ctx, t.tx, now); err != nil {
		return nil, err
	}
	message, err := canonical(name, map[string]any{"operation_id": id, "kind": op.Kind})
	if err != nil {
		return nil, err
	}
	if _, err := exec(ctx, t.tx, name, insertOutbox,
		uuid.Must(uuid.NewV7()), org, id, op.Topic, message, now); err != nil {
		return nil, err
	}
	return AcceptedNew{ID: id}, nil
}

func (t *Tenant) insertOperation(ctx context.Context, id ids.OperationID, op NewOperation, now int64) error {
	const name = "insert an operation"
	generation := opt.None[int64]()
	if g, ok := op.Generation.Get(); ok {
		n, err := signed(name, g)
		if err != nil {
			return err
		}
		generation = opt.Some(n)
	}
	payload, err := canonical(name, op.Payload)
	if err != nil {
		return err
	}
	project, target := opt.None[ids.ProjectID](), opt.None[ids.TargetID]()
	if tg, ok := op.Target.Get(); ok {
		project, target = opt.Some(tg.Project), opt.Some(tg.Target)
	}
	_, err = exec(ctx, t.tx, name, insertOperation,
		id, t.org.String(), project.Ptr(), target.Ptr(), op.Kind, op.LifecycleUID.Ptr(), generation.Ptr(),
		op.InputHash, payload, op.RequestedBy, now, op.DeadlineAt.Ptr())
	return err
}

// ClaimOperation claims the next due, unleased operation of one of kinds
// for worker, leased for lease by the database clock. The claim raises the
// fence. False when nothing is due.
func (s *Store) ClaimOperation(ctx context.Context, worker string, kinds []string, lease time.Duration) (Claim, bool, error) {
	const op = "claim an operation"
	type claimRow struct {
		id      ids.OperationID
		org     string
		kind    string
		attempt int32
		fence   int64
	}
	if kinds == nil {
		kinds = []string{}
	}
	r, ok, err := queryOpt(ctx, s.db, op, claimOperation, func(row pgx.CollectableRow) (claimRow, error) {
		var r claimRow
		err := row.Scan(&r.id, &r.org, &r.kind, &r.attempt, &r.fence)
		return r, err
	}, worker, millis(lease), kinds)
	if err != nil || !ok {
		return Claim{}, false, err
	}
	org, err := orgID(op, r.org)
	if err != nil {
		return Claim{}, false, err
	}
	return Claim{ID: r.id, Org: org, Kind: r.kind, Attempt: r.attempt, Fence: r.fence}, true, nil
}

// FinishOperation settles claim in phase. False when the fence moved on:
// another worker owns the operation now and this result must be dropped.
func (s *Store) FinishOperation(ctx context.Context, claim Claim, phase string, errorCode opt.Val[string]) (bool, error) {
	n, err := exec(ctx, s.db, "finish an operation", finishOperation, claim.ID, claim.Fence, phase, errorCode.Ptr())
	return n == 1, err
}

// RetryOperation gives claim back to be retried after after. False when
// fenced off.
func (s *Store) RetryOperation(ctx context.Context, claim Claim, after time.Duration, errorCode string) (bool, error) {
	n, err := exec(ctx, s.db, "retry an operation", retryOperation, claim.ID, claim.Fence, millis(after), errorCode)
	return n == 1, err
}

// RenewLease extends the lease of claim. False when fenced off: stop
// working.
func (s *Store) RenewLease(ctx context.Context, claim Claim, lease time.Duration) (bool, error) {
	n, err := exec(ctx, s.db, "renew a lease", renewLease, claim.ID, claim.Fence, millis(lease))
	return n == 1, err
}

// RecordEvent appends an event to the history of claim's operation and
// returns its sequence number; false when fenced off.
func (s *Store) RecordEvent(ctx context.Context, claim Claim, kind string, data opt.Val[any]) (seq int64, recorded bool, err error) {
	const op = "record an operation event"
	text, err := canonicalOpt(op, data)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, false, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	seq, ok, err := queryOpt(ctx, tx, op, nextEvent, pgx.RowTo[int64], claim.ID, claim.Fence)
	if err != nil || !ok {
		return 0, false, err
	}
	if _, err := exec(ctx, tx, op, insertEvent, claim.ID, seq, kind, text.Ptr(), s.now()); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, dbErr(op, err)
	}
	return seq, true, nil
}

// rollback ends tx unless it was committed; the Go form of sqlx's rollback
// on drop.
func rollback(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return dbErr("roll back", err)
	}
	return nil
}

// TakeOutbox hands out up to limit pending outbox messages, hidden from
// other callers for visibility. A message comes back until
// [Store.OutboxDelivered]: delivery is at-least-once.
func (s *Store) TakeOutbox(ctx context.Context, limit int64, visibility time.Duration) ([]OutboxMessage, error) {
	const op = "take outbox messages"
	type outboxRow struct {
		id        uuid.UUID
		org       string
		operation *ids.OperationID
		topic     string
		payload   string
		attempts  int32
		createdAt int64
	}
	rows, err := queryAll(ctx, s.db, op, takeOutbox, func(row pgx.CollectableRow) (outboxRow, error) {
		var r outboxRow
		err := row.Scan(&r.id, &r.org, &r.operation, &r.topic, &r.payload, &r.attempts, &r.createdAt)
		return r, err
	}, limit, millis(visibility))
	if err != nil {
		return nil, err
	}
	out := make([]OutboxMessage, 0, len(rows))
	for _, r := range rows {
		org, err := orgID(op, r.org)
		if err != nil {
			return nil, err
		}
		payload, err := jsonValue(op, r.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, OutboxMessage{
			ID: r.id, Org: org, Operation: opt.FromPtr(r.operation), Topic: r.topic,
			Payload: payload, Attempts: r.attempts, CreatedAt: r.createdAt,
		})
	}
	return out, nil
}

// OutboxDelivered marks an outbox message delivered. False when it already
// was.
func (s *Store) OutboxDelivered(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := exec(ctx, s.db, "mark an outbox message delivered", outboxDelivered, id)
	return n == 1, err
}

// Receive records a provider delivery for org. The body hash is computed by
// the database; only the hash is kept.
func (s *Store) Receive(ctx context.Context, org ids.OrgID, provider, delivery string, body []byte) (Received, error) {
	const op = "receive a delivery"
	if body == nil {
		body = []byte{}
	}
	receipt, inserted, err := queryOpt(ctx, s.db, op, insertInbox, pgx.RowTo[uuid.UUID],
		provider, delivery, org.String(), body, uuid.Must(uuid.NewV7()), s.now())
	if err != nil {
		return nil, err
	}
	if inserted {
		return ReceivedNew{Receipt: receipt}, nil
	}
	var same bool
	if err := queryOne(ctx, s.db, op, selectInbox, []any{&receipt, &same},
		provider, delivery, org.String(), body); err != nil {
		return nil, err
	}
	if same {
		return ReceivedDuplicate{Receipt: receipt}, nil
	}
	return ReceivedChanged{}, nil
}

// protocolErr is sqlx's Error::Protocol: something the store refuses to
// write.
func protocolErr(op, format string, args ...any) error {
	return DatabaseError{Op: op, Err: fmt.Errorf("encountered unexpected or invalid data: "+format, args...)}
}

// scanID reads a one-column row of a uuid.
func scanID[K ids.Kind](row pgx.CollectableRow) (ids.ID[K], error) {
	var id ids.ID[K]
	err := row.Scan(&id)
	return id, err
}

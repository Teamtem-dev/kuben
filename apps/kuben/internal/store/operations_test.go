package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from operations.rs.

const deployKind = "deploy"

// orgWithTarget is an organization with one project, environment, cluster,
// placement, application and target.
func orgWithTarget(t *testing.T, s *store.Store, slug string) (ids.OrgID, ids.ProjectID, ids.TargetID) {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, slug+"-shop"))
	app := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, app, placement))
	commit(t, tn)
	return o, project, tgt
}

func commit(t *testing.T, tn *store.Tenant) {
	t.Helper()
	if err := tn.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func deployOperation(project ids.ProjectID, tgt ids.TargetID, body string) store.NewOperation {
	return store.NewOperation{
		Kind:        deployKind,
		Target:      opt.Some(store.OperationTarget{Project: project, Target: tgt}),
		Generation:  opt.Some[uint64](1),
		InputHash:   []byte(body),
		Payload:     map[string]any{"release": body},
		RequestedBy: "user:alice",
		Topic:       "deploy.accepted",
	}
}

func deployAudit() store.NewAudit {
	return store.NewAudit{
		ActorKind: "user", ActorID: opt.Some("alice"), Action: "deploy.accepted", Outcome: "accepted",
	}
}

// count runs a `SELECT count(*)` with the given arguments on the pool.
func count(t *testing.T, s *store.Store, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.TestQueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func accept(t *testing.T, tn *store.Tenant, op store.NewOperation, key opt.Val[store.IdempotencyKey]) store.Accepted {
	t.Helper()
	a, err := tn.Accept(t.Context(), op, deployAudit(), key)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return a
}

func acceptedNew(t *testing.T, a store.Accepted) ids.OperationID {
	t.Helper()
	n, ok := a.(store.AcceptedNew)
	if !ok {
		t.Fatalf("not a new operation: %#v", a)
	}
	return n.ID
}

func TestAcceptanceIsOneTransactionAndIdempotent(t *testing.T) {
	s := pgtest.Store(t)
	o, project, tgt := orgWithTarget(t, s, "a")
	key := opt.Some(store.IdempotencyKey{Actor: "user:alice", Key: "k-1", TTL: time.Hour})

	// Not committed: nothing at all, not even the receipt.
	tn := tenant(t, s, o)
	lost := acceptedNew(t, accept(t, tn, deployOperation(project, tgt, "r1"), key))
	rollback(t, tn)
	if n := count(t, s, "SELECT count(*) FROM operations WHERE id = $1", lost); n != 0 {
		t.Fatalf("a rolled-back operation: %d", n)
	}

	tn = tenant(t, s, o)
	id := acceptedNew(t, accept(t, tn, deployOperation(project, tgt, "r1"), key))
	commit(t, tn)
	if n := count(t, s, "SELECT count(*) FROM operations WHERE id = $1", id); n != 1 {
		t.Fatalf("operations: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM outbox WHERE operation_id = $1", id); n != 1 {
		t.Fatalf("outbox: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM audit_events WHERE org_id = $1", o.String()); n != 1 {
		t.Fatalf("the audit record committed with the operation: %d", n)
	}

	tn = tenant(t, s, o)
	if a := accept(t, tn, deployOperation(project, tgt, "r1"), key); a != (store.AcceptedReplayed{ID: id}) {
		t.Fatalf("same key, same request: same operation: %#v", a)
	}
	if a := accept(t, tn, deployOperation(project, tgt, "r2"), key); a != (store.AcceptedKeyReused{ID: id}) {
		t.Fatalf("same key, another request: conflict: %#v", a)
	}
	commit(t, tn)
	if n := count(t, s, "SELECT count(*) FROM operations"); n != 1 {
		t.Fatalf("neither replay wrote an operation: %d", n)
	}
}

func claim(t *testing.T, s *store.Store, worker string, kinds ...string) (store.Claim, bool) {
	t.Helper()
	c, ok, err := s.ClaimOperation(t.Context(), worker, kinds, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return c, ok
}

func TestClaimsAreExclusive(t *testing.T) {
	s := pgtest.Store(t)
	o, project, tgt := orgWithTarget(t, s, "a")
	tn := tenant(t, s, o)
	for n := range 40 {
		accept(t, tn, deployOperation(project, tgt, fmt.Sprintf("r%d", n)), opt.None[store.IdempotencyKey]())
	}
	commit(t, tn)

	done := make([][]ids.OperationID, 8)
	failures := make([]error, len(done))
	var wg sync.WaitGroup
	for w := range done {
		wg.Go(func() { failures[w] = work(s, w, done) })
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("workers: %v", err)
	}
	seen := map[ids.OperationID]bool{}
	total := 0
	for _, ops := range done {
		for _, id := range ops {
			if seen[id] {
				t.Fatalf("claimed twice: %v", id)
			}
			seen[id] = true
			total++
		}
	}
	if total != 40 {
		t.Fatalf("every operation claimed once: %d", total)
	}
}

// work claims and finishes operations as worker w until none is due.
func work(s *store.Store, w int, done [][]ids.OperationID) error {
	ctx := context.Background()
	for {
		c, ok, err := s.ClaimOperation(ctx, fmt.Sprintf("w%d", w), []string{deployKind}, 30*time.Second)
		if err != nil || !ok {
			return err
		}
		finished, err := s.FinishOperation(ctx, c, "succeeded", opt.None[string]())
		if err != nil {
			return err
		}
		if !finished {
			return fmt.Errorf("the lease holder %d cannot finish %v", w, c.ID)
		}
		done[w] = append(done[w], c.ID)
	}
}

// A paused worker whose lease expired cannot write after a takeover.
func TestAPausedWorkerIsFencedOffAfterATakeover(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, project, tgt := orgWithTarget(t, s, "a")
	tn := tenant(t, s, o)
	accept(t, tn, deployOperation(project, tgt, "late"), opt.None[store.IdempotencyKey]())
	commit(t, tn)
	slow, ok := claim(t, s, "slow", deployKind)
	if !ok {
		t.Fatal("due")
	}
	if _, err := s.TestExec(ctx, "UPDATE operations SET lease_until = 0 WHERE id = $1", slow.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	fast, ok := claim(t, s, "fast", deployKind)
	if !ok || fast.ID != slow.ID {
		t.Fatalf("taken over: %+v, %v", fast, ok)
	}
	if fast.Fence <= slow.Fence {
		t.Fatalf("a new claim raises the fence: %d, %d", fast.Fence, slow.Fence)
	}
	if renewed := must[bool](t, "renew")(s.RenewLease(ctx, slow, 30*time.Second)); renewed {
		t.Fatal("a fenced-off worker renewed its lease")
	}
	if _, ok, err := s.RecordEvent(ctx, slow, "progress", opt.None[any]()); err != nil || ok {
		t.Fatalf("a fenced-off worker recorded an event: %v, %v", ok, err)
	}
	if finished := must[bool](t, "finish")(s.FinishOperation(ctx, slow, "succeeded", opt.None[string]())); finished {
		t.Fatal("a fenced-off worker finished")
	}
	for want, kind := range []string{"applied", "verified"} {
		seq, ok, err := s.RecordEvent(ctx, fast, kind, opt.None[any]())
		if err != nil || !ok || seq != int64(want+1) {
			t.Fatalf("event %s: %d, %v, %v", kind, seq, ok, err)
		}
	}
	if _, err := s.TestExec(ctx, "UPDATE operation_events SET kind = 'forged'"); err == nil {
		t.Fatal("history is append-only")
	}
	if finished := must[bool](t, "finish")(s.FinishOperation(ctx, fast, "succeeded", opt.None[string]())); !finished {
		t.Fatal("the lease holder cannot finish")
	}
	if _, ok := claim(t, s, "any", deployKind); ok {
		t.Fatal("finished operations are never claimed again")
	}
}

func takeOutbox(t *testing.T, s *store.Store) []store.OutboxMessage {
	t.Helper()
	return must[[]store.OutboxMessage](t, "take")(s.TakeOutbox(t.Context(), 10, time.Minute))
}

func TestOutboxDeliversAtLeastOnce(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, project, tgt := orgWithTarget(t, s, "a")
	tn := tenant(t, s, o)
	id := acceptedNew(t, accept(t, tn, deployOperation(project, tgt, "r1"), opt.None[store.IdempotencyKey]()))
	commit(t, tn)

	first := takeOutbox(t, s)
	if len(first) != 1 {
		t.Fatalf("first: %+v", first)
	}
	if op, _ := first[0].Operation.Get(); op != id {
		t.Fatalf("operation: %+v", first[0])
	}
	if m, ok := first[0].Payload.(map[string]any); !ok || m["kind"] != deployKind {
		t.Fatalf("payload: %#v", first[0].Payload)
	}
	if again := takeOutbox(t, s); len(again) != 0 {
		t.Fatalf("hidden while a delivery is in progress: %+v", again)
	}

	// The acknowledgement was lost: the message comes back.
	if _, err := s.TestExec(ctx, "UPDATE outbox SET available_at = 0"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	again := takeOutbox(t, s)
	if len(again) != 1 || again[0].Attempts != 2 {
		t.Fatalf("again: %+v", again)
	}
	if ok := must[bool](t, "ack")(s.OutboxDelivered(ctx, again[0].ID)); !ok {
		t.Fatal("ack")
	}
	if ok := must[bool](t, "ack twice")(s.OutboxDelivered(ctx, again[0].ID)); ok {
		t.Fatal("ack twice")
	}
	if _, err := s.TestExec(ctx, "UPDATE outbox SET available_at = 0"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if left := takeOutbox(t, s); len(left) != 0 {
		t.Fatalf("a delivered message came back: %+v", left)
	}
}

func TestInboxDeduplicatesDeliveries(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "a", "A")
	b := org(t, s, "b", "B")
	receive := func(o ids.OrgID, provider, body string) store.Received {
		t.Helper()
		return must[store.Received](t, "receive")(s.Receive(ctx, o, provider, "d-1", []byte(body)))
	}
	first, ok := receive(a, "github", `{"ref":"main"}`).(store.ReceivedNew)
	if !ok {
		t.Fatal("a first delivery is new")
	}
	if r := receive(a, "github", `{"ref":"main"}`); r != store.ReceivedDuplicate(first) {
		t.Fatalf("again: %#v", r)
	}
	if r := receive(a, "github", `{"ref":"evil"}`); r != (store.ReceivedChanged{}) {
		t.Fatalf("same delivery id, another body: %#v", r)
	}
	if r := receive(b, "github", `{"ref":"main"}`); r != (store.ReceivedChanged{}) {
		t.Fatalf("same delivery id, another organization: %#v", r)
	}
	if _, ok := receive(a, "gitlab", "x").(store.ReceivedNew); !ok {
		t.Fatal("another provider")
	}
}

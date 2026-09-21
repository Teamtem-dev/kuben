package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from repo/acceptance.rs: M1 acceptance on PostgreSQL (plan §18.1
// exit criteria, §19.1 invariants): concurrent A/B deploys, the tenant
// context on a shared connection pool, and the rollback and promotion
// policy.

// plannedPanic is what a request handler that panics mid-transaction says.
const plannedPanic = "a handler panics mid-transaction"

// carriedOut is the events of a run that is carried out and verified.
var carriedOut = []run.Event{
	run.EventReadyForDelivery, run.EventAcceptedByCluster, run.EventPreflightStarted, run.EventPreflightPassed,
	run.EventApplied, run.EventVerified,
}

// The targets of an acceptance shop.
const (
	prod    = 0
	staging = 1
)

// acceptanceShop is one organization's app `web` on production and staging
// (one placement each on cluster `primary`), two releases and a
// configuration revision per target.
type acceptanceShop struct {
	org           ids.OrgID
	project       ids.ProjectID
	projectSlug   string
	targets       [2]ids.TargetID
	lifecycleUIDs [2]uuid.UUID
	releases      [2]ids.ReleaseID
	revisions     [2]ids.ConfigRevisionID
}

func newAcceptanceShop(t *testing.T, s *store.Store, slug string) acceptanceShop {
	t.Helper()
	ctx := t.Context()
	sh := acceptanceShop{org: org(t, s, slug, slug), projectSlug: slug + "-shop"}
	tn := tenant(t, s, sh.org)
	sh.project = must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, sh.projectSlug, "Shop"))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "primary"))
	application := must[ids.ApplicationID](t, "application")(tn.CreateApplication(ctx, sh.project, "web", "Web"))
	for i, env := range []string{"prod", "staging"} {
		environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, sh.project, env, env, env == "prod"))
		placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, sh.project, environment, cluster,
			"kb-"+slug+"-"+env))
		sh.targets[i] = must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, sh.project, application, placement))
	}
	for i, n := range []byte{1, 2} {
		sh.releases[i], _ = mustCreate(t)(tn.CreateRelease(ctx, sh.project, store.PortableRelease{
			Application:     application,
			Artifacts:       map[string]artifact.Digest{"web": digestOf(t, n)},
			ProcessContract: map[string]any{},
			PortableConfig:  map[string]any{},
			RendererSchema:  1,
			Source:          opt.Some[any](map[string]any{"image_repository": "ghcr.io/acme/web"}),
			CreatedBy:       "user:alice",
		}))
	}
	config := map[string]any{"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}}}
	for i, tgt := range sh.targets {
		sh.revisions[i] = configRevision(t, tn, sh.project, tgt, config).ID
		state, ok, err := tn.TargetState(ctx, tgt)
		if err != nil || !ok {
			t.Fatalf("target: %v, %v", ok, err)
		}
		sh.lifecycleUIDs[i] = state.LifecycleUID
	}
	commit(t, tn)
	return sh
}

// run is a run of release on target on expecting expected.
func (sh acceptanceShop) run(on, release int, expected uint64, reason store.RunReason, hash string) store.StartDeployment {
	return store.StartDeployment{
		Project: sh.project, Target: sh.targets[on], Release: sh.releases[release], ConfigRevision: sh.revisions[on],
		ExpectedGeneration: target.Generation(expected), LifecycleUID: sh.lifecycleUIDs[on], Reason: reason,
		RequestedBy: "user:alice", InputHash: []byte(hash),
	}
}

// startAccepted starts req in its own transaction, as a request handler
// does, without failing the test: the racers run on other goroutines.
func startAccepted(ctx context.Context, s *store.Store, o ids.OrgID, req store.StartDeployment) (started store.Started, err error) {
	tn, err := s.Tenant(ctx, o)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, tn.Rollback(ctx)) }()
	started, err = tn.StartDeployment(ctx, req, deploymentAudit(), opt.None[store.IdempotencyKey]())
	if err != nil {
		return nil, err
	}
	return started, tn.Commit(ctx)
}

// settle plays the materializer: it claims the one due run, moves it
// through events under the claim's fence, and settles the operation.
func settle(t *testing.T, s *store.Store, r ids.DeploymentRunID, events []run.Event) {
	t.Helper()
	c, ok := claim(t, s, "acceptance", store.RunKind)
	if !ok {
		t.Fatal("a due run")
	}
	outcome := "succeeded"
	for _, event := range events {
		switch a := advance(t, s, c, r, event).(type) {
		case store.AdvanceMoved:
			if a.Phase == run.Failed {
				outcome = "failed"
			}
		case store.AdvanceIllegal, store.AdvanceFenced:
			t.Fatalf("%s: %#v", event, a)
		}
	}
	if !must[bool](t, "finish")(s.FinishOperation(t.Context(), c, outcome, opt.None[string]())) {
		t.Fatal("the claim still holds")
	}
}

// Criterion "A/B race" (I05, I01): racers that expect the same generation
// start at once; the target's row lock lets exactly one in, the others are
// told the generation moved, and nothing of theirs is left behind.
func TestConcurrentDeploysOfOneGenerationLetExactlyOneIn(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	sh := newAcceptanceShop(t, s, "a")
	results := make([]store.Started, 8)
	failures := make([]error, len(results))
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i], failures[i] = startAccepted(ctx, s, sh.org,
				sh.run(prod, i%2, 0, store.ReasonDeploy, fmt.Sprintf("racer-%d", i)))
		})
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("racers: %v", err)
	}
	won, moved := 0, 0
	for _, r := range results {
		switch {
		case r == store.StartedRejected{Reject: target.GenerationMoved{Current: 1, Expected: 0}}:
			moved++
		default:
			a, ok := r.(store.StartedAccepted)
			if !ok || a.Generation != 1 {
				t.Fatalf("unexpected: %#v", r)
			}
			won++
		}
	}
	if won != 1 || moved != 7 {
		t.Fatalf("exactly one racer wins: %d won, %d moved", won, moved)
	}

	tn := tenant(t, s, sh.org)
	state, ok, err := tn.TargetState(ctx, sh.targets[prod])
	if err != nil || !ok || state.DesiredGeneration != 1 {
		t.Fatalf("state: %+v, %v, %v", state, ok, err)
	}
	if runs := must[[]store.RunRecord](t, "runs")(tn.Runs(ctx, sh.targets[prod], 10)); len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	rollback(t, tn)
	if n := count(t, s, "SELECT count(*) FROM operations"); n != 1 {
		t.Fatalf("operations: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM outbox"); n != 1 {
		t.Fatalf("outbox: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM audit_events WHERE action = 'deployment.accepted'"); n != 1 {
		t.Fatalf("one accepted intent, one audit record; the losers wrote nothing: %d", n)
	}
}

// Criterion "tenant pool reuse" (I21): many transactions of two
// organizations share the pool's four connections at once, as the server's
// requests do; some commit, some fail on an error, some are dropped by a
// panicking handler. Each sees only its own organization, and afterwards no
// connection carries a tenant into its next transaction.
func TestTheTenantContextNeverOutlivesItsTransactionOnASharedPool(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	shops := [2]acceptanceShop{newAcceptanceShop(t, s, "a"), newAcceptanceShop(t, s, "b")}
	// The test server connects as a superuser, which bypasses row-level
	// security; probe as an ordinary role, as the server runs in production.
	var schema string
	if err := s.TestQueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := s.TestExec(ctx, "DO $$ BEGIN CREATE ROLE kuben_rls_probe NOLOGIN; "+
		"EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$; "+
		"GRANT USAGE ON SCHEMA "+ident+" TO kuben_rls_probe; "+
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA "+ident+" TO kuben_rls_probe;"); err != nil {
		t.Fatalf("probe role: %v", err)
	}

	panics := make([]any, 24)
	failures := make([]error, len(panics))
	var wg sync.WaitGroup
	for i := range panics {
		sh := shops[i%2]
		wg.Go(func() {
			defer func() { panics[i] = recover() }()
			failures[i] = handle(ctx, s, sh.org, sh.projectSlug, i%3)
		})
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("handlers: %v", err)
	}
	panicked := 0
	for _, p := range panics {
		if p == nil {
			continue
		}
		if p != plannedPanic {
			t.Fatalf("only the planned panics: %v", p)
		}
		panicked++
	}
	if panicked != 8 {
		t.Fatalf("panics: %d", panicked)
	}

	// Every connection of the pool, next transaction, no tenant: nothing.
	var conns []interface{ Release() }
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	for range 4 {
		conn, err := s.TestAcquire(ctx)
		if err != nil {
			t.Fatalf("connection: %v", err)
		}
		conns = append(conns, conn)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE kuben_rls_probe"); err != nil {
			t.Fatalf("role: %v", err)
		}
		var visible int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&visible); err != nil || visible != 0 {
			t.Fatalf("a tenant context outlived its transaction: %d, %v", visible, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}

// handle is one request of the pool test: it reads the projects as the
// probe role, then commits (0), fails on an error (1) or panics (2). The
// deferred rollback is what a server does for a request that did not
// commit, the Go form of Rust's rollback on drop.
func handle(ctx context.Context, s *store.Store, o ids.OrgID, own string, ending int) (err error) {
	tn, err := s.Tenant(ctx, o)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tn.Rollback(ctx)) }()
	if _, err := tn.TestExec(ctx, "SET LOCAL ROLE kuben_rls_probe"); err != nil {
		return err
	}
	rows, err := tn.TestQuery(ctx, "SELECT slug FROM projects")
	if err != nil {
		return err
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if diff := cmp.Diff([]string{own}, slugs); diff != "" {
		return fmt.Errorf("only its own organization (-want +got):\n%s", diff)
	}
	switch ending {
	case 0:
		return tn.Commit(ctx)
	case 1:
		if _, err := tn.TestExec(ctx, "SELECT no_such_column FROM projects"); err == nil {
			return errors.New("the transaction is not aborted")
		}
		return nil
	default:
		panic(plannedPanic)
	}
}

// Criterion "rollback policy" (I08, I11): a rollback and a promotion reuse
// existing releases (no build, no new release), a rollback pins the target,
// and a failed deploy keeps its failure as its outcome after the rollback
// that recovered from it succeeds: the rollback takes the failed run's
// rights (it is superseded and can no longer recover), never its outcome.
func TestRollbackAndPromotionReuseReleasesAndAFailedRunStaysFailed(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	sh := newAcceptanceShop(t, s, "a")
	none := opt.None[store.IdempotencyKey]()
	first := acceptedRun(t, startRun(t, s, sh.org, sh.run(prod, 0, 0, store.ReasonDeploy, "deploy-1"), none)).Run
	settle(t, s, first, carriedOut)
	second := acceptedRun(t, startRun(t, s, sh.org, sh.run(prod, 1, 1, store.ReasonDeploy, "deploy-2"), none)).Run
	settle(t, s, second, []run.Event{run.EventReadyForDelivery, run.EventFailed})
	rolledBack := acceptedRun(t, startRun(t, s, sh.org, sh.run(prod, 0, 2, store.ReasonRollback, "rollback-1"), none)).Run
	settle(t, s, rolledBack, carriedOut)
	promotion := acceptedRun(t, startRun(t, s, sh.org, sh.run(staging, 0, 0, store.ReasonPromotion, "promote-1"), none)).Run
	settle(t, s, promotion, carriedOut)

	tn := tenant(t, s, sh.org)
	runs := must[[]store.RunRecord](t, "runs")(tn.Runs(ctx, sh.targets[prod], 10))
	type entry struct {
		Generation target.Generation
		Reason     string
		Phase      run.Phase
		Outcome    string
	}
	var history []entry
	for _, r := range runs {
		history = append(history, entry{r.Generation, r.Reason, r.Phase, r.Outcome.Or("")})
	}
	if diff := cmp.Diff([]entry{
		{3, "rollback", run.Succeeded, "succeeded"},
		{2, "deploy", run.Superseded, "failed"},
		{1, "deploy", run.Succeeded, "succeeded"},
	}, history); diff != "" {
		t.Fatalf("the rollback took the failed deploy's rights, never its failure (-want +got):\n%s", diff)
	}
	if runs[0].Release != sh.releases[0] {
		t.Fatal("the rollback runs release 1 again")
	}
	prodState, ok, err := tn.TargetState(ctx, sh.targets[prod])
	if err != nil || !ok || prodState.Policy != target.Pinned {
		t.Fatalf("a rollback pins the target: %+v, %v, %v", prodState, ok, err)
	}

	stagingRuns := must[[]store.RunRecord](t, "runs")(tn.Runs(ctx, sh.targets[staging], 10))
	if len(stagingRuns) != 1 || stagingRuns[0].Reason != "promotion" || stagingRuns[0].Release != sh.releases[0] ||
		stagingRuns[0].Phase != run.Succeeded {
		t.Fatalf("the same release on another target: %+v", stagingRuns)
	}
	stagingState, ok, err := tn.TargetState(ctx, sh.targets[staging])
	if err != nil || !ok || stagingState.Policy != target.Auto {
		t.Fatalf("a promotion does not pin: %+v, %v, %v", stagingState, ok, err)
	}
	rollback(t, tn)
	if n := count(t, s, "SELECT count(*) FROM releases"); n != 2 {
		t.Fatalf("rollback and promotion made no release: nothing was built: %d", n)
	}
}

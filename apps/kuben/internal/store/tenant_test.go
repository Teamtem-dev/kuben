package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from product.rs.

// chain is the placement and target of a project with one environment,
// cluster and application.
type chain struct {
	placement ids.PlacementID
	target    ids.TargetID
}

func tenant(t *testing.T, s *store.Store, org ids.OrgID) *store.Tenant {
	t.Helper()
	tn, err := s.Tenant(t.Context(), org)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	// Rust rolled back on drop; after a commit this does nothing.
	t.Cleanup(func() {
		if err := tn.Rollback(context.Background()); err != nil {
			t.Errorf("rollback: %v", err)
		}
	})
	return tn
}

func must[T any](t *testing.T, what string) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
}

func org(t *testing.T, s *store.Store, slug, name string) ids.OrgID {
	t.Helper()
	o, err := s.CreateOrg(t.Context(), slug, name)
	if err != nil {
		t.Fatalf("org %s: %v", slug, err)
	}
	return o.ID
}

func newChain(t *testing.T, s *store.Store, o ids.OrgID, prefix string) chain {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, prefix+"-shop", "Shop"))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, prefix+"-eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster, prefix+"-shop-production"))
	application := must[ids.ApplicationID](t, "application")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	if err := tn.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return chain{placement: placement, target: tgt}
}

func TestTargetsBindOnlyWithinOneOrganizationAndProject(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "a", "A")
	b := org(t, s, "b", "B")
	chainA := newChain(t, s, a, "a")

	tn := tenant(t, s, a)
	state, ok, err := tn.TargetState(ctx, chainA.target)
	if err != nil || !ok {
		t.Fatalf("read: %v, %v", ok, err)
	}
	if state.DesiredGeneration != 0 || state.Policy != target.Auto {
		t.Fatalf("state: %+v", state)
	}
	projects := must[[]store.Project](t, "projects")(tn.Projects(ctx))
	var slugs []string
	for _, p := range projects {
		slugs = append(slugs, p.Slug)
	}
	if diff := cmp.Diff([]string{"a-shop"}, slugs); diff != "" {
		t.Fatalf("projects (-want +got):\n%s", diff)
	}
	rollback(t, tn)

	// Organization B cannot bind A's placement, even knowing its id.
	tn = tenant(t, s, b)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "b-shop", "Shop"))
	app := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	if _, err := tn.CreateTarget(ctx, project, app, chainA.placement); err == nil {
		t.Fatal("a placement of another organization")
	}
	rollback(t, tn)

	// Nor can another project of the same organization.
	tn = tenant(t, s, a)
	other := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "a-other", "Other"))
	app = must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, other, "api", "API"))
	if _, err := tn.CreateTarget(ctx, other, app, chainA.placement); err == nil {
		t.Fatal("a placement of another project")
	}
	rollback(t, tn)

	// B's transaction does not see A's target either.
	tn = tenant(t, s, b)
	if _, ok, err := tn.TargetState(ctx, chainA.target); err != nil || ok {
		t.Fatalf("B sees A's target: %v, %v", ok, err)
	}
}

func rollback(t *testing.T, tn *store.Tenant) {
	t.Helper()
	if err := tn.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

func TestRowLevelSecurityShowsOneOrganizationAndFailsClosed(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "a", "A")
	b := org(t, s, "b", "B")
	newChain(t, s, a, "a")
	newChain(t, s, b, "b")

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

	conn, err := s.TestAcquire(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	defer conn.Release()
	begin := func(tenant bool) pgx.Tx {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE kuben_rls_probe"); err != nil {
			t.Fatalf("role: %v", err)
		}
		if tenant {
			if _, err := tx.Exec(ctx, store.SetTenant, a.String()); err != nil {
				t.Fatalf("tenant: %v", err)
			}
		}
		return tx
	}

	tx := begin(true)
	rows, err := tx.Query(ctx, "SELECT slug FROM projects ORDER BY slug")
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if diff := cmp.Diff([]string{"a-shop"}, slugs); diff != "" {
		t.Fatalf("only organization A's rows (-want +got):\n%s", diff)
	}
	var targets int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM application_targets").Scan(&targets); err != nil || targets != 1 {
		t.Fatalf("targets: %d, %v", targets, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The same connection, next transaction, no tenant: nothing.
	tx = begin(false)
	var visible int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("the tenant setting outlived its transaction: %d, %v", visible, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Writing a row of another organization is refused.
	tx = begin(true)
	if _, err := tx.Exec(ctx, store.InsertProject,
		uuid.Must(uuid.NewV7()), b.String(), "smuggled", "Smuggled", time.Now().UnixMilli()); err == nil {
		t.Fatal("a row of organization B written as A")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// update runs one statement on id in its own tenant transaction.
func update(t *testing.T, s *store.Store, o ids.OrgID, sql string, id any) (uint64, error) {
	t.Helper()
	tn := tenant(t, s, o)
	n, err := tn.TestExec(t.Context(), sql, id)
	if err != nil {
		rollback(t, tn) // returns the connection now: the pool has four
		return 0, err
	}
	return n, tn.Commit(t.Context())
}

func TestPlacementsAndTargetsOnlyMoveForward(t *testing.T) {
	s := pgtest.Store(t)
	o := org(t, s, "a", "A")
	c := newChain(t, s, o, "a")

	for _, step := range []struct {
		sql, why string
		id       any
		ok       bool
	}{
		{"UPDATE environment_placements SET namespace = 'elsewhere' WHERE id = $1", "the namespace is part of the binding", c.placement, false},
		{"UPDATE environment_placements SET namespace_uid = 'uid-1', state = 'ready' WHERE id = $1", "record the namespace UID", c.placement, true},
		{"UPDATE environment_placements SET namespace_uid = 'uid-2' WHERE id = $1", "a recorded namespace UID never changes", c.placement, false},
		{"UPDATE application_targets SET desired_generation = 3 WHERE id = $1", "raise the generation", c.target, true},
		{"UPDATE application_targets SET desired_generation = 2 WHERE id = $1", "the generation never decreases", c.target, false},
		{"UPDATE application_targets SET lifecycle_uid = gen_random_uuid() WHERE id = $1", "the lifecycle UID is identity", c.target, false},
		{"UPDATE application_targets SET deleting = TRUE WHERE id = $1", "begin deletion", c.target, true},
		{"UPDATE application_targets SET deleting = FALSE WHERE id = $1", "a deleting target stays deleting", c.target, false},
	} {
		n, err := update(t, s, o, step.sql, step.id)
		switch {
		case step.ok && (err != nil || n != 1):
			t.Fatalf("%s: %d, %v", step.why, n, err)
		case !step.ok && err == nil:
			t.Fatalf("%s: the update was accepted", step.why)
		}
	}

	tn := tenant(t, s, o)
	state, ok, err := tn.TargetState(t.Context(), c.target)
	if err != nil || !ok {
		t.Fatalf("read: %v, %v", ok, err)
	}
	if state.DesiredGeneration != target.Generation(3) || !state.Deleting {
		t.Fatalf("state: %+v", state)
	}
}

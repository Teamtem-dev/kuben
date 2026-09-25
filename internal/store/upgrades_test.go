package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from upgrades.rs.

func TestUpgradesAreJournaledAndNewerSchemasRefused(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	latest := store.LatestMigration()
	state, err := s.SchemaState(ctx)
	if err != nil || state != (store.SchemaState{Applied: latest, Dirty: false}) {
		t.Fatalf("state: %+v, %v", state, err)
	}
	again, err := s.MigrateJournaled(ctx, "2.0.0")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if again != (store.Migrated{FromSchema: latest, ToSchema: latest, Resumed: false}) {
		t.Fatalf("migrated: %+v", again)
	}
	if _, err := s.MigrateJournaled(ctx, "2.0.1"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	versions, err := s.ServerVersions(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if diff := cmp.Diff([]string{"2.0.1", "2.0.0"}, versions); diff != "" {
		t.Fatalf("versions (-want +got):\n%s", diff)
	}

	// An upgrade a crash left `running` is closed and resumed.
	if _, err := s.TestExec(ctx, store.BeginRun,
		uuid.Must(uuid.NewV7()), "2.0.1", "2.1.0", latest, latest, time.Now().UnixMilli()); err != nil {
		t.Fatalf("run: %v", err)
	}
	resumed, err := s.MigrateJournaled(ctx, "2.1.0")
	if err != nil || !resumed.Resumed {
		t.Fatalf("resume: %+v, %v", resumed, err)
	}
	var open int64
	if err := s.TestQueryRow(ctx, "SELECT count(*) FROM upgrade_runs WHERE status = 'running'").Scan(&open); err != nil {
		t.Fatalf("count: %v", err)
	}
	if open != 0 {
		t.Fatalf("%d runs still running", open)
	}

	// A migration only a newer Kuben knows.
	if _, err := s.TestExec(ctx,
		`INSERT INTO _sqlx_migrations (version, description, installed_on, success, checksum, execution_time) `+
			`VALUES ($1, 'future', now(), true, '\x00', 0)`, latest+1); err != nil {
		t.Fatalf("future: %v", err)
	}
	_, err = s.GuardSchema(ctx)
	var ahead store.SchemaAheadError
	if !errors.As(err, &ahead) || ahead.Database != latest+1 || ahead.Binary != latest {
		t.Fatalf("guard: %v", err)
	}
	if n, err := s.ActiveOperations(ctx); err != nil || n != 0 {
		t.Fatalf("active operations: %d, %v", n, err)
	}
	if agents, err := s.AgentProtocols(ctx); err != nil || len(agents) != 0 {
		t.Fatalf("agents: %v, %v", agents, err)
	}
}

// Rust sorted server versions by Option<Version>, newest first; a text
// that is not a version is None and sorts last.
func TestServerVersionsSortAsRustOptions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.0.1", "2.0.0", 1},
		{"2.0.0-rc.1", "2.0.0", -1},
		{"dev", "2.0.0", -1},
		{"2.0.0", "dev", 1},
		{"dev", "nightly", 0},
	} {
		if got := store.CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// The journal keeps 2048 characters, not bytes (`chars().take(2048)`).
func TestTruncateCountsCharacters(t *testing.T) {
	if got := store.TruncateChars("héllo", 2); got != "hé" {
		t.Errorf("got %q", got)
	}
	if got := store.TruncateChars("hé", 5); got != "hé" {
		t.Errorf("got %q", got)
	}
}

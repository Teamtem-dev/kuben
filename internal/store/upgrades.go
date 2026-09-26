package store

// The upgrade journal and the facts an upgrade preflight reads (M4.8,
// migration 0026); the port of repo/upgrades.rs.
//
// [Store.MigrateJournaled] refuses a schema newer than this binary or a
// migration that failed half-way, writes a `running` journal row before the
// migrations when the journal exists, applies them, and settles the row.

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/upgrade"
)

const (
	hasMigrations = "SELECT to_regclass('_sqlx_migrations') IS NOT NULL"
	appliedSchema = "SELECT COALESCE(max(version) FILTER (WHERE success), 0), bool_or(NOT success) IS TRUE " +
		"FROM _sqlx_migrations"
	hasJournal     = "SELECT to_regclass('upgrade_runs') IS NOT NULL"
	hasVersions    = "SELECT to_regclass('server_versions') IS NOT NULL"
	selectVersions = "SELECT version FROM server_versions"
	beginRun       = "INSERT INTO upgrade_runs " +
		"(id, from_version, to_version, from_schema, to_schema, status, started_at) " +
		"VALUES ($1, $2, $3, $4, $5, 'running', $6)"
	settleRun = "UPDATE upgrade_runs SET status = $2, detail = $3, finished_at = $4, to_schema = $5 " +
		"WHERE id = $1"
	interruptedRuns = "SELECT id, to_version FROM upgrade_runs WHERE status = 'running'"
	seenVersion     = "INSERT INTO server_versions (version, schema, first_started_at, last_started_at) " +
		"VALUES ($1, $2, $3, $3) ON CONFLICT (version) DO UPDATE " +
		"SET schema = EXCLUDED.schema, last_started_at = EXCLUDED.last_started_at, " +
		"starts = server_versions.starts + 1"
	activeOperations = "SELECT count(*) FROM operations WHERE NOT done"
	agentProtocols   = "SELECT cluster_id::text, protocol_version FROM cluster_agents " +
		"WHERE revoked_at IS NULL AND linked_at IS NOT NULL ORDER BY cluster_id"
)

// detailLimit is how many characters of a failure the journal keeps.
const detailLimit = 2048

// SchemaState is the migrations a database has.
type SchemaState struct {
	// Applied is the newest migration applied; 0 for an empty database.
	Applied int64
	// Dirty is set when a migration failed half-way.
	Dirty bool
}

// Migrated is what [Store.MigrateJournaled] did.
type Migrated struct {
	FromSchema int64
	ToSchema   int64
	// Resumed is set when a run an earlier start left unfinished was
	// completed.
	Resumed bool
}

// AgentProtocol is the protocol version a linked, unrevoked agent
// negotiated, if it did.
type AgentProtocol struct {
	Cluster  string
	Protocol opt.Val[uint32]
}

// SchemaState is the migrations this database has.
func (s *Store) SchemaState(ctx context.Context) (SchemaState, error) {
	var exists bool
	if err := queryOne(ctx, s.db, "read the schema state", hasMigrations, []any{&exists}); err != nil {
		return SchemaState{}, err
	}
	if !exists {
		return SchemaState{Applied: 0, Dirty: false}, nil
	}
	var state SchemaState
	if err := queryOne(ctx, s.db, "read the schema state", appliedSchema, []any{&state.Applied, &state.Dirty}); err != nil {
		return SchemaState{}, err
	}
	return state, nil
}

// GuardSchema refuses a database this binary must not migrate.
func (s *Store) GuardSchema(ctx context.Context) (SchemaState, error) {
	state, err := s.SchemaState(ctx)
	if err != nil {
		return SchemaState{}, err
	}
	binary := LatestMigration()
	if state.Dirty {
		return SchemaState{}, DirtySchemaError{}
	}
	if state.Applied > binary {
		return SchemaState{}, SchemaAheadError{Database: state.Applied, Binary: binary}
	}
	return state, nil
}

// ServerVersions is every server version that started here, newest first
// (a text that is not a version sorts last).
func (s *Store) ServerVersions(ctx context.Context) ([]string, error) {
	var exists bool
	if err := queryOne(ctx, s.db, "read server versions", hasVersions, []any{&exists}); err != nil {
		return nil, err
	}
	if !exists {
		return []string{}, nil
	}
	versions, err := queryAll(ctx, s.db, "read server versions", selectVersions, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(versions, func(a, b string) int { return compareVersions(b, a) })
	return versions, nil
}

// compareVersions orders texts as Rust ordered Option<Version>: a text that
// is not a version (None) before any version.
func compareVersions(a, b string) int {
	va, okA := upgrade.ParseVersion(a)
	vb, okB := upgrade.ParseVersion(b)
	switch {
	case okA && okB:
		return va.Compare(vb)
	case okA:
		return 1
	case okB:
		return -1
	default:
		return 0
	}
}

// MigrateJournaled migrates as version, journaled: see the file comment.
func (s *Store) MigrateJournaled(ctx context.Context, version string) (Migrated, error) {
	before, err := s.GuardSchema(ctx)
	if err != nil {
		return Migrated{}, err
	}
	target := LatestMigration()
	var journal bool
	if err := queryOne(ctx, s.db, "read the upgrade journal", hasJournal, []any{&journal}); err != nil {
		return Migrated{}, err
	}
	resumed := false
	run := opt.None[uuid.UUID]()
	if journal {
		resumed, err = s.closeInterrupted(ctx, version, before.Applied)
		if err != nil {
			return Migrated{}, err
		}
		if before.Applied < target {
			previous, err := s.newestVersion(ctx)
			if err != nil {
				return Migrated{}, err
			}
			id, err := s.beginRun(ctx, previous, version, before.Applied, target, s.now())
			if err != nil {
				return Migrated{}, err
			}
			run = opt.Some(id)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		return Migrated{}, s.failRun(ctx, run, before.Applied, err)
	}
	now := s.now()
	if run.IsNone() && before.Applied < target {
		// The journal arrived with these migrations: record them now.
		id, err := s.beginRun(ctx, opt.None[string](), version, before.Applied, target, now)
		if err != nil {
			return Migrated{}, err
		}
		run = opt.Some(id)
	}
	if id, ok := run.Get(); ok {
		if err := s.settle(ctx, id, "succeeded", opt.None[string](), target); err != nil {
			return Migrated{}, err
		}
	}
	if _, err := exec(ctx, s.db, "record the server version", seenVersion, version, target, now); err != nil {
		return Migrated{}, err
	}
	return Migrated{FromSchema: before.Applied, ToSchema: target, Resumed: resumed}, nil
}

// closeInterrupted settles every run an earlier start left `running`.
func (s *Store) closeInterrupted(ctx context.Context, version string, schema int64) (bool, error) {
	type openRun struct {
		id uuid.UUID
		to string
	}
	open, err := queryAll(ctx, s.db, "read interrupted upgrades", interruptedRuns,
		func(row pgx.CollectableRow) (openRun, error) {
			var r openRun
			err := row.Scan(&r.id, &r.to)
			return r, err
		})
	if err != nil {
		return false, err
	}
	for _, r := range open {
		detail := "interrupted; resumed by " + version
		if err := s.settle(ctx, r.id, "failed", opt.Some(detail), schema); err != nil {
			return false, err
		}
		s.log.WarnContext(ctx, "resuming an interrupted upgrade",
			slog.String("run", r.id.String()), slog.String("interrupted", r.to), slog.String("version", version))
	}
	return len(open) > 0, nil
}

// newestVersion is the newest server version that started here.
func (s *Store) newestVersion(ctx context.Context) (opt.Val[string], error) {
	versions, err := s.ServerVersions(ctx)
	if err != nil || len(versions) == 0 {
		return opt.None[string](), err
	}
	return opt.Some(versions[0]), nil
}

// beginRun writes a `running` journal row.
func (s *Store) beginRun(ctx context.Context, previous opt.Val[string], version string, from, to, startedAt int64) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.UUID{}, dbErr("begin an upgrade run", err)
	}
	if _, err := exec(ctx, s.db, "begin an upgrade run", beginRun,
		id, previous.Ptr(), version, from, to, startedAt); err != nil {
		return uuid.UUID{}, err
	}
	return id, nil
}

// failRun records a failed migration on the run, if there is one, and
// returns the migration's error (with the journal's, should that fail too).
func (s *Store) failRun(ctx context.Context, run opt.Val[uuid.UUID], applied int64, migrateErr error) error {
	id, ok := run.Get()
	if !ok {
		return migrateErr
	}
	after := applied
	if state, err := s.SchemaState(ctx); err == nil {
		after = state.Applied
	}
	detail := truncateChars(migrateErr.Error(), detailLimit)
	if err := s.settle(ctx, id, "failed", opt.Some(detail), after); err != nil {
		// Rust ignored this failure; it is kept, after the migration's.
		return errors.Join(migrateErr, err)
	}
	return migrateErr
}

func (s *Store) settle(ctx context.Context, id uuid.UUID, status string, detail opt.Val[string], schema int64) error {
	_, err := exec(ctx, s.db, "settle an upgrade run", settleRun, id, status, detail.Ptr(), s.now(), schema)
	return err
}

// truncateChars keeps the first n characters of s (Rust's chars().take(n)).
func truncateChars(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// ActiveOperations is the number of operations not settled yet.
func (s *Store) ActiveOperations(ctx context.Context) (uint64, error) {
	var n int64
	if err := queryOne(ctx, s.db, "count active operations", activeOperations, []any{&n}); err != nil {
		return 0, err
	}
	return counter("count active operations", n)
}

// AgentProtocols is the cluster id and protocol version of every linked,
// unrevoked agent, by cluster id.
func (s *Store) AgentProtocols(ctx context.Context) ([]AgentProtocol, error) {
	return queryAll(ctx, s.db, "read agent protocols", agentProtocols,
		func(row pgx.CollectableRow) (AgentProtocol, error) {
			var cluster string
			var protocol *int32
			if err := row.Scan(&cluster, &protocol); err != nil {
				return AgentProtocol{}, err
			}
			a := AgentProtocol{Cluster: cluster}
			// A negative version is not one (Rust: u32::try_from(p).ok()).
			if protocol != nil && *protocol >= 0 {
				a.Protocol = opt.Some(uint32(*protocol)) //nolint:gosec // non-negative int32
			}
			return a, nil
		})
}

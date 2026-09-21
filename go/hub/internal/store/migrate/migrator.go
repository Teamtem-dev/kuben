package migrate

import (
	"context"
	"errors"
	"hash/crc32"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Table is sqlx's default ledger table; Kuben never changed it (no
// sqlx.toml), so both binaries read and write this one.
const Table = "_sqlx_migrations"

// The statements of sqlx-postgres/src/migrate.rs with the table name filled
// in. The ledger DDL keeps sqlx's whitespace; only its meaning matters, but
// there is no reason to differ.
const (
	currentDatabase = "SELECT current_database()"
	advisoryLock    = "SELECT pg_advisory_lock($1)"
	advisoryUnlock  = "SELECT pg_advisory_unlock($1)"
	ensureTable     = `
CREATE TABLE IF NOT EXISTS _sqlx_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    installed_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    success BOOLEAN NOT NULL,
    checksum BYTEA NOT NULL,
    execution_time BIGINT NOT NULL
);
                `
	dirtyVersion  = "SELECT version FROM _sqlx_migrations WHERE success = false ORDER BY version LIMIT 1"
	appliedList   = "SELECT version, checksum FROM _sqlx_migrations ORDER BY version"
	insertApplied = `
    INSERT INTO _sqlx_migrations ( version, description, success, checksum, execution_time )
    VALUES ( $1, $2, TRUE, $3, -1 )
                `
	updateDuration = `
    UPDATE _sqlx_migrations
    SET execution_time = $1
    WHERE version = $2
                `
)

// LockID is the advisory lock sqlx takes for the database named database:
// 0x3d32ad9e times the CRC-32/ISO-HDLC (IEEE) of the name, as an i64
// (sqlx-postgres migrate.rs `generate_lock_id`; the product fits: both
// factors are below 2^32 and the first below 2^30).
func LockID(database string) int64 {
	return 0x3d32ad9e * int64(crc32.ChecksumIEEE([]byte(database)))
}

// Migrator runs a fixed, sorted set of migrations.
type Migrator struct {
	migrations []Migration
}

// New is a migrator for ms (sorted by version then type, as sqlx sorts).
func New(ms []Migration) *Migrator {
	sorted := slices.Clone(ms)
	slices.SortStableFunc(sorted, compare)
	return &Migrator{migrations: sorted}
}

// Migrations is every migration, in the order Run applies them.
func (m *Migrator) Migrations() []Migration { return slices.Clone(m.migrations) }

// Latest is the newest version, or 0 without migrations.
func (m *Migrator) Latest() int64 {
	var latest int64
	for _, mig := range m.migrations {
		latest = max(latest, mig.Version)
	}
	return latest
}

type applied struct {
	version  int64
	checksum []byte
}

// Run applies the pending migrations on conn, after validating the applied
// ones, holding sqlx's advisory lock: sqlx's Migrator::run_direct with
// locking on, no target and no schemas to create.
//
// As in sqlx, a failure returns without releasing the lock (a session
// lock); the caller must close conn when Run fails, which releases it.
func (m *Migrator) Run(ctx context.Context, conn *pgx.Conn) error {
	lockID, err := lock(ctx, conn)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, ensureTable); err != nil {
		return &ExecuteError{Err: err}
	}
	var dirty int64
	switch err := conn.QueryRow(ctx, dirtyVersion).Scan(&dirty); {
	case err == nil:
		return &DirtyError{Version: dirty}
	case !errors.Is(err, pgx.ErrNoRows):
		return &ExecuteError{Err: err}
	}
	done, err := listApplied(ctx, conn)
	if err != nil {
		return err
	}
	if err := m.validate(done); err != nil {
		return err
	}
	byVersion := make(map[int64][]byte, len(done))
	for _, a := range done {
		byVersion[a.version] = a.checksum
	}
	for i := range m.migrations {
		mig := &m.migrations[i]
		if mig.Type.IsDown() {
			continue
		}
		if checksum, ok := byVersion[mig.Version]; ok {
			if !slices.Equal(mig.Checksum, checksum) {
				return &VersionMismatchError{Version: mig.Version}
			}
			continue
		}
		if err := apply(ctx, conn, mig); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, advisoryUnlock, lockID); err != nil {
		return &ExecuteError{Err: err}
	}
	return nil
}

// lock waits for the database's migration lock; sqlx names the database
// again on unlock, which is the same name on one connection.
func lock(ctx context.Context, conn *pgx.Conn) (int64, error) {
	var database string
	if err := conn.QueryRow(ctx, currentDatabase).Scan(&database); err != nil {
		return 0, &ExecuteError{Err: err}
	}
	id := LockID(database)
	if _, err := conn.Exec(ctx, advisoryLock, id); err != nil {
		return 0, &ExecuteError{Err: err}
	}
	return id, nil
}

func listApplied(ctx context.Context, conn *pgx.Conn) ([]applied, error) {
	rows, err := conn.Query(ctx, appliedList)
	if err != nil {
		return nil, &ExecuteError{Err: err}
	}
	done, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (applied, error) {
		var a applied
		err := row.Scan(&a.version, &a.checksum)
		return a, err
	})
	if err != nil {
		return nil, &ExecuteError{Err: err}
	}
	return done, nil
}

// validate is sqlx's validate_applied_migrations with ignore_missing off.
func (m *Migrator) validate(done []applied) error {
	for _, a := range done {
		if !slices.ContainsFunc(m.migrations, func(mig Migration) bool { return mig.Version == a.version }) {
			return &VersionMissingError{Version: a.version}
		}
	}
	return nil
}

// apply runs one migration and records it in the same transaction (unless
// it opted out), then stores how long it took: sqlx inserts the row with
// execution_time -1 and updates it after the commit.
func apply(ctx context.Context, conn *pgx.Conn, mig *Migration) error {
	start := time.Now() //nolint:forbidigo // a duration measurement for the ledger, not a decision
	if mig.NoTx {
		if err := execute(ctx, conn, mig); err != nil {
			return err
		}
	} else if err := inTransaction(ctx, conn, mig); err != nil {
		return err
	}
	elapsed := time.Since(start)
	if _, err := conn.Exec(ctx, updateDuration, elapsed.Nanoseconds(), mig.Version); err != nil {
		return &ExecuteError{Err: err}
	}
	return nil
}

func inTransaction(ctx context.Context, conn *pgx.Conn, mig *Migration) (err error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return &ExecuteError{Err: err}
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, &ExecuteError{Err: rbErr})
			}
		}
	}()
	if err := execute(ctx, tx, mig); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return &ExecuteError{Err: err}
	}
	return nil
}

// executor is a connection or a transaction.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// execute runs the migration's SQL and inserts its ledger row. The SQL has
// no arguments and goes out with the simple query protocol, as sqlx's
// `execute` of a plain string does: every statement of the file runs.
func execute(ctx context.Context, conn executor, mig *Migration) error {
	if _, err := conn.Exec(ctx, mig.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return &ExecuteMigrationError{Err: err, Version: mig.Version}
	}
	if _, err := conn.Exec(ctx, insertApplied, mig.Version, mig.Description, mig.Checksum); err != nil {
		return &ExecuteError{Err: err}
	}
	return nil
}

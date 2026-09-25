// Package store is the persistence layer on PostgreSQL (ADR-025): product
// state and durable operations (ADR-032), identity and audit. It replaces
// the Rust crate kuben-store, one Go file per Rust file (store.go and
// pool.go for src/db.rs, tenant.go for repo/product.rs, users.go for
// repo/users.rs, …) so the port can be read side by side. The SQL is the
// Rust constants' text, unchanged; the schema is migrations/ and the
// migration ledger is sqlx's own (see package migrate).
//
// Every repository test runs against a real PostgreSQL in a schema of its
// own (package pgtest, KUBEN_TEST_PG_URL).
package store

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/store/migrate"
	"github.com/Teamtem-dev/kuben/internal/store/migrations"
)

// Store is the handle to the PostgreSQL pool. It is safe for concurrent use
// and cheap to copy by pointer; [Store.Close] closes the pool for every copy.
type Store struct {
	db    pool
	clock clock.Clock
	log   *slog.Logger
}

// Pool settings sqlx had by default and Kuben relied on.
const (
	minConnections  = 2
	maxConnLifetime = 30 * time.Minute // sqlx max_lifetime
	maxConnIdleTime = 10 * time.Minute // sqlx idle_timeout
)

// Connect connects to PostgreSQL and runs the embedded migrations.
func Connect(ctx context.Context, cfg config.DatabaseCfg) (*Store, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	s, err := open(ctx, pc, cfg.MaxConnections)
	if err != nil {
		return nil, err
	}
	return s.migrated(ctx)
}

// ConnectUnmigrated connects without migrating: for tools that must not
// change the schema (a backup). The schema may be older than this binary's.
func ConnectUnmigrated(ctx context.Context, cfg config.DatabaseCfg) (*Store, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	return open(ctx, pc, cfg.MaxConnections)
}

// ConnectConfig connects with explicit options (for example a search_path
// per test, see package pgtest) and runs the migrations.
func ConnectConfig(ctx context.Context, pc *pgxpool.Config, maxConnections uint32) (*Store, error) {
	s, err := open(ctx, pc, maxConnections)
	if err != nil {
		return nil, err
	}
	return s.migrated(ctx)
}

// WithClock is the same store reading the time from c (tests).
func (s *Store) WithClock(c clock.Clock) *Store {
	copied := *s
	copied.clock = c
	return &copied
}

// WithLogger is the same store logging to l.
func (s *Store) WithLogger(l *slog.Logger) *Store {
	copied := *s
	copied.log = l
	return &copied
}

// Migrator is the migrator of the embedded migrations.
func Migrator() (*migrate.Migrator, error) {
	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		return nil, MigrationError{Err: err}
	}
	return migrate.New(ms), nil
}

// LatestMigration is the newest migration this binary carries.
func LatestMigration() int64 {
	m, err := Migrator()
	if err != nil {
		// The files are embedded at build time and the tests load them.
		return 0
	}
	return m.Latest()
}

// poolConfig checks the URL as Kuben 2.0 does and parses it.
func poolConfig(cfg config.DatabaseCfg) (*pgxpool.Config, error) {
	url := strings.TrimSpace(cfg.URL.Expose())
	if url == "" {
		return nil, NotConfiguredError{}
	}
	if strings.HasPrefix(url, "sqlite:") {
		return nil, SQLiteError{}
	}
	if !strings.HasPrefix(url, "postgres:") && !strings.HasPrefix(url, "postgresql:") {
		return nil, UnsupportedURLError{URL: cfg.URL.Expose()}
	}
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, dbErr("parse database url", err)
	}
	return pc, nil
}

// open builds the pool with sqlx's settings (at least 2 connections) and
// establishes one connection, as sqlx's PgPoolOptions::connect did.
func open(ctx context.Context, pc *pgxpool.Config, maxConnections uint32) (*Store, error) {
	limit := max(maxConnections, minConnections)
	pc.MaxConns = int32(min(limit, math.MaxInt32)) //nolint:gosec // bounded by the min above
	pc.MaxConnLifetime = maxConnLifetime
	pc.MaxConnIdleTime = maxConnIdleTime
	p, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, dbErr("connect", err)
	}
	s := &Store{db: pool{p: p}, clock: clock.System{}, log: slog.Default()}
	if err := s.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrated(ctx context.Context) (*Store, error) {
	if _, err := s.GuardSchema(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Migrate applies pending migrations (idempotent; safe to call on every
// boot), on one connection holding sqlx's advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	m, err := Migrator()
	if err != nil {
		return err
	}
	c, err := s.db.acquire(ctx)
	if err != nil {
		return MigrationError{Err: &migrate.ExecuteError{Err: err}}
	}
	if err := m.Run(ctx, c.Conn()); err != nil {
		// A failed run may still hold the session's advisory lock or an
		// aborted transaction: close the connection instead of pooling it.
		closeErr := c.Conn().Close(ctx)
		c.Release()
		if closeErr != nil {
			return errors.Join(MigrationError{Err: err}, dbErr("close the migration connection", closeErr))
		}
		return MigrationError{Err: err}
	}
	c.Release()
	return nil
}

// Ping is a lightweight liveness probe.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.db.Exec(ctx, "SELECT 1")
	return dbErr("ping", err)
}

// Backend is the backend name for logs and `/healthz/details`.
func (*Store) Backend() string { return "postgres" }

const roleBypassing = "SELECT rolname::text, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user" //nolint:gosec // SQL, not a credential

// RoleBypassingRowSecurity is the current role when it bypasses row-level
// security: a superuser or a `BYPASSRLS` role. Tenant isolation then rests
// on the queries alone (migration 0004). False for an ordinary role, as
// production should use.
func (s *Store) RoleBypassingRowSecurity(ctx context.Context) (string, bool, error) {
	var role string
	var bypasses bool
	err := s.db.QueryRow(ctx, roleBypassing).Scan(&role, &bypasses)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, dbErr("read the database role", err)
	}
	if !bypasses {
		return "", false, nil
	}
	return role, true, nil
}

// Close closes the pool. Part of the ordered shutdown (Invariant I-15).
func (s *Store) Close() { s.db.p.Close() }

// now is the store's clock in unix milliseconds.
func (s *Store) now() int64 { return s.clock.NowMs() }

// queryAll runs sql and reads every row with scan.
func queryAll[T any](ctx context.Context, q querier, op, sql string, scan func(pgx.CollectableRow) (T, error), args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, dbErr(op, err)
	}
	out, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, dbErr(op, err)
	}
	return out, nil
}

// queryOpt runs sql and reads its first row, if there is one (sqlx's
// fetch_optional).
func queryOpt[T any](ctx context.Context, q querier, op, sql string, scan func(pgx.CollectableRow) (T, error), args ...any) (T, bool, error) {
	var zero T
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return zero, false, dbErr(op, err)
	}
	out, err := pgx.CollectOneRow(rows, scan)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return zero, false, nil
	case err != nil:
		return zero, false, dbErr(op, err)
	}
	return out, true, nil
}

// queryOne reads the one row sql always returns (sqlx's fetch_one).
func queryOne(ctx context.Context, q querier, op, sql string, dest []any, args ...any) error {
	return dbErr(op, q.QueryRow(ctx, sql, args...).Scan(dest...))
}

// exec runs a statement and returns the rows it affected.
func exec(ctx context.Context, q querier, op, sql string, args ...any) (uint64, error) {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return 0, dbErr(op, err)
	}
	return uint64(tag.RowsAffected()), nil //nolint:gosec // a row count is never negative
}

// counter reads a counter column: `CHECK (>= 0)` in the schema, so a
// negative value is corruption, not something to clamp.
func counter(op string, value int64) (uint64, error) {
	if value < 0 {
		return 0, decodeErr(op, "counter %d is negative", value)
	}
	return uint64(value), nil
}

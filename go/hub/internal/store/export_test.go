package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Statements the tests run by hand, as the Rust tests did.
const (
	BeginRun      = beginRun
	SetTenant     = setTenant
	InsertProject = insertProject
)

// TruncateChars exposes truncateChars to the external tests.
func TruncateChars(s string, n int) string { return truncateChars(s, n) }

// CompareVersions exposes compareVersions to the external tests.
func CompareVersions(a, b string) int { return compareVersions(a, b) }

// TestExec runs a statement on the pool (Rust tests used store.pool()).
func (s *Store) TestExec(ctx context.Context, sql string, args ...any) (uint64, error) {
	return exec(ctx, s.db, "test", sql, args...)
}

// TestQueryRow runs a query on the pool.
func (s *Store) TestQueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.db.QueryRow(ctx, sql, args...)
}

// TestAcquire is one pooled connection, for tests that need several
// transactions on the same session.
func (s *Store) TestAcquire(ctx context.Context) (*pgxpool.Conn, error) { return s.db.acquire(ctx) }

// ReleaseContent exposes releaseContent to the external tests.
func ReleaseContent(r PortableRelease) (string, error) { return releaseContent(r) }

// PlanContent exposes planContent to the external tests.
func PlanContent(version string, capabilities, resources any) (string, error) {
	return planContent(version, capabilities, resources)
}

// RetentionBefore exposes retentionBefore to the external tests.
func RetentionBefore(now int64, days uint32) int64 { return retentionBefore(now, days) }

// TestQuery runs a query in the tenant's transaction.
func (t *Tenant) TestQuery(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

// TestExec runs a statement in the tenant's transaction (Rust tests used t.tx).
func (t *Tenant) TestExec(ctx context.Context, sql string, args ...any) (uint64, error) {
	return exec(ctx, t.tx, "test", sql, args...)
}

// TestQueryRow runs a query in the tenant's transaction.
func (t *Tenant) TestQueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

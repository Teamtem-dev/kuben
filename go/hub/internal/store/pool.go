package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acquireTimeout bounds the wait for a pooled connection, as sqlx's
// PgPoolOptions::acquire_timeout(10 s) did. pgxpool has no such option: the
// wait ends only with the caller's context, so every acquisition goes
// through [pool.acquire].
const acquireTimeout = 10 * time.Second

// errPoolTimedOut is sqlx's Error::PoolTimedOut.
var errPoolTimedOut = errors.New("pool timed out while waiting for an open connection")

// querier runs statements: the pool, one connection or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pool is a pgxpool whose acquisitions time out like sqlx's.
type pool struct{ p *pgxpool.Pool }

var _ querier = pool{}

func (p pool) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	bounded, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()
	c, err := p.p.Acquire(bounded)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", errPoolTimedOut, err)
		}
		return nil, err
	}
	return c, nil
}

// Exec runs one statement on a pooled connection.
func (p pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer c.Release()
	return c.Exec(ctx, sql, args...)
}

// Query runs a query; closing the rows returns the connection.
func (p pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &connRows{Rows: rows, release: sync.OnceFunc(c.Release)}, nil
}

// QueryRow runs a query for one row; scanning it returns the connection.
func (p pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c, err := p.acquire(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return connRow{row: c.QueryRow(ctx, sql, args...), release: c.Release}
}

// Begin starts a transaction; its commit or rollback returns the connection.
func (p pool) Begin(ctx context.Context) (pgx.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &connTx{Tx: tx, release: sync.OnceFunc(c.Release)}, nil
}

type connRows struct {
	pgx.Rows
	release func()
}

func (r *connRows) Close() {
	r.Rows.Close()
	r.release()
}

// Next returns the connection as soon as the last row is read, as pgxpool
// does, so a caller that reads to the end without Close does not leak it.
func (r *connRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

type connRow struct {
	row     pgx.Row
	release func()
}

func (r connRow) Scan(dest ...any) error {
	defer r.release()
	return r.row.Scan(dest...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type connTx struct {
	pgx.Tx
	release func()
}

func (t *connTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *connTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
}

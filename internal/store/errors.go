package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

// Error is a store failure: the variants of the Rust StoreError, with its
// messages. Callers match on SchemaAheadError and DirtySchemaError (with
// errors.As / errors.Is) and use [IsUniqueViolation] for duplicates.
//
//sumtype:decl
type Error interface {
	error
	storeError()
}

// DatabaseError is a failed statement or a row that could not be decoded
// (sqlx::Error). Op says which store call failed.
type DatabaseError struct {
	Op  string
	Err error
}

// MigrationError is a failed migration run (sqlx's MigrateError); Err is a
// migrate.Error.
type MigrationError struct{ Err error }

// UnsupportedURLError is a database URL of another kind than PostgreSQL.
type UnsupportedURLError struct{ URL string }

// SQLiteError is a SQLite URL: 1.x installations are not carried over.
type SQLiteError struct{}

// NotConfiguredError is an empty database URL.
type NotConfiguredError struct{}

// SchemaAheadError is a database a newer Kuben has migrated.
type SchemaAheadError struct{ Database, Binary int64 }

// DirtySchemaError is a database whose ledger records a failed migration.
type DirtySchemaError struct{}

func (DatabaseError) storeError()       {}
func (MigrationError) storeError()      {}
func (UnsupportedURLError) storeError() {}
func (SQLiteError) storeError()         {}
func (NotConfiguredError) storeError()  {}
func (SchemaAheadError) storeError()    {}
func (DirtySchemaError) storeError()    {}

func (e DatabaseError) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("database error: %v", e.Err)
	}
	return fmt.Sprintf("database error: %s: %v", e.Op, e.Err)
}

func (e MigrationError) Error() string { return fmt.Sprintf("migration error: %v", e.Err) }

func (e UnsupportedURLError) Error() string {
	return fmt.Sprintf("unsupported database url %s: Kuben keeps its data in PostgreSQL (postgres://…)", e.URL)
}

func (SQLiteError) Error() string {
	return "SQLite is no longer supported: Kuben keeps its data in PostgreSQL (ADR-025) and does not carry the data of a SQLite (1.x) installation over. Point database.url at an empty PostgreSQL server"
}

func (NotConfiguredError) Error() string {
	return "database.url is not set: Kuben keeps its data in PostgreSQL. For a local server run `docker run -d --name kuben-postgres -e POSTGRES_PASSWORD=kuben -p 5432:5432 postgres:17-alpine` and set KUBEN_DATABASE__URL=postgres://postgres:kuben@localhost:5432/postgres"
}

func (e SchemaAheadError) Error() string {
	return fmt.Sprintf("the database is at schema %d, newer than this Kuben knows (%d): it was upgraded by a newer Kuben; run that version or newer, or restore the backup taken before the upgrade", e.Database, e.Binary)
}

func (DirtySchemaError) Error() string {
	return "a database migration failed half-way (_sqlx_migrations has an unsuccessful row): restore the backup taken before the upgrade"
}

// Unwrap is the driver's error.
func (e DatabaseError) Unwrap() error { return e.Err }

// Unwrap is the migrator's error.
func (e MigrationError) Unwrap() error { return e.Err }

// dbErr wraps a driver error; nil stays nil.
func dbErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return DatabaseError{Op: op, Err: err}
}

// decodeErr is a column value the store cannot read (sqlx::Error::Decode).
func decodeErr(op string, format string, args ...any) error {
	return DatabaseError{Op: op, Err: fmt.Errorf("error occurred while decoding: "+format, args...)}
}

// IsUniqueViolation reports whether a unique constraint refused the write:
// what it adds exists already (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// Kerr is the domain error a store failure becomes at the API: always
// internal, with the store's message as the detail (Rust's
// `From<StoreError> for kuben_core::Error`).
func Kerr(err Error) *kerrors.Error {
	return kerrors.New(kerrors.Internal, "%s", err.Error())
}

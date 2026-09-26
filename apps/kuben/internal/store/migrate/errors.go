package migrate

import "fmt"

// Error is a failure of the migrator: the variants of sqlx's MigrateError
// that running migrations can produce, with its messages.
//
//sumtype:decl
type Error interface {
	error
	migrateError()
}

// ExecuteError is a database failure outside a migration's own SQL: the
// lock, the ledger, a transaction boundary.
type ExecuteError struct{ Err error }

// ExecuteMigrationError is a migration whose SQL failed.
type ExecuteMigrationError struct {
	Err     error
	Version int64
}

// SourceError is a migration directory that could not be resolved.
type SourceError struct{ Err error }

// VersionMissingError is an applied migration this binary does not have.
type VersionMissingError struct{ Version int64 }

// VersionMismatchError is an applied migration whose file has changed since.
type VersionMismatchError struct{ Version int64 }

// DirtyError is a migration recorded as unsuccessful.
type DirtyError struct{ Version int64 }

func (*ExecuteError) migrateError()          {}
func (*ExecuteMigrationError) migrateError() {}
func (*SourceError) migrateError()           {}
func (*VersionMissingError) migrateError()   {}
func (*VersionMismatchError) migrateError()  {}
func (*DirtyError) migrateError()            {}

func (e *ExecuteError) Error() string { return fmt.Sprintf("while executing migrations: %v", e.Err) }

func (e *ExecuteMigrationError) Error() string {
	return fmt.Sprintf("while executing migration %d: %v", e.Version, e.Err)
}

func (e *SourceError) Error() string { return fmt.Sprintf("while resolving migrations: %v", e.Err) }

func (e *VersionMissingError) Error() string {
	return fmt.Sprintf("migration %d was previously applied but is missing in the resolved migrations", e.Version)
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("migration %d was previously applied but has been modified", e.Version)
}

func (e *DirtyError) Error() string {
	return fmt.Sprintf("migration %d is partially applied; fix and remove row from `_sqlx_migrations` table", e.Version)
}

// Unwrap is the database error.
func (e *ExecuteError) Unwrap() error { return e.Err }

// Unwrap is the database error.
func (e *ExecuteMigrationError) Unwrap() error { return e.Err }

// Unwrap is the resolution error.
func (e *SourceError) Unwrap() error { return e.Err }

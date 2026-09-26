// Package migrations holds Kuben's PostgreSQL migrations, embedded in the
// binary. The files are byte-for-byte copies of the Rust crate's
// crates/kuben-store/migrations/postgres (they are frozen until 2.1): sqlx
// stores a SHA-384 of every applied file in `_sqlx_migrations`, so one
// differing byte would make either binary refuse a database the other one
// migrated.
package migrations

import "embed"

// FS is the migration files, named `<VERSION>_<DESCRIPTION>.sql` as sqlx
// resolves them.
//
//go:embed *.sql
var FS embed.FS

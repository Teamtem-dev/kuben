// Package migrate runs SQL migrations exactly as sqlx 0.9 does, on sqlx's
// own ledger table `_sqlx_migrations`, so the Rust and the Go binary can
// take turns on one database in either direction. It replaces the sqlx
// migrator (sqlx-core/src/migrate/{migration,migrator,source,error}.rs and
// sqlx-postgres/src/migrate.rs) that crates/kuben-store used through
// `sqlx::migrate!`.
//
// What must match byte for byte, and where sqlx does it:
//   - the checksum: SHA-384 of the file's bytes, nothing stripped or
//     normalised (migration.rs `checksum`, source.rs `checksum_with` with no
//     ignored characters, which is the default without a sqlx.toml);
//   - version and description: the file name split at the first `_`, the
//     version parsed as i64, the description with the type's suffix removed
//     and `_` replaced by a space (source.rs `resolve_blocking_with_config`);
//   - the ledger DDL, the rows written, the advisory lock id and the order of
//     the steps (sqlx-postgres migrate.rs, migrator.rs `run_direct`).
package migrate

import (
	"crypto/sha512"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Type is what a migration file does, from its name.
type Type string

// The types, in sqlx's order (a Simple migration sorts before a reversible
// one of the same version).
const (
	// Simple is a `.sql` file with no down migration.
	Simple Type = "simple"
	// ReversibleUp is the `.up.sql` half of a reversible migration.
	ReversibleUp Type = "reversible_up"
	// ReversibleDown is the `.down.sql` half of a reversible migration.
	ReversibleDown Type = "reversible_down"
)

// typeOf is sqlx's MigrationType::from_filename.
func typeOf(name string) Type {
	switch {
	case strings.HasSuffix(name, ReversibleUp.suffix()):
		return ReversibleUp
	case strings.HasSuffix(name, ReversibleDown.suffix()):
		return ReversibleDown
	default:
		return Simple
	}
}

func (t Type) suffix() string {
	switch t {
	case Simple:
		return ".sql"
	case ReversibleUp:
		return ".up.sql"
	case ReversibleDown:
		return ".down.sql"
	}
	return ".sql"
}

func (t Type) rank() int {
	switch t {
	case Simple:
		return 0
	case ReversibleUp:
		return 1
	case ReversibleDown:
		return 2
	}
	return 3
}

// IsDown reports whether the migration reverts another one; Run skips it.
func (t Type) IsDown() bool { return t == ReversibleDown }

// Migration is one migration file, resolved as sqlx resolves it.
type Migration struct {
	Version     int64
	Description string
	Type        Type
	SQL         string
	// Checksum is the SHA-384 of SQL, as stored in `_sqlx_migrations`.
	Checksum []byte
	// NoTx is set by a file starting with `-- no-transaction`: it then runs
	// outside a transaction.
	NoTx bool
}

// Checksum is sqlx's migration checksum: the SHA-384 of the text, as is.
func Checksum(sql string) []byte {
	sum := sha512.Sum384([]byte(sql))
	return sum[:]
}

// Load resolves the migrations in the top directory of fsys as sqlx's
// resolve_blocking does: regular files named `<VERSION>_<DESCRIPTION>.sql`,
// sorted by version then type. Other files are silently ignored; a matching
// name whose version is not an integer is an error.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, &SourceError{Err: fmt.Errorf("error reading migration directory: %w", err)}
	}
	var out []Migration
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			// not a file; ignore
			continue
		}
		m, ok, err := resolve(fsys, entry.Name())
		if err != nil {
			return nil, &SourceError{Err: err}
		}
		if ok {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, compare)
	return out, nil
}

// resolve reads one file; false when its name is not a migration's.
func resolve(fsys fs.FS, name string) (Migration, bool, error) {
	versionText, rest, found := strings.Cut(name, "_")
	if !found || !strings.HasSuffix(rest, ".sql") {
		return Migration{}, false, nil
	}
	// Rust's i64::from_str: optional sign, decimal digits, nothing else.
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		return Migration{}, false, fmt.Errorf(
			"error parsing migration filename %s; expected integer version prefix (e.g. `01_foo.sql`)",
			rustQuote(name))
	}
	typ := typeOf(rest)
	// Rust's trim_end_matches removes the suffix as often as it repeats.
	description := rest
	for suffix := typ.suffix(); strings.HasSuffix(description, suffix); {
		description = strings.TrimSuffix(description, suffix)
	}
	description = strings.ReplaceAll(description, "_", " ")
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Migration{}, false, fmt.Errorf("error reading contents of migration %s: %w", name, err)
	}
	// fs::read_to_string refuses what is not UTF-8.
	if !utf8.Valid(data) {
		return Migration{}, false, fmt.Errorf("error reading contents of migration %s: stream did not contain valid UTF-8", name)
	}
	sql := string(data)
	return Migration{
		Version:     version,
		Description: description,
		Type:        typ,
		SQL:         sql,
		Checksum:    Checksum(sql),
		NoTx:        strings.HasPrefix(sql, "-- no-transaction"),
	}, true, nil
}

// compare is sqlx's Ord for Migration: version, then type.
func compare(a, b Migration) int {
	if a.Version != b.Version {
		if a.Version < b.Version {
			return -1
		}
		return 1
	}
	return a.Type.rank() - b.Type.rank()
}

// rustQuote is Rust's `{:?}` of a file name for the characters file names
// hold; see SUBSTITUTIONS.md ("Rust {:?} in messages").
func rustQuote(s string) string { return strconv.Quote(s) }

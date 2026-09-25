package migrations_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/store/migrations"
)

// rustDir is where the Rust crate keeps the originals, relative to this
// package's directory.
const rustDir = "../../../crates/kuben-store/migrations/postgres"

func embedded(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	return names
}

func TestEveryMigrationIsEmbedded(t *testing.T) {
	names := embedded(t)
	if len(names) != 33 {
		t.Fatalf("embedded %d migrations, want 33: %v", len(names), names)
	}
	if names[0] != "0001_init.sql" || names[len(names)-1] != "0034_usage_rollups.sql" {
		t.Fatalf("first/last migration: %s, %s", names[0], names[len(names)-1])
	}
}

// While the Rust crate exists, its migrations and these must be the same
// bytes: both binaries run on one database and sqlx compares checksums.
func TestCopiesAreIdenticalToRust(t *testing.T) {
	entries, err := os.ReadDir(rustDir)
	if os.IsNotExist(err) {
		t.Skip("the Rust crate is gone; the Go copies are the originals now")
	}
	if err != nil {
		t.Fatalf("read %s: %v", rustDir, err)
	}
	var rust []string
	for _, e := range entries {
		// sqlx reads regular files only; a directory there is not a migration.
		if e.Type().IsRegular() {
			rust = append(rust, e.Name())
		}
	}
	slices.Sort(rust)
	if got := embedded(t); !slices.Equal(got, rust) {
		t.Fatalf("migration files differ:\n go:   %v\n rust: %v", got, rust)
	}
	for _, name := range rust {
		want, err := os.ReadFile(filepath.Join(rustDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the Rust original", name)
		}
	}
}

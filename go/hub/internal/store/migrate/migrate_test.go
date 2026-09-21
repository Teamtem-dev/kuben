package migrate_test

import (
	"encoding/hex"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Teamtem-dev/kuben/go/hub/internal/store/migrate"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/migrations"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// SHA-384 of the migration files as `shasum -a 384` prints it: sqlx hashes
// the file's bytes, nothing stripped or normalised.
const (
	initChecksum   = "8f31b61b91f7e6dde541110d2cac976d5ecb019d1a4a6a74102b1067bac0ef73098861abe4429361c37412b1f765df37"
	rollupChecksum = "87b967d9a38751484d1cb4bfc469ed17ae06ffa615c23ff3b4f0ba5c9d6ab0b3a9692a2e838fa982356cde716c5c74d9"
)

func TestEmbeddedMigrationsResolveAsSqlxDoes(t *testing.T) {
	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var versions []int64
	for _, m := range ms {
		versions = append(versions, m.Version)
		if m.Type != migrate.Simple || m.NoTx {
			t.Errorf("migration %d: type %s, no-transaction %v", m.Version, m.Type, m.NoTx)
		}
	}
	want := []int64{
		1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
		21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34,
	}
	if diff := cmp.Diff(want, versions); diff != "" {
		t.Fatalf("versions (-want +got):\n%s", diff)
	}
	if len(ms) != len(want) {
		t.Fatalf("%d migrations", len(ms))
	}
	first, last := ms[0], ms[len(ms)-1]
	if first.Description != "init" || ms[1].Description != "team releases" || last.Description != "usage rollups" {
		t.Errorf("descriptions: %q, %q, %q", first.Description, ms[1].Description, last.Description)
	}
	if got := hex.EncodeToString(first.Checksum); got != initChecksum {
		t.Errorf("checksum of 0001: %s", got)
	}
	if got := hex.EncodeToString(last.Checksum); got != rollupChecksum {
		t.Errorf("checksum of 0034: %s", got)
	}
	if got := migrate.New(ms).Latest(); got != 34 {
		t.Errorf("latest: %d", got)
	}
}

// source.rs resolve_blocking_with_config, rule by rule.
func TestFileNamesAreReadAsSqlxReadsThem(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	fsys := fstest.MapFS{
		"README.md":              file("not a migration"),
		"0003.sql":               file("no underscore: ignored"),
		"0004_notes.txt":         file("not .sql: ignored"),
		"0005_dir.sql/inner.sql": file("a directory: ignored"),
		"2_add_users.sql":        file("create table users ();"),
		"1_first_step.sql":       file("-- no-transaction\ncreate index concurrently x on y (z);"),
		"3_double.sql.sql":       file("select 1;"),
		"4_pair.up.sql":          file("create table a ();"),
		"4_pair.down.sql":        file("drop table a;"),
		"+6_signed.sql":          file("select 6;"),
		"7_crlf_bom.sql":         file("\uFEFFselect 1;\r\n"),
	}
	ms, err := migrate.Load(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	type row struct {
		Version     int64
		Description string
		Type        migrate.Type
		NoTx        bool
	}
	var got []row
	for _, m := range ms {
		got = append(got, row{m.Version, m.Description, m.Type, m.NoTx})
		if !slicesEqual(m.Checksum, migrate.Checksum(m.SQL)) {
			t.Errorf("migration %d: checksum is not the SHA-384 of its text", m.Version)
		}
	}
	want := []row{
		{1, "first step", migrate.Simple, true},
		{2, "add users", migrate.Simple, false},
		{3, "double", migrate.Simple, false},
		{4, "pair", migrate.ReversibleUp, false},
		{4, "pair", migrate.ReversibleDown, false},
		{6, "signed", migrate.Simple, false},
		{7, "crlf bom", migrate.Simple, false},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("resolved (-want +got):\n%s", diff)
	}
	if len(ms) != len(want) {
		t.Fatalf("%d migrations", len(ms))
	}
	if sql := ms[6].SQL; sql != "\uFEFFselect 1;\r\n" {
		t.Errorf("the text is kept as read: %q", sql)
	}
}

func TestAVersionThatIsNotANumberIsAnError(t *testing.T) {
	_, err := migrate.Load(fstest.MapFS{"x1_bad.sql": &fstest.MapFile{Data: []byte("select 1;")}})
	var source *migrate.SourceError
	if !errors.As(err, &source) {
		t.Fatalf("want a source error, got %v", err)
	}
	want := "while resolving migrations: error parsing migration filename \"x1_bad.sql\"; expected integer version prefix (e.g. `01_foo.sql`)"
	if err.Error() != want {
		t.Fatalf("message:\n got %s\nwant %s", err, want)
	}
}

// Values of sqlx-postgres's generate_lock_id, computed with Python's
// zlib.crc32 (CRC-32/ISO-HDLC) times 0x3d32ad9e.
func TestLockIDIsSqlxs(t *testing.T) {
	for name, want := range map[string]int64{
		"postgres": 368907709894077238,
		"kuben":    2445118493912974152,
		"":         0,
	} {
		if got := migrate.LockID(name); got != want {
			t.Errorf("LockID(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestErrorMessagesAreSqlxs(t *testing.T) {
	for _, tc := range []struct {
		err  migrate.Error
		want string
	}{
		{&migrate.VersionMissingError{Version: 35}, "migration 35 was previously applied but is missing in the resolved migrations"},
		{&migrate.VersionMismatchError{Version: 4}, "migration 4 was previously applied but has been modified"},
		{&migrate.DirtyError{Version: 4}, "migration 4 is partially applied; fix and remove row from `_sqlx_migrations` table"},
		{&migrate.ExecuteMigrationError{Version: 4, Err: errors.New("boom")}, "while executing migration 4: boom"},
		{&migrate.ExecuteError{Err: errors.New("boom")}, "while executing migrations: boom"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func slicesEqual(a, b []byte) bool { return hex.EncodeToString(a) == hex.EncodeToString(b) }

func pool(t *testing.T, pc *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.NewWithConfig(t.Context(), pc)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func embedded(t *testing.T) *migrate.Migrator {
	t.Helper()
	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return migrate.New(ms)
}

func run(t *testing.T, p *pgxpool.Pool, m *migrate.Migrator) error {
	t.Helper()
	c, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	err = m.Run(t.Context(), c.Conn())
	if err != nil {
		// As the store does: a failed run may still hold the session lock.
		if closeErr := c.Conn().Close(t.Context()); closeErr != nil {
			t.Fatalf("close: %v", closeErr)
		}
	}
	return err
}

// The ledger is sqlx's: its rows are what sqlx writes, so the Rust binary
// accepts a database the Go binary migrated (and the other way round).
func TestRunWritesSqlxsLedger(t *testing.T) {
	p := pool(t, pgtest.Schema(t))
	m := embedded(t)
	if err := run(t, p, m); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := p.Query(t.Context(),
		"SELECT version, description, success, checksum, execution_time FROM _sqlx_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	defer rows.Close()
	want := m.Migrations()
	i := 0
	for rows.Next() {
		var version, elapsed int64
		var description string
		var success bool
		var checksum []byte
		if err := rows.Scan(&version, &description, &success, &checksum, &elapsed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(want) {
			t.Fatalf("extra ledger row %d", version)
		}
		w := want[i]
		if version != w.Version || description != w.Description || !success ||
			!slicesEqual(checksum, w.Checksum) || elapsed < 0 {
			t.Errorf("ledger row %d: %q %v %x %d", version, description, success, checksum, elapsed)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if i != len(want) {
		t.Fatalf("%d ledger rows, want %d", i, len(want))
	}
	if err := run(t, p, m); err != nil {
		t.Fatalf("a second run is a no-op: %v", err)
	}
	// The run released its lock: another session can take it at once.
	c, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	var got bool
	if err := c.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)",
		migrate.LockID(currentDatabase(t, p))).Scan(&got); err != nil || !got {
		t.Fatalf("the migration lock is still held: %v %v", got, err)
	}
	if _, err := c.Exec(t.Context(), "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

func currentDatabase(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := p.QueryRow(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("current database: %v", err)
	}
	return name
}

func TestRunRefusesAChangedAMissingAndADirtyMigration(t *testing.T) {
	p := pool(t, pgtest.Schema(t))
	m := embedded(t)
	if err := run(t, p, m); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := p.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	exec(`UPDATE _sqlx_migrations SET checksum = '\x00' WHERE version = 4`)
	var mismatch *migrate.VersionMismatchError
	if err := run(t, p, m); !errors.As(err, &mismatch) || mismatch.Version != 4 {
		t.Fatalf("changed migration: %v", err)
	}
	all := m.Migrations()
	if len(all) < 4 || all[3].Version != 4 {
		t.Fatalf("migration 4 is not the fourth: %d migrations", len(all))
	}
	exec(`UPDATE _sqlx_migrations SET checksum = $1 WHERE version = 4`, all[3].Checksum)
	if err := run(t, p, m); err != nil {
		t.Fatalf("the restored checksum is accepted: %v", err)
	}

	exec(`INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time) VALUES (99, 'future', TRUE, '\x00', 0)`)
	var missing *migrate.VersionMissingError
	if err := run(t, p, m); !errors.As(err, &missing) || missing.Version != 99 {
		t.Fatalf("missing migration: %v", err)
	}
	exec(`UPDATE _sqlx_migrations SET success = FALSE WHERE version = 99`)
	var dirty *migrate.DirtyError
	if err := run(t, p, m); !errors.As(err, &dirty) || dirty.Version != 99 {
		t.Fatalf("dirty migration: %v", err)
	}
}

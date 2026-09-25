package backup_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/backup"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

func TestPasswordsLeaveTheURL(t *testing.T) {
	cases := []struct {
		url, bare string
		password  opt.Val[string]
	}{
		{"postgres://kuben:s%40cret@db:5432/kuben?sslmode=verify-full",
			"postgres://kuben@db:5432/kuben?sslmode=verify-full", opt.Some("s@cret")},
		{"postgres://kuben@db/kuben", "postgres://kuben@db/kuben", opt.None[string]()},
		{"postgresql:///kuben?host=/run/postgresql", "postgresql:///kuben?host=/run/postgresql", opt.None[string]()},
	}
	for _, c := range cases {
		bare, password := backup.SplitPassword(c.url)
		if bare != c.bare || password != c.password {
			t.Errorf("SplitPassword(%q) = %q, %v; want %q, %v", c.url, bare, password, c.bare, c.password)
		}
	}
	if got := backup.PercentDecode("a%2Fb%zz"); got != "a/b%zz" {
		t.Errorf("PercentDecode = %q", got)
	}
}

func TestStaleBackupsAreReported(t *testing.T) {
	const hour = 3_600_000
	if _, err := backup.Freshness(opt.None[int64](), 0, 26); err == nil {
		t.Error("no backup, watched: fresh")
	}
	if _, err := backup.Freshness(opt.None[int64](), 0, 0); err != nil {
		t.Errorf("no backup, not watched: %v", err)
	}
	msg, err := backup.Freshness(opt.Some[int64](0), 2*hour, 26)
	if err != nil || msg != "the newest good backup is 2 h old" {
		t.Errorf("2 h old: %q, %v", msg, err)
	}
	if _, err := backup.Freshness(opt.Some[int64](0), 27*hour, 26); err == nil {
		t.Error("27 h old against 26: fresh")
	} else if want := "the newest good backup is 27 h old, over backup.max_age_hours = 26"; err.Error() != want {
		t.Errorf("stale text %q", err)
	}
	if _, err := backup.Freshness(opt.Some[int64](0), 27*hour, 0); err != nil {
		t.Errorf("not watched: %v", err)
	}
}

func TestToolVersionsAreRead(t *testing.T) {
	cases := []struct {
		text  string
		major int32
		ok    bool
	}{
		{"pg_dump (PostgreSQL) 17.4", 17, true},
		{"pg_restore (PostgreSQL) 16.9 (Debian 16.9-1)", 16, true},
		{"nothing here", 0, false},
	}
	for _, c := range cases {
		major, ok := backup.ParseMajor(c.text)
		if major != c.major || ok != c.ok {
			t.Errorf("ParseMajor(%q) = %d, %v", c.text, major, ok)
		}
	}
}

// fakeBackup writes a backup named stamp under root with dump as its
// archive.
func fakeBackup(t *testing.T, root, stamp string, dump []byte) string {
	t.Helper()
	dir := filepath.Join(root, backup.Prefix+stamp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.DumpFile), dump, 0o600); err != nil {
		t.Fatal(err)
	}
	size, sum, err := backup.SHA256File(filepath.Join(dir, backup.DumpFile))
	if err != nil {
		t.Fatal(err)
	}
	text, err := backup.EncodeManifest(backup.Manifest{
		Format: backup.Format, Kuben: "2.0.0", CreatedAt: 1, Schema: 25, DatabaseVersion: 170_004,
		Dump: backup.FileSum{File: backup.DumpFile, Bytes: size, SHA256: sum},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), text, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDamagedBackupsAreRefused(t *testing.T) {
	root := t.TempDir()
	dir := fakeBackup(t, root, "20260917T000000Z", []byte("PGDMP archive"))
	m, err := backup.CheckedManifest(dir)
	if err != nil || m.Schema != 25 {
		t.Fatalf("intact: %+v, %v", m, err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.DumpFile), []byte("PGDMP changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.CheckedManifest(dir); err == nil {
		t.Error("a changed dump was accepted")
	}
	if _, err := backup.CheckedManifest(root); err == nil {
		t.Error("not a backup, accepted")
	}
}

func TestOnlyTheNewestCompleteBackupsAreKept(t *testing.T) {
	root := t.TempDir()
	for _, stamp := range []string{"20260915T000000Z", "20260916T000000Z", "20260917T000000Z"} {
		fakeBackup(t, root, stamp, []byte("x"))
	}
	for _, name := range []string{"kuben-incomplete", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := backup.Prune(root, 2)
	if err != nil || removed != 1 {
		t.Fatalf("prune: %d, %v", removed, err)
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	if exists("kuben-20260915T000000Z") || !exists("kuben-20260917T000000Z") {
		t.Error("the wrong backup was removed")
	}
	if !exists("kuben-incomplete") || !exists("unrelated") {
		t.Error("an incomplete or unrelated directory was removed")
	}
	if removed, err := backup.Prune(root, 0); err != nil || removed != 0 {
		t.Errorf("0 keeps everything: %d, %v", removed, err)
	}
}

// The manifest is written as serde_json's pretty printer wrote it.
func TestTheManifestIsWrittenAsBefore(t *testing.T) {
	text, err := backup.EncodeManifest(backup.Manifest{
		Format: 1, Kuben: "2.0.0", CreatedAt: 5, Schema: 25, DatabaseVersion: 170_004,
		Dump:            backup.FileSum{File: backup.DumpFile, Bytes: 3, SHA256: "ab"},
		KeyFingerprints: []string{"1:ff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "format": 1,
  "kuben": "2.0.0",
  "created_at": 5,
  "schema": 25,
  "database_version": 170004,
  "dump": {
    "file": "database.dump",
    "bytes": 3,
    "sha256": "ab"
  },
  "keyring_included": false,
  "key_fingerprints": [
    "1:ff"
  ]
}`
	if diff := cmp.Diff(want, string(text)); diff != "" {
		t.Errorf("manifest (-want +got):\n%s", diff)
	}
	var m backup.Manifest
	if err := m.UnmarshalJSON([]byte(`{"format":1}`)); err == nil {
		t.Error("a manifest without its members was read")
	}
	if err := m.UnmarshalJSON(text); err != nil || m.Dump.Bytes != 3 || len(m.KeyFingerprints) != 1 {
		t.Errorf("round trip: %+v, %v", m, err)
	}
}

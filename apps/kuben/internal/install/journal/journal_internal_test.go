package journal

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

func TestTheFirstSightingDecidesTheOwnerForGood(t *testing.T) {
	j := fresh()
	if got := j.Claim(KindPostgresDatabase, "kuben", false, 1); got != OwnerPreexisting {
		t.Fatalf("first claim %s", got)
	}
	if got := j.Claim(KindPostgresDatabase, "kuben", true, 2); got != OwnerPreexisting {
		t.Fatalf("a later run never takes a database over: %s", got)
	}
	if j.Owns(KindPostgresDatabase, "kuben") {
		t.Error("owns a preexisting database")
	}
	if got := j.Claim(KindCluster, "k3s", true, 3); got != OwnerKuben {
		t.Fatalf("k3s %s", got)
	}
	if !j.Owns(KindCluster, "k3s") {
		t.Error("does not own k3s")
	}
	if _, ok := j.OwnerOf(KindFile, "/etc/kuben/config.toml"); ok {
		t.Error("an unseen file has an owner")
	}
	j.Release(KindCluster, "k3s")
	if _, ok := j.OwnerOf(KindCluster, "k3s"); ok {
		t.Error("released k3s still has an owner")
	}
}

func TestRunsRecordStepsAndStayBounded(t *testing.T) {
	j := fresh()
	j.begin("2.0.0", 1)
	j.record("binary", StepChanged, "v2.0.0 → /usr/local/bin/kuben", 2)
	if _, ok := j.Unfinished(); !ok {
		t.Error("a run in progress is unfinished")
	}
	j.finish(false, 3)
	if run, ok := j.Unfinished(); !ok || len(run.Steps) != 1 {
		t.Errorf("unfinished = %v, %v", run, ok)
	}
	j.begin("2.0.0", 4)
	j.record("binary", StepUnchanged, "up to date", 5)
	j.finish(true, 6)
	if _, ok := j.Unfinished(); ok {
		t.Error("a successful run is unfinished")
	}
	if j.LastRunChanged() {
		t.Error("the last run changed nothing")
	}
	for range 30 {
		j.begin("2.0.0", 7)
	}
	if len(j.Runs) != maxRuns {
		t.Errorf("%d runs kept", len(j.Runs))
	}
}

func TestTheBookSavesAtomicallyAndMovesAnUnreadableFileAside(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	c := clock.Fixed(1_700_000_000_000)
	book, err := OpenBook(dir, opt.None[uint32](), c, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Begin("2.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Claim(KindSystemUser, "kuben", true); err != nil {
		t.Fatal(err)
	}
	book.Start("user")
	if err := book.Done(true, "kuben (uid 999)"); err != nil {
		t.Fatal(err)
	}
	book.Start("database")
	if err := book.Finish(errors.New("PostgreSQL did not answer")); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		ID     string
		Result StepResult
	}
	got := []outcome{}
	for _, s := range book.Journal().Runs[0].Steps {
		got = append(got, outcome{s.ID, s.Result})
	}
	if diff := cmp.Diff([]outcome{{"user", StepChanged}, {"database", StepFailed}}, got); diff != "" {
		t.Errorf("steps (-want +got):\n%s", diff)
	}
	if ok, known := book.Journal().Runs[0].Succeeded.Get(); !known || ok {
		t.Error("the run did not fail")
	}
	again, err := OpenBook(dir, opt.None[uint32](), c, logger)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(book.Journal(), again.Journal(), cmp.AllowUnexported(opt.Val[int64]{}, opt.Val[bool]{})); diff != "" {
		t.Errorf("reopened (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(dir, "install/journal.json.new")); err == nil {
		t.Error("the staged file is left behind")
	}

	if err := os.WriteFile(filepath.Join(dir, File), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenBook(dir, opt.None[uint32](), c, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Journal().Runs) != 0 {
		t.Error("an unreadable journal was read")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "install"))
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range entries {
		kept = kept || strings.Contains(e.Name(), "unreadable")
	}
	if !kept {
		t.Error("the unreadable journal is not kept aside")
	}
}

func TestTheFormatReadsBack(t *testing.T) {
	text := `{"format":1,"runs":[{"kuben":"2.0.0","startedAt":1,"steps":[{"id":"k3s","result":"changed","detail":"v1.33","at":2}]}],
            "resources":[{"kind":"cluster","name":"k3s","owner":"kuben","since":2}]}`
	var j Journal
	if err := json.Unmarshal([]byte(text), &j); err != nil {
		t.Fatal(err)
	}
	if !j.Owns(KindCluster, "k3s") {
		t.Error("k3s is not Kuben's")
	}
	if j.Runs[0].Steps[0].Result != StepChanged {
		t.Errorf("result %s", j.Runs[0].Steps[0].Result)
	}
	if _, ok := j.Unfinished(); !ok {
		t.Error("an interrupted run has no outcome")
	}
}

// The file is serde_json's pretty form: camelCase members, absent options
// left out, empty lists as [], `<` unescaped, a final newline.
func TestTheFileIsWrittenAsSerdeWroteIt(t *testing.T) {
	dir := t.TempDir()
	book, err := OpenBook(dir, opt.None[uint32](), clock.Fixed(5), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Begin("2.0.0"); err != nil {
		t.Fatal(err)
	}
	book.Start("platform")
	if err := book.Done(false, "a<b"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "format": 1,
  "runs": [
    {
      "kuben": "2.0.0",
      "startedAt": 5,
      "steps": [
        {
          "id": "platform",
          "result": "unchanged",
          "detail": "a<b",
          "at": 5
        }
      ]
    }
  ],
  "resources": []
}
`
	if diff := cmp.Diff(want, string(data)); diff != "" {
		t.Errorf("file (-want +got):\n%s", diff)
	}
	peeked, ok := Peek(filepath.Join(dir, File))
	if !ok || len(peeked.Runs) != 1 {
		t.Errorf("peek = %v, %v", peeked, ok)
	}
	if _, ok := Peek(filepath.Join(dir, "missing.json")); ok {
		t.Error("a missing file peeked")
	}
}

func TestTheDebugNamesAreRusts(t *testing.T) {
	if got := KindPostgresDatabase.DebugName() + " " + OwnerKuben.DebugName(); got != "PostgresDatabase Kuben" {
		t.Errorf("got %s", got)
	}
}

package store_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

func TestTheLatestJournalOfAHostIsKeptOnce(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	decode := func(text string) any {
		t.Helper()
		v, err := wire.DecodeAny([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := decode(`{"format": 1, "runs": []}`)
	if ok, err := s.RecordInstallJournal(ctx, "vps-1", first); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	if ok, err := s.RecordInstallJournal(ctx, "vps-1", first); err != nil || ok {
		t.Fatalf("an unchanged journal is not written again: %v %v", ok, err)
	}
	second := decode(`{"format": 1, "runs": [{"kuben": "2.0.0"}]}`)
	if ok, err := s.RecordInstallJournal(ctx, "vps-1", second); err != nil || !ok {
		t.Fatalf("newer: %v %v", ok, err)
	}
	journal, _, ok, err := s.InstallJournal(ctx, "vps-1")
	if err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	if diff := cmp.Diff(second, journal); diff != "" {
		t.Fatal(diff)
	}
	if _, _, ok, err := s.InstallJournal(ctx, "vps-2"); err != nil || ok {
		t.Fatalf("another host: %v %v", ok, err)
	}
}

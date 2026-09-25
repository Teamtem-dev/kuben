package upgrade_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/upgrade"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

func TestFreeSpaceIsReadFromDF(t *testing.T) {
	out := "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/sda1 102400 2048 100352 2% /\n"
	if got := upgrade.ParseDF(out); got != opt.Some[uint64](100_352<<10) {
		t.Errorf("parse: %v", got)
	}
	if got := upgrade.ParseDF(""); got.IsSome() {
		t.Errorf("empty: %v", got)
	}
	if got := upgrade.ParseDF("header only"); got.IsSome() {
		t.Errorf("header only: %v", got)
	}
	if got := upgrade.FreeBytes(t.Context(), "/definitely/not/there/at/all"); !got.IsSome() {
		t.Error("the nearest existing ancestor is measured")
	}
}

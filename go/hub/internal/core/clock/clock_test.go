package clock_test

import (
	"math"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
)

func TestPlusHoursSaturates(t *testing.T) {
	if got := clock.PlusHours(1_000, 2); got != 1_000+7_200_000 {
		t.Fatalf("got %d", got)
	}
	if got := clock.PlusHours(math.MaxInt64-5, 1); got != math.MaxInt64 {
		t.Fatalf("got %d", got)
	}
	if got := clock.PlusHours(0, math.MaxUint64); got != math.MaxInt64 {
		t.Fatalf("got %d", got)
	}
	if got := clock.SaturatingAdd(math.MinInt64+1, -5); got != math.MinInt64 {
		t.Fatalf("got %d", got)
	}
}

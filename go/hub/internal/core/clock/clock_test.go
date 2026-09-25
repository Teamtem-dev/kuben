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

func TestSaturatingSub(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{10, 3, 7},
		{math.MinInt64 + 1, 5, math.MinInt64},
		{math.MaxInt64 - 1, -5, math.MaxInt64},
		{0, math.MinInt64, math.MaxInt64},
		{-1, math.MaxInt64, math.MinInt64},
	}
	for _, c := range cases {
		if got := clock.SaturatingSub(c.a, c.b); got != c.want {
			t.Errorf("SaturatingSub(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

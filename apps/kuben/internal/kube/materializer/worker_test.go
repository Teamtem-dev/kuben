package materializer_test

import (
	"math"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/materializer"
)

func TestRunsWaitForApprovalUntilTheirWindowCloses(t *testing.T) {
	none := opt.None[time.Duration]()
	cases := []struct {
		expires opt.Val[int64]
		now     int64
		want    opt.Val[time.Duration]
		why     string
	}{
		{opt.None[int64](), 0, none, "no window: nothing to wait for"},
		{opt.Some[int64](1_000), 1_000, none, "closed"},
		{opt.Some[int64](1_000), 2_000, none, "closed long ago"},
		{opt.Some[int64](1_001), 1_000, opt.Some(3 * time.Second), "at least one poll"},
		{opt.Some[int64](60_000), 0, opt.Some(time.Minute), "a minute"},
		{opt.Some[int64](math.MaxInt64), 0, opt.Some(time.Hour), "at most an hour"},
		{opt.Some[int64](math.MaxInt64), math.MinInt64, none, "no overflow"},
	}
	for _, c := range cases {
		if got := materializer.ApprovalWait(c.expires, c.now); got != c.want {
			t.Errorf("%s: %v", c.why, got)
		}
	}
}

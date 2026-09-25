package link_test

import (
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/agent/link"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/link.rs; the link end to end against
// the hub is go/hub/internal/platform/agentlink/link_test.go (tests/link.rs).

func TestBackoffDoublesUpToTheCapAndJitterTakesOffAtMostHalf(t *testing.T) {
	minimum, maximum := time.Second, 5*time.Minute
	cases := []struct {
		failures uint32
		jitter   float64
		want     time.Duration
	}{
		{0, 0, time.Second},
		{3, 0, 8 * time.Second},
		{30, 0, maximum},
		{3, 1, 4 * time.Second},
		{3, 7, 4 * time.Second}, // jitter is clamped
		{1 << 31, 0, maximum},
	}
	for _, c := range cases {
		if got := link.Backoff(c.failures, minimum, maximum, c.jitter); got != c.want {
			t.Errorf("backoff(%d, jitter %v) = %v, want %v", c.failures, c.jitter, got, c.want)
		}
	}
}

func TestRenewalIsDueTwoThirdsIntoTheLifeAtTheLatest(t *testing.T) {
	notBefore := time.Unix(1_789_000_000, 0)
	life := link.Lifetime{NotBefore: notBefore, NotAfter: notBefore.Add(24 * time.Hour)}
	close := func(a, b time.Time) bool { return a.Sub(b).Abs() < time.Second }
	if !close(link.RenewAt(life, 0), notBefore.Add(16*time.Hour)) {
		t.Fatal(link.RenewAt(life, 0))
	}
	if !close(link.RenewAt(life, 1), notBefore.Add((16*60-144)*time.Minute)) {
		t.Fatal(link.RenewAt(life, 1))
	}
	if !close(link.RenewAt(life, 9), link.RenewAt(life, 1)) {
		t.Fatal("jitter is clamped")
	}
}

func TestOnlyRefusalsTheHubMustLiftAreLasting(t *testing.T) {
	refused := func(reason protocol.Refusal) error { return link.RefusedError{Reason: reason} }
	if !link.IsLasting(refused(protocol.RefusalRevoked)) || !link.IsLasting(refused(protocol.RefusalUnsupportedProtocol)) ||
		!link.IsLasting(refused(protocol.RefusalUnknownCluster)) {
		t.Fatal("lasting refusals")
	}
	if link.IsLasting(refused(protocol.RefusalBadRequest)) || link.IsLasting(link.ErrClosed) {
		t.Fatal("passing trouble")
	}
}

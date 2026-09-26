package discovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

type steps struct{ now int64 }

func (s *steps) NowMs() int64 { return s.now }

func TestRunStopsWithItsContextAndPublishesNothingHalfDiscovered(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var w discovery.Watch
	// No store: a round that saw its context end touches nothing.
	err := discovery.Run(ctx, discovery.Deps{Logger: quiet(), Cluster: k3sCluster(t).cluster(), Watch: &w, Clock: &steps{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := w.Get(); ok {
		t.Fatal("published facts of cancelled probes")
	}
}

func orgWithPrimary(t *testing.T, s *store.Store, slug string, primary bool) ids.OrgID {
	t.Helper()
	ctx := t.Context()
	o, err := s.CreateOrg(ctx, slug, slug)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if primary {
		tn, err := s.Tenant(ctx, o.ID)
		if err != nil {
			t.Fatalf("tenant: %v", err)
		}
		if _, err := tn.CreateCluster(ctx, "primary"); err != nil {
			t.Fatalf("cluster: %v", err)
		}
		if err := tn.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	return o.ID
}

// recorded is the facts recorded for org's primary cluster.
func recorded(t *testing.T, s *store.Store, org ids.OrgID) (store.CapabilityRecord, bool) {
	t.Helper()
	tn, err := s.Tenant(t.Context(), org)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	defer func() {
		if err := tn.Rollback(context.Background()); err != nil {
			t.Errorf("rollback: %v", err)
		}
	}()
	r, ok, err := tn.ClusterCapabilities(t.Context(), "primary")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return r, ok
}

func TestFactsAreRecordedForEveryPrimaryCluster(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := orgWithPrimary(t, s, "disc-a", true)
	b := orgWithPrimary(t, s, "disc-b", false)
	clk := &steps{now: 1_000}
	var w discovery.Watch
	l := discovery.NewLoop(discovery.Deps{Logger: quiet(), Cluster: k3sCluster(t).cluster(), Store: s, Watch: &w, Clock: clk})

	pause, ok := l.Round(ctx)
	if !ok || pause != discovery.Interval {
		t.Fatalf("round: %v, %v", pause, ok)
	}
	published, ok := w.Get()
	if !ok || !published.MetricsAPI {
		t.Fatalf("published: %+v, %v", published, ok)
	}
	text, err := jsonx.CanonicalValue(published)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonx.DecodeAny([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := recorded(t, s, a)
	if !ok || got.ObservedAt != 1_000 {
		t.Fatalf("A: %+v, %v", got, ok)
	}
	if diff := cmp.Diff(want, got.Facts); diff != "" {
		t.Fatalf("stored facts (-want +got):\n%s", diff)
	}
	if _, ok := recorded(t, s, b); ok {
		t.Fatal("B has no primary cluster")
	}

	// Unchanged facts, same organizations: not written again.
	clk.now += time.Minute.Milliseconds()
	l.Round(ctx)
	if got, _ := recorded(t, s, a); got.ObservedAt != 1_000 {
		t.Fatalf("rewritten unchanged: %d", got.ObservedAt)
	}
	// A new organization gets the facts at once.
	c := orgWithPrimary(t, s, "disc-c", true)
	clk.now += time.Minute.Milliseconds()
	l.Round(ctx)
	if got, ok := recorded(t, s, c); !ok || got.ObservedAt != clk.now {
		t.Fatalf("C: %+v, %v", got, ok)
	}
	// Unchanged facts are still rewritten every ten minutes.
	clk.now += discovery.Rewrite.Milliseconds()
	l.Round(ctx)
	if got, _ := recorded(t, s, a); got.ObservedAt != clk.now {
		t.Fatalf("not rewritten: %d", got.ObservedAt)
	}
}

func TestOfflineClusterRetriesSooner(t *testing.T) {
	s := pgtest.Store(t)
	f := k3sCluster(t)
	forbid(&f.typed.Fake, "get", "group")
	var w discovery.Watch
	l := discovery.NewLoop(discovery.Deps{Logger: quiet(), Cluster: f.cluster(), Store: s, Watch: &w, Clock: &steps{}})
	if pause, ok := l.Round(t.Context()); !ok || pause != discovery.FirstRetry {
		t.Fatalf("pause: %v, %v", pause, ok)
	}
}

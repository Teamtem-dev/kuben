package discovery_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestWatchPublishesOnlyChanges(t *testing.T) {
	var w discovery.Watch
	if _, ok := w.Get(); ok {
		t.Fatal("no facts before the first discovery")
	}
	changed := w.Changed()
	if closed(changed) {
		t.Fatal("nothing changed yet")
	}
	first := discovery.ClusterFacts{MetricsAPI: true}
	if !w.Publish(first) || !closed(changed) {
		t.Fatal("the first facts are a change")
	}
	changed = w.Changed()
	if w.Publish(discovery.ClusterFacts{MetricsAPI: true, GatewayClasses: []discovery.Readiness{}}) || closed(changed) {
		t.Fatal("equal facts are no change")
	}
	got, ok := w.Get()
	if !ok || !got.Equal(first) {
		t.Fatalf("get: %+v, %v", got, ok)
	}
	if !w.Publish(discovery.ClusterFacts{CertManager: true}) || !closed(changed) {
		t.Fatal("different facts are a change")
	}
}

func TestWatchHandsOutCopies(t *testing.T) {
	var w discovery.Watch
	f := discovery.ClusterFacts{
		GatewayAPI:     opt.Some(discovery.GatewayAPI{Kinds: []string{"HTTPRoute"}}),
		ClusterIssuers: []discovery.Readiness{{Name: "le", Ready: true}},
	}
	w.Publish(f)
	f.ClusterIssuers[0].Ready = false
	got, _ := w.Get()
	got.ClusterIssuers[0].Name = "changed"
	g, _ := got.GatewayAPI.Get()
	g.Kinds[0] = "changed"
	again, _ := w.Get()
	if again.ClusterIssuers[0] != (discovery.Readiness{Name: "le", Ready: true}) || !again.Serves("HTTPRoute") {
		t.Fatalf("a reader changed the published facts: %+v", again)
	}
}

// Readers that take Changed before Get see the last facts, however the
// publications interleave with them (run with -race).
func TestWatchReadersNeverMissTheLastChange(_ *testing.T) {
	var w discovery.Watch
	const last = 200
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				changed := w.Changed()
				if f, ok := w.Get(); ok && f.KubernetesVersion == opt.Some(fmt.Sprint(last)) {
					return
				}
				<-changed
			}
		})
	}
	for i := range last + 1 {
		w.Publish(discovery.ClusterFacts{KubernetesVersion: opt.Some(fmt.Sprint(i))})
	}
	readers.Wait()
}

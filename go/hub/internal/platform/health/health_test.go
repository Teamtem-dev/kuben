package health_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
)

type steps struct{ now int64 }

func (s *steps) NowMs() int64 { return s.now }

func TestReadinessAndSubsystems(t *testing.T) {
	c := &steps{now: 1_000}
	h := health.New(c)
	if h.IsReady() || !h.IsLive(10_000) {
		t.Fatal("starts live and not ready")
	}
	h.Starting("informers")
	h.Degrade("controllers", "boom")
	if !h.AnyDegraded() {
		t.Fatal("degraded")
	}
	h.OK("controllers")
	if h.AnyDegraded() {
		t.Fatal("recovered")
	}
	h.SetReady(true)
	if !h.IsReady() {
		t.Fatal("ready")
	}
	d := h.Details()
	if len(d) != 2 || d[0].Name != "controllers" || d[1].Name != "informers" {
		t.Fatalf("got %+v", d)
	}
	c.now += 10_000
	if h.IsLive(10_000) {
		t.Fatal("a stale heartbeat is not live")
	}
	h.Heartbeat()
	if !h.IsLive(10_000) {
		t.Fatal("beat again")
	}
	h.Degrade("db", "down")
	data, err := json.Marshal(h.Details()[1].Subsystem)
	if err != nil || string(data) != `{"state":"degraded","last_error":"down","updated_at_ms":11000}` {
		t.Fatalf("got %s %v", data, err)
	}
	data, _ = json.Marshal(h.Details()[0].Subsystem)
	if string(data) != `{"state":"ok","updated_at_ms":1000}` {
		t.Fatalf("got %s", data)
	}
	_ = clock.System{}
}

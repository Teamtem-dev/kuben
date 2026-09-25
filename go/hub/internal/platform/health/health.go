// Package health is the subsystem health registry behind /livez, /readyz
// and /api/v1/healthz/details (crates/kuben-platform/src/health.rs).
package health

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/metrics"
)

// State is how a subsystem is doing.
type State string

// The states.
const (
	Ok       State = "ok"
	Degraded State = "degraded"
	Starting State = "starting"
	// Standby is healthy but deliberately idle, e.g. waiting for the
	// controller lease.
	Standby State = "standby"
)

// Subsystem is the reported health of one part of the server.
type Subsystem struct {
	State       State           `json:"state"`
	LastError   opt.Val[string] `json:"last_error,omitzero"`
	UpdatedAtMs int64           `json:"updated_at_ms"`
}

// Health is shared by every part of the server; the zero Health is not
// usable, make one with New.
type Health struct {
	clock     clock.Clock
	ready     atomic.Bool
	heartbeat atomic.Int64 // last watchdog beat (unix ms); /livez fails when stale

	mu         sync.Mutex // guards subsystems
	subsystems map[string]Subsystem

	metrics *metrics.Metrics // the process's metrics, beside its health
}

// New is a registry that is not ready and has just beaten.
func New(c clock.Clock) *Health {
	h := &Health{clock: c, subsystems: map[string]Subsystem{}, metrics: metrics.New()}
	h.heartbeat.Store(c.NowMs())
	return h
}

// Metrics are the process's Prometheus metrics; every part of the server
// that reports health reports its counters here too. A nil Health has none
// (nil metrics record nothing).
func (h *Health) Metrics() *metrics.Metrics {
	if h == nil {
		return nil
	}
	return h.metrics
}

// SetReady sets readiness.
func (h *Health) SetReady(ready bool) { h.ready.Store(ready) }

// IsReady reports readiness.
func (h *Health) IsReady() bool { return h.ready.Load() }

// Heartbeat is called by the watchdog every second.
func (h *Health) Heartbeat() { h.heartbeat.Store(h.clock.NowMs()) }

// IsLive reports whether the watchdog beat within maxAgeMs.
func (h *Health) IsLive(maxAgeMs int64) bool {
	return h.clock.NowMs()-h.heartbeat.Load() < maxAgeMs
}

// Starting marks name as starting.
func (h *Health) Starting(name string) { h.set(name, Starting, opt.None[string]()) }

// OK marks name as healthy.
func (h *Health) OK(name string) { h.set(name, Ok, opt.None[string]()) }

// Standby marks name as deliberately idle.
func (h *Health) Standby(name string) { h.set(name, Standby, opt.None[string]()) }

// Degrade marks name as degraded by err.
func (h *Health) Degrade(name, err string) { h.set(name, Degraded, opt.Some(err)) }

func (h *Health) set(name string, s State, lastError opt.Val[string]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subsystems[name] = Subsystem{State: s, LastError: lastError, UpdatedAtMs: h.clock.NowMs()}
}

// Named is a subsystem with its name.
type Named struct {
	Name string
	Subsystem
}

// Details is every subsystem, sorted by name.
func (h *Health) Details() []Named {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := slices.Sorted(maps.Keys(h.subsystems))
	out := make([]Named, 0, len(names))
	for _, n := range names {
		out = append(out, Named{Name: n, Subsystem: h.subsystems[n]})
	}
	return out
}

// AnyDegraded reports whether a subsystem is degraded.
func (h *Health) AnyDegraded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subsystems {
		if s.State == Degraded {
			return true
		}
	}
	return false
}

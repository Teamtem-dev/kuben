// Package metrics is Kuben's Prometheus metrics (crates/kuben/src/telemetry.rs
// install_metrics and the `metrics::counter!`/`gauge!` calls of the Rust
// crates). The metric names and labels are a contract (plan §6): dashboards
// and alerts read them.
//
// Each [Metrics] owns its registry (no process-wide default registry), and
// [Serve] exposes it on `server.metrics_bind`.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the counters and gauges Kuben emits. A nil *Metrics records
// nothing, so parts built without one (tests) need no special case.
type Metrics struct {
	registry          *prometheus.Registry
	subsystemFailures *prometheus.CounterVec
	subsystemPanics   *prometheus.CounterVec
	reconcileErrors   *prometheus.CounterVec
	leader            prometheus.Gauge
	sseLagged         prometheus.Counter
	auditWriteErrors  prometheus.Counter
}

// New registers every metric on a registry of its own.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		subsystemFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuben_subsystem_failures_total", Help: "Supervised subsystems that returned an error and were restarted.",
		}, []string{"subsystem"}),
		subsystemPanics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuben_subsystem_panics_total", Help: "Supervised subsystems that panicked and were restarted.",
		}, []string{"subsystem"}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kuben_reconcile_errors_total", Help: "Failed reconciliations, by kind.",
		}, []string{"kind"}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kuben_leader", Help: "1 while this replica holds the controller lease.",
		}),
		sseLagged: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kuben_sse_lagged_total", Help: "Deltas an event stream subscriber missed because it lagged.",
		}),
		auditWriteErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kuben_audit_write_errors_total", Help: "Audit records that could not be written.",
		}),
	}
	m.registry.MustRegister(m.subsystemFailures, m.subsystemPanics, m.reconcileErrors, m.leader, m.sseLagged, m.auditWriteErrors)
	return m
}

// SubsystemFailed counts a subsystem that failed and restarts.
func (m *Metrics) SubsystemFailed(name string) {
	if m == nil {
		return
	}
	m.subsystemFailures.WithLabelValues(name).Inc()
}

// SubsystemPanicked counts a subsystem that panicked and restarts.
func (m *Metrics) SubsystemPanicked(name string) {
	if m == nil {
		return
	}
	m.subsystemPanics.WithLabelValues(name).Inc()
}

// ReconcileFailed counts a failed reconciliation of kind.
func (m *Metrics) ReconcileFailed(kind string) {
	if m == nil {
		return
	}
	m.reconcileErrors.WithLabelValues(kind).Inc()
}

// Leading sets whether this replica holds the controller lease.
func (m *Metrics) Leading(leading bool) {
	if m == nil {
		return
	}
	if leading {
		m.leader.Set(1)
		return
	}
	m.leader.Set(0)
}

// SSELagged counts deltas a stream subscriber missed.
func (m *Metrics) SSELagged(n uint64) {
	if m == nil {
		return
	}
	m.sseLagged.Add(float64(n))
}

// AuditWriteFailed counts an audit record that could not be written.
func (m *Metrics) AuditWriteFailed() {
	if m == nil {
		return
	}
	m.auditWriteErrors.Inc()
}

// Handler is the Prometheus text exposition of the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Serve exposes the metrics at bind (any path, as the Rust exporter
// answered) until ctx ends. An address that does not parse or cannot be
// bound is logged, not fatal: metrics are then off.
func Serve(ctx context.Context, m *Metrics, bind string, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if _, err := net.ResolveTCPAddr("tcp", bind); err != nil {
		logger.Warn("invalid metrics bind address; metrics disabled", "bind", bind)
		close(done)
		return done
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", bind)
	if err != nil {
		logger.Warn("failed to install metrics exporter", "error", err)
		close(done)
		return done
	}
	srv := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("prometheus metrics listening", "addr", ln.Addr().String())
	go func() { //nolint:forbidigo // owned: shut down when ctx ends
		defer close(done)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(ln) }() //nolint:forbidigo // owned: ends with Shutdown below
		select {
		case err := <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				logger.Warn("metrics exporter stopped", "error", err)
			}
			return
		case <-ctx.Done():
		}
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(stop) //nolint:errcheck // best effort on the way out
		<-served
	}()
	return done
}

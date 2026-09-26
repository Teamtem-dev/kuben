package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"

	"github.com/go-faster/jx"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
)

// livez: the runtime schedules work (the watchdog beat within 10 s).
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Health.IsLive(10_000) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// readyz: the initial sync is done and the database answers.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.deps.Health.IsReady() && s.deps.Store.Ping(r.Context()) == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// GetHealthDetails is the health of every subsystem (authenticated).
func (s *Server) GetHealthDetails(ctx context.Context) (*gen.HealthDetails, error) {
	if _, ok := httpx.UserFrom(ctx); !ok {
		return nil, kerrors.ErrUnauthorized
	}
	subsystems := gen.HealthDetailsSubsystems{}
	for _, n := range s.deps.Health.Details() {
		raw, err := json.Marshal(n.Subsystem)
		if err != nil {
			return nil, kerrors.Wrap(err, "health details")
		}
		subsystems[n.Name] = jx.Raw(raw)
	}
	return &gen.HealthDetails{
		Ready:      s.deps.Health.IsReady(),
		Database:   s.deps.Store.Backend(),
		Cluster:    s.deps.Cluster.IsSome(),
		Seq:        int64(min(s.deps.Projections.Seq(), math.MaxInt64)), //nolint:gosec // bounded
		Pods:       s.deps.Projections.PodCount(),
		Subsystems: subsystems,
	}, nil
}

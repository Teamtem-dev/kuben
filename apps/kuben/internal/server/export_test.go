package server

import (
	"context"
	"log/slog"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
)

// Internals under test.
var (
	Election = election
	WaitAll  = waitAll
)

// StartCluster starts the cluster subsystems of a server with cfg's roles,
// without a store (discovery records nothing without the controller role).
func StartCluster(ctx context.Context, cfg config.Config, r *registry.Registry, p *projection.Projections,
	h *health.Health, logger *slog.Logger,
) []<-chan struct{} {
	return clusterWork{
		cfg: cfg, registry: r, projections: p, health: h, logger: logger, facts: &discovery.Watch{},
	}.start(ctx)
}

package controller

import (
	"context"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
)

// ParseDuration exposes parseDuration to the tests.
func ParseDuration(s string) (uint64, bool) { return parseDuration(s) }

// Reconcilers are the reconcilers over one client (the fake client serves
// cached and live reads alike).
type Reconcilers struct {
	Project     reconcile.Reconciler
	Environment reconcile.Reconciler
	App         reconcile.Reconciler
	gateway     *gatewayReconciler
}

// NewReconcilers builds the reconcilers the way RunAll does.
func NewReconcilers(c client.Client, facts *discovery.Watch, clk clock.Clock, projections *projection.Projections) Reconcilers {
	s := &shared{client: c, reader: c, facts: facts, clock: clk, logger: slog.New(slog.DiscardHandler)}
	return Reconcilers{
		Project:     projectReconciler{s},
		Environment: environmentReconciler{s},
		App:         appReconciler{s},
		gateway:     &gatewayReconciler{shared: s, projections: projections},
	}
}

// Gateway runs one pass of the gateway reconciler.
func (r Reconcilers) Gateway(ctx context.Context) error { return r.gateway.reconcile(ctx) }

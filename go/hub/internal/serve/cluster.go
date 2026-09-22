package serve

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/leader"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/supervise"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// election is the leader-election settings, or none when
// kube.leader_election is off. A controller replica that must campaign
// but has no namespace for its Lease stops the server.
func election(cfg config.Config) (opt.Val[leader.Election], error) {
	if !cfg.Kube.LeaderElection {
		return opt.None[leader.Election](), nil
	}
	ns, ok := registry.OwnNamespace(cfg.Kube.Namespace).Get()
	if !ok {
		return opt.None[leader.Election](), errors.New(
			"kube.leader_election needs a namespace for its Lease: set KUBEN_KUBE__NAMESPACE (automatic inside a pod)")
	}
	return opt.Some(leader.Election{Namespace: ns, Identity: leader.Identity()}), nil
}

// clusterWork is what a server with a cluster runs besides the API.
type clusterWork struct {
	cfg         config.Config
	registry    *registry.Registry
	projections *projection.Projections
	store       *store.Store
	health      *health.Health
	logger      *slog.Logger
	election    opt.Val[leader.Election]
	// facts is published by discovery on controller replicas.
	facts *discovery.Watch
}

// start starts the cluster subsystems (serve.rs spawn_cluster_tasks) and
// returns the channels closed when each has ended.
func (c clusterWork) start(ctx context.Context) []<-chan struct{} {
	c.health.OK("cluster")
	primary := c.registry.Primary()
	done := []<-chan struct{}{
		supervise.Go(ctx, "informers", c.health, c.logger, func(ctx context.Context) error {
			return projection.Run(ctx, primary, c.projections, c.logger)
		}),
		c.readyWhenSynced(ctx),
	}
	if !c.cfg.HasRole(config.RoleController) {
		return done
	}
	// What the cluster can do (ADR-031), discovered on every controller
	// replica: rendering and the gateway are gated on it.
	done = append(done, discovery.Start(ctx, c.health, discovery.Deps{
		Logger: c.logger, Cluster: primary, Store: c.store, Watch: c.facts, Clock: clock.System{},
	}))
	if ch, ok := c.controllers(ctx, c.leading()); ok {
		done = append(done, ch)
	}
	return done
}

// readyWhenSynced makes the server ready once every informer has listed
// once: until then the projections are incomplete and lookups would answer
// 404 for objects that exist.
func (c clusterWork) readyWhenSynced(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() { //nolint:forbidigo // owned: ends with ctx, waited for at shutdown
		defer close(done)
		select {
		case <-c.projections.Synced():
			c.logger.Info("informers synced; ready")
			c.health.SetReady(true)
		case <-ctx.Done():
		}
	}()
	return done
}

// leading is the work that runs on one replica at a time (serve.rs
// leading): the controllers and the materializer's drift watch. None is
// part of this build yet (slice S1-D).
func (c clusterWork) leading() []func(context.Context) error {
	return nil
}

// controllers runs work under the controllers subsystem: behind the
// controller Lease when leader election is on, directly otherwise. Without
// work it starts nothing, so this replica never holds the Lease while
// reconciling nothing (a Rust replica would wait for it in vain).
func (c clusterWork) controllers(ctx context.Context, work []func(context.Context) error) (<-chan struct{}, bool) {
	if len(work) == 0 {
		c.logger.Warn("the controller role has no reconcilers in this build yet; not campaigning for the controller lease")
		return nil, false
	}
	all := func(ctx context.Context) error {
		g, ctx := errgroup.WithContext(ctx)
		for _, w := range work {
			g.Go(func() error { return w(ctx) })
		}
		return g.Wait() //nolint:wrapcheck // the supervisor logs it under the subsystem name
	}
	e, elect := c.election.Get()
	return supervise.Go(ctx, leader.Subsystem, c.health, c.logger, func(ctx context.Context) error {
		if !elect {
			return all(ctx)
		}
		return leader.Run(ctx, c.registry.Primary().Typed, e, c.health, c.logger, all) //nolint:wrapcheck // says what failed
	}), true
}

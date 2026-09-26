package server

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/leader"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/supervise"
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
	// keyring opens the secret revisions runs are bound to.
	keyring *keyring.Keyring
	// agents is AgentLink's hub, when it listens.
	agents opt.Val[materializer.AgentDispatch]
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
	// SQL is the only desired-state writer; the materializer writes its
	// resources (ADR-032). Its claims are fenced in SQL, so every replica
	// runs one.
	worker := materializer.New(materializer.Deps{
		Store: c.store, Cluster: primary, ID: leader.Identity(), Logger: c.logger, Clock: clock.System{},
		Facts: opt.Some(c.facts), Keyring: opt.Some(c.keyring), Agents: c.agents,
	})
	return append(done,
		c.controllers(ctx, worker),
		supervise.Go(ctx, materializer.Subsystem, c.health, c.logger, func(ctx context.Context) error {
			return materializer.Run(ctx, worker, c.health)
		}),
	)
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

// controllers runs what runs on one replica at a time (serve.rs leading)
// under the controllers subsystem: the reconcilers and the materializer's
// drift watch, behind the controller Lease when leader election is on.
func (c clusterWork) controllers(ctx context.Context, worker *materializer.Worker) <-chan struct{} {
	leading := func(ctx context.Context) error {
		g, ctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return controller.RunAll(ctx, controller.Deps{
				Logger: c.logger, Cluster: c.registry.Primary(), Projections: c.projections, Facts: c.facts,
				Health: c.health, Clock: clock.System{},
			})
		})
		g.Go(func() error { return materializer.WatchDrift(ctx, worker) })
		return g.Wait() //nolint:wrapcheck // the supervisor logs it under the subsystem name
	}
	e, elect := c.election.Get()
	return supervise.Go(ctx, leader.Subsystem, c.health, c.logger, func(ctx context.Context) error {
		if !elect {
			return leading(ctx)
		}
		return leader.Run(ctx, c.registry.Primary().Typed, e, c.health, c.logger, leading) //nolint:wrapcheck // says what failed
	})
}

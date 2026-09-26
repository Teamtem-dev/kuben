package server

// AgentLink in `kuben serve` (serve.rs agent_link, spawn_local_agent and
// the agentlink task): the hub's endpoint for cluster agents (ADR-027). It
// needs no kubeconfig of its own; the materializer hands it envelopes.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Teamtem-dev/kuben/internal/agentlink"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/supervise"
)

// agentLink is AgentLink of a controller replica, when `agent.bind` is
// set. In a pod the state directory is not kept: the CA agents pin lives
// in a Secret (M2.8).
func agentLink(ctx context.Context, cfg config.Config, st *store.Store, cluster opt.Val[*registry.Registry],
	logger *slog.Logger,
) (opt.Val[*agentlink.AgentLink], error) {
	none := opt.None[*agentlink.AgentLink]()
	if !cfg.HasRole(config.RoleController) {
		return none, nil
	}
	if r, ok := cluster.Get(); ok && cfg.Agent.Bind.IsSome() && config.InCluster() {
		namespace := agentlink.LocalNamespace(cfg.Agent, cfg.Kube.Namespace)
		if err := agentlink.SyncCA(ctx, r.Primary().Typed, namespace, cfg.StateDir(), logger); err != nil {
			return none, fmt.Errorf("keeping the AgentLink CA in its Secret: %w", err)
		}
	}
	link, ok, err := agentlink.Build(cfg.Agent, cfg.StateDir(), st, clock.System{}, logger)
	if err != nil || !ok {
		return none, err //nolint:wrapcheck // says what failed
	}
	return opt.Some(link), nil
}

// agentDispatch is the handle the materializer hands envelopes to.
func agentDispatch(link opt.Val[*agentlink.AgentLink]) opt.Val[materializer.AgentDispatch] {
	l, ok := link.Get()
	if !ok {
		return opt.None[materializer.AgentDispatch]()
	}
	return opt.Some[materializer.AgentDispatch](l.Hub())
}

// startAgentLink runs the AgentLink listener and, with `agent.local` and a
// cluster, the local agent's enrollment publisher.
func startAgentLink(ctx context.Context, cfg config.Config, st *store.Store, cluster opt.Val[*registry.Registry],
	link opt.Val[*agentlink.AgentLink], h *health.Health, logger *slog.Logger,
) []<-chan struct{} {
	l, ok := link.Get()
	if !ok {
		return nil
	}
	var done []<-chan struct{}
	if r, inCluster := cluster.Get(); cfg.Agent.Local && inCluster {
		deps := agentlink.LocalAgentDeps{
			Store: st, Client: r.Primary().Typed, Config: cfg.Agent, OrgSlug: cfg.Bootstrap.OrgSlug,
			StateDir: cfg.StateDir(), Namespace: agentlink.LocalNamespace(cfg.Agent, cfg.Kube.Namespace),
			Clock: clock.System{}, Logger: logger,
		}
		done = append(done, supervise.Go(ctx, "local-agent", h, logger, func(ctx context.Context) error {
			return agentlink.RunLocalAgent(ctx, deps)
		}))
	}
	return append(done, supervise.Go(ctx, "agentlink", h, logger, l.Serve))
}

package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/supervise"
	"github.com/Teamtem-dev/kuben/internal/usage"
)

// startUsage starts the collector of this replica's live usage window
// (serve.rs spawn_usage) under its supervisor and returns the window, and
// the channel closed when the collector has ended.
func startUsage(
	ctx context.Context, cluster registry.Cluster, st *store.Store, h *health.Health, logger *slog.Logger,
) (*usage.Buffer, <-chan struct{}) {
	buffer := &usage.Buffer{}
	deps := usage.Deps{
		Cluster: cluster.Dynamic, Buffer: buffer, Keep: storeRollups(st),
		Health: h, Clock: clock.System{}, Logger: logger,
	}
	done := supervise.Go(ctx, usage.Subsystem, h, logger, func(ctx context.Context) error {
		return usage.Run(ctx, deps)
	})
	return buffer, done
}

// storeRollups stores hourly usage (M5.5) in the app's organization; an
// app the store does not know (any more) is skipped.
func storeRollups(st *store.Store) usage.Keep {
	return func(ctx context.Context, key usage.SeriesKey, r usage.Rollup) error {
		org, err := ids.Parse[ids.Org](key.Org)
		if err != nil {
			return fmt.Errorf("organization label %q: %w", key.Org, err)
		}
		t, err := st.Tenant(ctx, org)
		if err != nil {
			return fmt.Errorf("usage of %s/%s: %w", key.Namespace, key.App, err)
		}
		defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		target, found, err := t.TargetByName(ctx, key.Namespace, key.App)
		if err != nil || !found {
			return err //nolint:wrapcheck // a store error naming its operation
		}
		if err := t.KeepUsage(ctx, target, store.UsageRollup(r)); err != nil {
			return err //nolint:wrapcheck // a store error naming its operation
		}
		return t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
	}
}

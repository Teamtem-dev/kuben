package discovery

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/supervise"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

const (
	// Subsystem is the health entry of the discovery loop.
	Subsystem = "discovery"
	// Interval is how often discovery runs.
	Interval = time.Minute
	// FirstRetry is the pause while the API groups cannot be listed: until
	// discovery sees the cluster at all, it retries sooner.
	FirstRetry = 5 * time.Second
	// Rewrite is how often unchanged facts are still written, so
	// organizations and clusters created meanwhile get them.
	Rewrite = 10 * time.Minute
)

// Deps is what the discovery loop works with.
type Deps struct {
	Logger *slog.Logger
	// Cluster is the primary cluster, whose facts are discovered.
	Cluster registry.Cluster
	Store   *store.Store
	// Watch receives the facts when they change.
	Watch *Watch
	Clock clock.Clock
}

// written is what was recorded last, when (unix ms), and for how many
// organizations: a new organization gets the facts at once, not at the
// next rewrite.
type written struct {
	facts ClusterFacts
	atMs  int64
	orgs  int
}

// loop is the state of [Run] between rounds.
type loop struct {
	deps    Deps
	written opt.Val[written]
}

// Run discovers the primary cluster's capabilities until ctx ends: it
// publishes them on d.Watch when they change and records them in SQL for
// every organization's `primary` cluster. It returns nil once ctx ends; a
// failure to record is logged and retried at the next round.
func Run(ctx context.Context, d Deps) error {
	l := &loop{deps: d}
	for {
		pause, ok := l.round(ctx)
		if !ok {
			return nil
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Start runs [Run] under supervise as the Subsystem entry of h; the
// channel is closed when it ended.
func Start(ctx context.Context, h *health.Health, d Deps) <-chan struct{} {
	return supervise.Go(ctx, Subsystem, h, d.Logger, func(ctx context.Context) error { return Run(ctx, d) })
}

// round discovers once, publishes and records; it returns the pause before
// the next round, and false when ctx ended meanwhile (the probes it
// cancelled would report every fact unknown, so nothing is published).
func (l *loop) round(ctx context.Context) (time.Duration, bool) {
	d := l.deps
	facts := Discover(ctx, d.Logger, d.Cluster)
	if ctx.Err() != nil {
		return 0, false
	}
	if d.Watch.Publish(facts) {
		d.Logger.Info("cluster capabilities", "capabilities", facts.Summary())
	}
	orgs := 0
	if ids, err := d.Store.OrgIDs(ctx); err == nil {
		orgs = len(ids)
	}
	now := d.Clock.NowMs()
	due := true
	if last, ok := l.written.Get(); ok {
		due = !last.facts.Equal(facts) || clock.SaturatingSub(now, last.atMs) >= Rewrite.Milliseconds() || last.orgs != orgs
	}
	if due {
		if _, err := d.Store.RecordCapabilitiesEverywhere(ctx, registry.Primary, facts, now); err != nil {
			d.Logger.Warn("cannot record the cluster capabilities", "error", err)
		} else {
			l.written = opt.Some(written{facts: facts.clone(), atMs: now, orgs: orgs})
		}
	}
	if slices.Contains(facts.Unknown, ProbeAPIGroups) {
		return FirstRetry, true
	}
	return Interval, true
}

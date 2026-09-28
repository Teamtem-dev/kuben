package httpapi

// Build changes on the event stream (2.1): every replica that serves
// /api/v1/stream reads, while a stream is open on it, the build attempts
// that changed since its last look (store.BuildsChangedSince, every
// organization) and publishes each whose BuildDto changed as a `build`
// delta (projection.BuildChanged, which documents its JSON). The build
// worker may run on another replica: SQL is what they share.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const (
	// BuildFeedSubsystem is the build feed's name in the health details.
	BuildFeedSubsystem = "build-feed"
	// buildFeedInterval is how often the feed looks for changed builds.
	buildFeedInterval = 2 * time.Second
	// buildFeedOverlap is how far back a fresh look starts: a change
	// committed just before is not missed.
	buildFeedOverlap = 5 * time.Second
	// buildFeedSkew is how far behind the newest change a look starts:
	// replicas' clocks differ a little, and a change read twice is
	// published once.
	buildFeedSkew = 2 * time.Second
	// buildFeedBatch is the most changes read of one organization at a
	// time; the next look goes on from the newest.
	buildFeedBatch = 500
)

// buildSeen is what the feed last published of one build.
type buildSeen struct {
	dto       string
	updatedAt int64
}

// buildReader is where the feed reads changes: the store.
type buildReader interface {
	orgs(ctx context.Context) ([]ids.OrgID, error)
	changed(ctx context.Context, org ids.OrgID, since int64) ([]store.BuildChange, error)
}

// storeBuilds reads changes from the store.
type storeBuilds struct{ s *Server }

func (r storeBuilds) orgs(ctx context.Context) ([]ids.OrgID, error) {
	orgs, err := r.s.deps.Store.OrgIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing organizations: %w", err)
	}
	return orgs, nil
}

func (r storeBuilds) changed(ctx context.Context, org ids.OrgID, since int64) ([]store.BuildChange, error) {
	return r.s.changedBuilds(ctx, org, since)
}

// buildFeed is the state of a replica's feed.
type buildFeed struct {
	read buildReader
	// since is the updated_at the next look starts at (unix ms).
	since int64
	// seen is what was published of the builds changed at or after since.
	seen map[ids.BuildAttemptID]buildSeen
}

// RunBuildFeed publishes build changes on the event stream until ctx
// ends; a failed look is logged and tried again at the next tick.
func (s *Server) RunBuildFeed(ctx context.Context) error {
	s.deps.Health.OK(BuildFeedSubsystem)
	feed := &buildFeed{read: storeBuilds{s: s}, seen: map[ids.BuildAttemptID]buildSeen{}}
	feed.reset(s.deps.Clock.NowMs())
	tick := time.NewTicker(buildFeedInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if err := s.lookForBuilds(ctx, feed); err != nil && ctx.Err() == nil {
			s.deps.Logger.Warn("build changes are late", "error", err)
		}
	}
}

// reset starts the feed over from now.
func (f *buildFeed) reset(nowMs int64) {
	f.since = nowMs - buildFeedOverlap.Milliseconds()
	clear(f.seen)
}

// lookForBuilds publishes what changed since the last look. Without a
// stream open it only keeps up with the time.
func (s *Server) lookForBuilds(ctx context.Context, f *buildFeed) error {
	p := s.deps.Projections
	if p == nil || !p.Subscribed() {
		f.reset(s.deps.Clock.NowMs())
		return nil
	}
	orgs, err := f.read.orgs(ctx)
	if err != nil {
		return err
	}
	newest := f.since
	for _, org := range orgs {
		changed, err := f.read.changed(ctx, org, f.since)
		if err != nil {
			return err
		}
		for _, c := range changed {
			newest = max(newest, c.UpdatedAt)
			dto := buildDto(c.Attempt)
			data, err := json.Marshal(&dto)
			if err != nil {
				return fmt.Errorf("encoding a build: %w", err)
			}
			if prev, ok := f.seen[c.Attempt.ID]; ok && prev.dto == string(data) {
				continue
			}
			f.seen[c.Attempt.ID] = buildSeen{dto: string(data), updatedAt: c.UpdatedAt}
			p.PublishBuild(projection.BuildChanged{
				Org: org.String(), App: c.Namespace + "/" + c.AppSlug,
				Project: c.ProjectSlug, Environment: c.EnvironmentSlug, Name: c.AppSlug, Build: data,
			})
		}
	}
	f.since = max(f.since, newest-buildFeedSkew.Milliseconds())
	// What changed before since is not read again.
	for id, seen := range f.seen {
		if seen.updatedAt < f.since {
			delete(f.seen, id)
		}
	}
	return nil
}

// changedBuilds is the builds of org changed at or after since.
func (s *Server) changedBuilds(ctx context.Context, org ids.OrgID, since int64) ([]store.BuildChange, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("reading the builds of %s: %w", org, err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	changed, err := t.BuildsChangedSince(ctx, since, buildFeedBatch)
	if err != nil {
		return nil, fmt.Errorf("reading the builds of %s: %w", org, err)
	}
	return changed, nil
}

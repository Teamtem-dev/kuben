package httpapi

import (
	"context"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// FixedBuilds are the changes a test's feed reads, by organization.
type FixedBuilds map[ids.OrgID][]store.BuildChange

func (f FixedBuilds) orgs(context.Context) ([]ids.OrgID, error) {
	out := make([]ids.OrgID, 0, len(f))
	for org := range f {
		out = append(out, org)
	}
	return out, nil
}

func (f FixedBuilds) changed(_ context.Context, org ids.OrgID, since int64) ([]store.BuildChange, error) {
	var out []store.BuildChange
	for _, c := range f[org] {
		if c.UpdatedAt >= since {
			out = append(out, c)
		}
	}
	return out, nil
}

// BuildFeed is a feed reading from builds, started at nowMs.
type BuildFeed struct{ f *buildFeed }

// NewBuildFeed is a feed of builds started at nowMs.
func NewBuildFeed(builds FixedBuilds, nowMs int64) BuildFeed {
	f := &buildFeed{read: builds, seen: map[ids.BuildAttemptID]buildSeen{}}
	f.reset(nowMs)
	return BuildFeed{f: f}
}

// Look is one look of the feed on s's projections.
func (b BuildFeed) Look(ctx context.Context, s *Server) error { return s.lookForBuilds(ctx, b.f) }

// Since is where the next look starts.
func (b BuildFeed) Since() int64 { return b.f.since }

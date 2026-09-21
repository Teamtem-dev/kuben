package api

import (
	"context"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// access resolves the caller's authority for the request in ctx.
func (s *Server) access(ctx context.Context) (access.Access, error) {
	return access.Resolve(ctx, s.deps.Store, s.policy) //nolint:wrapcheck // kerr errors already
}

// Timestamp is the RFC 3339 time of a millisecond timestamp, with as many
// fractional digits as it needs (routes/request.rs, jiff's Display).
func Timestamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// projectDto is routes/projects.rs ProjectDto::of. Readiness comes from the
// projections of the cluster (slice S1); without them it is false, as in
// Rust without a cluster.
func projectDto(org ids.OrgID, p store.Project) gen.ProjectDto {
	return gen.ProjectDto{
		Name:         p.Slug,
		UID:          gen.NewOptNilString(p.ID.String()),
		DisplayName:  p.Name,
		Description:  optNilString(p.Description),
		Org:          gen.NewOptNilString(org.String()),
		Environments: int32(min(p.Environments, 1<<31-1)), //nolint:gosec // bounded
		Ready:        false,
		Deleting:     p.Deleting,
		CreatedAt:    gen.NewOptNilString(Timestamp(p.CreatedAt)),
	}
}

// ListProjects lists the projects of the caller's organizations.
func (s *Server) ListProjects(ctx context.Context) ([]gen.ProjectDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	items := []gen.ProjectDto{}
	for _, org := range a.OrgIDs() {
		projects, err := s.projectsOf(ctx, org)
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			items = append(items, projectDto(org, p))
		}
	}
	return items, nil
}

func (s *Server) projectsOf(ctx context.Context, org ids.OrgID) ([]store.Project, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx)  //nolint:errcheck // read only
	return t.Projects(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// GetProject is one project.
func (s *Server) GetProject(ctx context.Context, params gen.GetProjectParams) (gen.GetProjectRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.ProjectRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	dto := projectDto(p.org, p.project)
	return &dto, nil
}

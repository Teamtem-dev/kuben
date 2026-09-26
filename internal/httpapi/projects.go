package httpapi

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// access resolves the caller's authority for the request in ctx.
func (s *Server) access(ctx context.Context) (access.Access, error) {
	return access.Resolve(ctx, s.deps.Store, s.policy) //nolint:wrapcheck // kerrors errors already
}

// Timestamp is the RFC 3339 time of a millisecond timestamp, with as many
// fractional digits as it needs (routes/request.rs, jiff's Display).
func Timestamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// projectDto is routes/projects.rs ProjectDto::of: ready when the
// project's projection is ready and the project is not being deleted.
func projectDto(org ids.OrgID, p store.Project, view opt.Val[projection.ProjectView]) gen.ProjectDto {
	v, seen := view.Get()
	return gen.ProjectDto{
		Name:         p.Slug,
		UID:          gen.NewOptNilString(p.ID.String()),
		DisplayName:  p.Name,
		Description:  optNilString(p.Description),
		Org:          gen.NewOptNilString(org.String()),
		Environments: int32(min(p.Environments, 1<<31-1)), //nolint:gosec // bounded
		Ready:        seen && v.Ready && !p.Deleting,
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
			items = append(items, projectDto(org, p, s.projectView(org, p.Slug)))
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
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	dto := projectDto(p.org, p.project, p.view)
	return &dto, nil
}

// CreateProject creates a project in the caller's first organization and
// asks the materializer to write its resource (routes/projects.rs create).
func (s *Server) CreateProject(ctx context.Context, req *gen.CreateProject) (gen.CreateProjectRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := DNSLabel("name", req.Name, 40); err != nil {
		return nil, err
	}
	displayName := strings.TrimFunc(req.DisplayName, unicode.IsSpace)
	if displayName == "" || len(displayName) > 100 {
		return nil, kerrors.New(kerrors.Validation, "display_name must be 1–100 characters")
	}
	orgs := a.OrgIDs()
	if len(orgs) == 0 {
		return nil, kerrors.ErrForbidden
	}
	org := orgs[0]
	if _, err := a.Require(perm.ProjectWrite, authz.OrgChain(org)); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	// Project resources are cluster-wide: a name another organization
	// holds there is taken.
	taken := kerrors.New(kerrors.Conflict, "project `%s` already exists", req.Name)
	if v, ok := s.deps.Projections.Project(req.Name); ok && v.Org != opt.Some(org.String()) {
		return nil, taken
	}
	description := opt.None[string]()
	if d, ok := req.Description.Get(); ok {
		if d = strings.TrimFunc(d, unicode.IsSpace); d != "" {
			description = opt.Some(d)
		}
	}
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	id, err := t.CreateProjectDescribed(ctx, req.Name, displayName, description)
	if err != nil {
		return nil, duplicate(err, "project `"+req.Name+"`")
	}
	if _, err := t.Request(ctx, store.ProjectApply, store.ProjectSubject(id), actor,
		requestAudit(a, store.ProjectApply, "project", req.Name)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	project, found, err := t.Project(ctx, req.Name)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return nil, taken
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := projectDto(org, project, opt.None[projection.ProjectView]())
	return &dto, nil
}

// DeleteProject deletes an empty project (routes/projects.rs delete).
// Projects with environments are refused (409): deleting environments is an
// explicit, per-environment decision.
func (s *Server) DeleteProject(ctx context.Context, params gen.DeleteProjectParams) (gen.DeleteProjectRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.ProjectWrite, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	remaining, err := t.LiveEnvironments(ctx, p.project.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if remaining > 0 {
		return nil, kerrors.New(kerrors.Conflict, "project `%s` still has %d environment(s); delete them first",
			params.Project, remaining)
	}
	marked, err := t.MarkProjectDeleting(ctx, p.project.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !marked {
		return nil, kerrors.New(kerrors.Conflict, "project `%s` is being deleted", params.Project)
	}
	_, actor := a.Actor()
	if _, err := t.Request(ctx, store.ProjectDelete, store.ProjectSubject(p.project.ID), actor,
		requestAudit(a, store.ProjectDelete, "project", params.Project)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteProjectNoContent{}, nil
}

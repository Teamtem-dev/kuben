package api

import (
	"context"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// Scopes (routes/scope.rs): URL path segments resolved into authorized
// scopes on the SQL model (ADR-032). A project or environment is found by
// slug in each of the caller's organizations in turn; one that does not
// exist, or exists only in an organization the caller does not belong to,
// is 404, not 403, so its existence does not leak. The chain names every
// node by its SQL id; a row that came from the resource model keeps its
// Kubernetes UID as an alias. Live status from the projections is not
// ported yet (slice S1).

func scopeNotFound(kind, name string) error {
	return kerr.New(kerr.NotFound, "%s `%s`", kind, name)
}

// EnvironmentShortName is the environment name used in URLs and the UI:
// resource without its `<project>-` prefix, or resource itself when that
// would leave nothing.
func EnvironmentShortName(project, resource string) string {
	if rest, ok := strings.CutPrefix(resource, project); ok {
		if short, ok := strings.CutPrefix(rest, "-"); ok && short != "" {
			return short
		}
	}
	return resource
}

// projectScope is ProjectScope: a live project of one of the caller's
// organizations.
type projectScope struct {
	org     ids.OrgID
	project store.Project
	// view is the project's projection, when the cluster has it for this
	// organization.
	view opt.Val[projection.ProjectView]
}

func (p projectScope) chain() authz.ScopeChain {
	c := authz.ProjectChain(p.org, p.project.ID.UUID())
	if legacy, ok := p.project.LegacyUID.Get(); ok {
		c.Aliases = []authz.ScopeRef{{Level: authz.LevelProject, ID: legacy}}
	}
	return c
}

// findProject is scope::project: the project `name` in the first of the
// caller's organizations that has one.
func (s *Server) findProject(ctx context.Context, a access.Access, name string) (projectScope, error) {
	for _, org := range a.OrgIDs() {
		found, ok, err := s.projectNamed(ctx, org, name)
		if err != nil {
			return projectScope{}, err
		}
		if ok {
			return projectScope{org: org, project: found, view: s.projectView(org, found.Slug)}, nil
		}
	}
	return projectScope{}, scopeNotFound("project", name)
}

// projectView is the projection of project slug, unless it belongs to
// another organization than org.
func (s *Server) projectView(org ids.OrgID, slug string) opt.Val[projection.ProjectView] {
	v, ok := s.deps.Projections.Project(slug)
	if !ok || v.Org != opt.Some(org.String()) {
		return opt.None[projection.ProjectView]()
	}
	return opt.Some(*v)
}

func (s *Server) projectNamed(ctx context.Context, org ids.OrgID, name string) (store.Project, bool, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return store.Project{}, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx)       //nolint:errcheck // read only
	return t.Project(ctx, name) //nolint:wrapcheck // a store error, answered as internal
}

// envScope is EnvScope: a live environment of a project.
type envScope struct {
	project projectScope
	env     store.EnvironmentRecord
	// view is the environment's projection, when the cluster has it.
	view opt.Val[projection.EnvironmentView]
}

// namespace is the environment's namespace: its placement's, else the one
// its controller creates.
func (e envScope) namespace() string {
	return e.env.Namespace.Or(render.NamespaceName(e.resourceName()))
}

// resourceName is the environment's Kubernetes object name.
func (e envScope) resourceName() string {
	return render.EnvironmentResourceName(e.project.project.Slug, e.env.Slug)
}

// deleting reports whether the environment, or its project, is being
// deleted (scope.rs EnvScope::deleting).
func (e envScope) deleting() bool {
	return e.env.Deleting || e.project.project.Deleting
}

func (e envScope) chain() authz.ScopeChain {
	c := e.project.chain()
	c.Environment = opt.Some(e.env.ID.UUID())
	if legacy, ok := e.env.LegacyUID.Get(); ok {
		c.Aliases = append(c.Aliases, authz.ScopeRef{Level: authz.LevelEnvironment, ID: legacy})
	}
	return c
}

// findEnvironment is scope::environment: environment env of the project
// `project` (found as findProject finds it).
func (s *Server) findEnvironment(ctx context.Context, a access.Access, project, env string) (envScope, error) {
	p, err := s.findProject(ctx, a, project)
	if err != nil {
		return envScope{}, err
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return envScope{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	record, found, err := t.Environment(ctx, p.project.ID, env)
	if err != nil {
		return envScope{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return envScope{}, scopeNotFound("environment", env)
	}
	return envScope{
		project: p, env: record, view: s.environmentView(render.EnvironmentResourceName(p.project.Slug, record.Slug)),
	}, nil
}

// environmentView is the projection of the environment object resource.
func (s *Server) environmentView(resource string) opt.Val[projection.EnvironmentView] {
	v, ok := s.deps.Projections.Environment(resource)
	if !ok {
		return opt.None[projection.EnvironmentView]()
	}
	return opt.Some(*v)
}

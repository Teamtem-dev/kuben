package httpapi

// Building an app's source now (2.1): the same request as a source sync
// (apps_source.go SyncAppSource): the branch head is read and built unless
// it is built already. The answer names the sync, and the newest build as
// it stands, which the sync may replace with a new one.

import (
	"context"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
)

// TriggerBuild asks for a build of the source's branch head: 409 when the
// app has no Git source.
func (s *Server) TriggerBuild(ctx context.Context, params gen.TriggerBuildParams) (gen.TriggerBuildRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppDeploy, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	binding, found, err := t.BindingOfTarget(ctx, app.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` has no Git source to build", params.App)
	}
	reference := params.Project + "/" + params.Environment + "/" + params.App
	data := map[string]any{"repository": binding.Repository.String(), "branch": binding.Branch.String()}
	operation, err := t.RequestSync(ctx, binding, requestedBy(a), sourceAudit(a, "syncSource", reference, data))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	newest, err := t.BuildsOfTarget(ctx, app.app.Target, 1)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := &gen.TriggeredBuildDto{SyncOperation: operation.String()}
	out.Build.SetToNull()
	if len(newest) > 0 {
		out.Build = gen.NewOptNilBuildDto(buildDto(newest[0]))
	}
	return out, nil
}

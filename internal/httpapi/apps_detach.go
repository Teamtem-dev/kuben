package api

// The export and detach routes (routes/apps/export.rs, M4.11); the
// document itself is apps_export.go.

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// exportOf is the export of app now; 409 before its first delivered run.
func (s *Server) exportOf(ctx context.Context, app appScope, project, environment string) (map[string]any, error) {
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	material, found, err := t.ExportMaterial(ctx, app.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerr.New(kerr.Conflict, "app `%s` has not been delivered yet", app.app.Slug)
	}
	return ExportDocument(ExportSubject{
		Project: project, Environment: environment, App: app.app.Slug, Namespace: app.app.Namespace,
		Delivery: string(app.app.Delivery), ExportedAt: Timestamp(s.deps.Clock.NowMs()),
	}, material), nil
}

// ExportApp exports an app: portable release and configuration, standard
// manifests, references, inventory and runbook. No secret values.
func (s *Server) ExportApp(ctx context.Context, params gen.ExportAppParams) (gen.ExportAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	doc, err := s.exportOf(ctx, app, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	raw, err := rawObject(doc)
	if err != nil {
		return nil, err
	}
	out := gen.ExportAppOK(raw)
	return &out, nil
}

// detachedDto is DetachedAppDto::from, without the export.
func detachedDto(d store.DetachedApp) *gen.DetachedAppDto {
	at := func(v opt.Val[int64]) gen.OptNilString {
		if ms, ok := v.Get(); ok {
			return gen.NewOptNilString(Timestamp(ms))
		}
		var n gen.OptNilString
		n.SetToNull()
		return n
	}
	return &gen.DetachedAppDto{
		ID: d.TargetID, App: d.App, Namespace: d.Namespace, Reason: d.Reason, RequestedBy: d.RequestedBy,
		RequestedAt: Timestamp(d.RequestedAt), CompletedAt: at(d.CompletedAt), ReleasedAt: at(d.ReleasedAt),
		ReleasedBy: optNilString(d.ReleasedBy),
	}
}

// withExport is dto holding export.
func withExport(dto *gen.DetachedAppDto, export map[string]any) (*gen.DetachedAppDto, error) {
	raw, err := rawObject(export)
	if err != nil {
		return nil, err
	}
	dto.Export = gen.NewOptNilDetachedAppDtoExport(raw)
	return dto, nil
}

// DetachApp detaches an app: Kuben lets go of it and leaves its objects
// running.
func (s *Server) DetachApp(ctx context.Context, req *gen.DetachRequest, params gen.DetachAppParams) (gen.DetachAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppWrite, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	reason := strings.TrimSpace(req.Reason)
	if req.Confirm != params.App {
		return nil, kerr.New(kerr.Validation, "confirm must repeat the app's name")
	}
	if reason == "" || utf8.RuneCountInString(reason) > 1024 {
		return nil, kerr.New(kerr.Validation, "reason must be 1 to 1024 characters")
	}
	export, err := s.exportOf(ctx, app, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	record, err := s.recordDetach(ctx, a, app, reason, export, params)
	if err != nil {
		return nil, err
	}
	return withExport(detachedDto(record), export)
}

// recordDetach marks app deleting, records its detach with export and
// asks for the deletion that orphans its objects, in one transaction.
func (s *Server) recordDetach(
	ctx context.Context, a access.Access, app appScope, reason string, export map[string]any, params gen.DetachAppParams,
) (store.DetachedApp, error) {
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	busy, err := t.RunInFlight(ctx, app.app.Target)
	if err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if busy {
		return store.DetachedApp{}, kerr.New(kerr.Conflict,
			"a run of app `%s` has not finished: wait for it or cancel it first", params.App)
	}
	marked, err := t.MarkTargetDeleting(ctx, app.app.Target)
	if err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !marked {
		return store.DetachedApp{}, kerr.New(kerr.Conflict, "app `%s` is being deleted", params.App)
	}
	_, actor := a.Actor()
	project, env := app.env.project.project.ID, app.env.env.ID
	if err := t.RecordDetach(ctx, store.NewDetach{
		Project: project, Environment: env, Target: app.app.Target, App: app.app.Slug,
		Namespace: app.app.Namespace, Reason: reason, RequestedBy: actor, Export: export,
	}); err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	audit := requestAudit(a, "app.detached", "app", params.Project+"/"+params.Environment+"/"+params.App)
	audit.Data = opt.Some[any](map[string]any{"reason": reason})
	if _, err := t.Request(ctx, store.TargetDelete, store.DetachSubject(project, env, app.app.Target), actor, audit); err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	record, found, err := t.DetachedApp(ctx, app.app.Target)
	if err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.DetachedApp{}, kerr.New(kerr.Internal, "the detach record is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return record, nil
}

// ListDetachedApps is the apps detached from an environment.
func (s *Server) ListDetachedApps(ctx context.Context, params gen.ListDetachedAppsParams) (gen.ListDetachedAppsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.DetachedApps(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListDetachedAppsOKApplicationJSON, 0, len(found))
	for _, d := range found {
		out = append(out, *detachedDto(d))
	}
	return &out, nil
}

// detachedIn is the detach record id of environment e, 404 when there is
// none there.
func detachedIn(ctx context.Context, t *store.Tenant, e envScope, id uuid.UUID) (store.DetachedApp, error) {
	record, found, err := t.DetachedApp(ctx, ids.From[ids.Target](id))
	if err != nil {
		return store.DetachedApp{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || record.EnvironmentID != e.env.ID.UUID() {
		return store.DetachedApp{}, kerr.New(kerr.NotFound, "detached app `%s`", id)
	}
	return record, nil
}

// GetDetachedApp is one detached app, with its export.
func (s *Server) GetDetachedApp(ctx context.Context, params gen.GetDetachedAppParams) (gen.GetDetachedAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	target := ids.From[ids.Target](params.ID)
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	record, err := detachedIn(ctx, t, e, params.ID)
	if err != nil {
		return nil, err
	}
	export, err := t.DetachedExport(ctx, target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := detachedDto(record)
	if doc, ok := export.Get(); ok {
		object, isObject := doc.(map[string]any)
		if !isObject {
			// Rust returned the stored value as it was; only objects are
			// ever stored.
			return nil, kerr.New(kerr.Internal, "the export of `%s` is not an object", record.App)
		}
		return withExport(dto, object)
	}
	return dto, nil
}

// ReleaseDetachedApp releases a detached app: someone owns it now, and
// deleting the environment no longer keeps its namespace.
func (s *Server) ReleaseDetachedApp(ctx context.Context, params gen.ReleaseDetachedAppParams) (gen.ReleaseDetachedAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.EnvWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	target := ids.From[ids.Target](params.ID)
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	record, err := detachedIn(ctx, t, e, params.ID)
	if err != nil {
		return nil, err
	}
	if record.CompletedAt.IsNone() {
		return nil, kerr.New(kerr.Conflict, "the detach of `%s` has not finished", record.App)
	}
	_, actor := a.Actor()
	released, err := t.ReleaseDetached(ctx, target, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !released {
		return nil, kerr.New(kerr.Conflict, "`%s` was released already", record.App)
	}
	ref := params.Project + "/" + params.Environment + "/" + record.App
	if err := t.AppendAudit(ctx, requestAudit(a, "app.detached.released", "app", ref)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.ReleaseDetachedAppNoContent{}, nil
}

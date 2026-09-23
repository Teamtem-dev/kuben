package api

// Release history and rollback (routes/apps/releases.rs, scenario 5), from
// the app's deployment runs: each run is a revision, numbered by the target
// generation it owns.

import (
	"context"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

const (
	releasePage = 50
	// rollbackReach is how far back a rollback may reach.
	rollbackReach = 1000
)

// actors shows who requested runs: `user:<id>` and `token:<id>` by the
// user's email.
type actors map[string]string

func (s *Server) loadActors(ctx context.Context) (actors, error) {
	users, err := s.deps.Store.ListUsers(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := actors{}
	for _, u := range users {
		out[u.ID.String()] = u.Email
	}
	return out, nil
}

func (a actors) name(requestedBy string) string {
	id := requestedBy
	if _, after, found := strings.Cut(requestedBy, ":"); found {
		id = after
	}
	if email, ok := a[id]; ok {
		return email
	}
	return id
}

// releaseReason is the reason shown for r, told apart from the run before
// it: the first run created the app; a deploy of the same release changed
// the configuration only.
func releaseReason(r store.RunRecord, previous opt.Val[store.RunRecord]) string {
	switch r.Reason {
	case "rollback", "restart", "handover", "build":
		return r.Reason
	case "promotion":
		return "promote"
	}
	p, ok := previous.Get()
	switch {
	case !ok:
		return "create"
	case p.Release == r.Release:
		return "config"
	}
	return "deploy"
}

// ListReleases is the release history, newest first (50 revisions).
func (s *Server) ListReleases(ctx context.Context, params gen.ListReleasesParams) (gen.ListReleasesRes, error) {
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
	// One more than shown: the oldest shown run needs its predecessor.
	runs, _, err := s.runsOf(ctx, app, releasePage+1)
	if err != nil {
		return nil, err
	}
	names, err := s.loadActors(ctx)
	if err != nil {
		return nil, err
	}
	items := gen.ListReleasesOKApplicationJSON{}
	for i, r := range runs {
		if i >= releasePage {
			break
		}
		previous := opt.None[store.RunRecord]()
		if i+1 < len(runs) {
			previous = opt.Some(runs[i+1])
		}
		var note gen.OptNilString
		note.SetToNull()
		items = append(items, gen.ReleaseDto{
			Revision: int64(min(uint64(r.Generation), 1<<63-1)), //nolint:gosec // bounded
			Image:    optNilString(r.Image), Reason: releaseReason(r, previous), Note: note,
			Actor: gen.NewOptNilString(names.name(r.RequestedBy)), CreatedAt: r.CreatedAt, Current: i == 0,
		})
	}
	return &items, nil
}

// rollbackSpec is what a rollback restores: image, processes and env of
// the revision; domains and volumes stay as they are now (data layout never
// moves back).
func rollbackSpec(current, revision v1alpha1.AppSpec) v1alpha1.AppSpec {
	return v1alpha1.AppSpec{
		Source: revision.Source, Runtime: revision.Runtime, Env: revision.Env,
		Domains: current.Domains, Volumes: current.Volumes,
	}
}

// RollbackApp rolls back to an earlier revision: a new run of its release
// and configuration. The app stays on it until automatic deploys resume.
func (s *Server) RollbackApp(ctx context.Context, req *gen.Rollback, params gen.RollbackAppParams) (gen.RollbackAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppDeploy, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	notFound := kerr.New(kerr.NotFound, "revision %d", req.Revision)
	run, config, found, err := s.revision(ctx, app, req.Revision)
	if err != nil || !found {
		return nil, orConflict(err, notFound)
	}
	restored, ok := specOf(opt.Some(config), run.Image)
	if !ok {
		return nil, kerr.New(kerr.Validation, "revision %d cannot be restored", req.Revision)
	}
	current, ok := desiredSpec(app.app)
	if !ok {
		return nil, kerr.New(kerr.Conflict, "app `%s` has no configuration yet", params.App)
	}
	spec := rollbackSpec(current, restored)
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	dto, err := s.deployChangeFor(ctx, a, app, &spec, deployArtifact{release: opt.Some(run.Release)}, store.ReasonRollback)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// revision is the run owning generation revision (within rollbackReach)
// and its configuration; false when there is none.
func (s *Server) revision(ctx context.Context, app appScope, revision int64) (store.RunRecord, any, bool, error) {
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return store.RunRecord{}, nil, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	runs, err := t.Runs(ctx, app.app.Target, rollbackReach)
	if err != nil {
		return store.RunRecord{}, nil, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, r := range runs {
		if revision < 0 || uint64(r.Generation) != uint64(revision) {
			continue
		}
		config, found, err := t.ConfigRevision(ctx, app.app.Target, r.ConfigRevision)
		return r, config, found, err //nolint:wrapcheck // a store error, answered as internal
	}
	return store.RunRecord{}, nil, false, nil
}

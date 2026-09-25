package api

// An app's builds (M3): what was built from which commit, where it stands,
// why it failed, and which release and deployment run it produced
// (routes/apps/builds.rs).

import (
	"context"
	"math"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	defaultBuildsLimit int64 = 20
	maxBuildsLimit     int64 = 100
)

// buildDto is BuildDto::from: absent values are null.
func buildDto(a store.BuildAttempt) gen.BuildDto {
	image := opt.None[string]()
	if d, ok := a.Digest.Get(); ok {
		image = opt.Some(a.ImageRepository + "@" + d.String())
	}
	release := opt.None[string]()
	if r, ok := a.Release.Get(); ok {
		release = opt.Some(r.String())
	}
	deployment := opt.None[string]()
	if r, ok := a.Run.Get(); ok {
		deployment = opt.Some(r.String())
	}
	return gen.BuildDto{
		ID:              a.ID.String(),
		Attempt:         int32(min(a.AttemptNo, math.MaxInt32)), //nolint:gosec // bounded
		Repository:      a.Repository.String(),
		Branch:          a.Branch.String(),
		Commit:          a.Commit.String(),
		Strategy:        string(a.Recipe.Strategy.OrAuto()),
		Phase:           string(a.Phase),
		BlockedReason:   optNilString(a.BlockedReason),
		Failure:         optNilString(a.Failure),
		FailureDetail:   optNilString(a.FailureDetail),
		Image:           optNilString(image),
		Release:         optNilString(release),
		Deployment:      optNilString(deployment),
		DeployDecision:  optNilString(a.DeployDecision),
		CancelRequested: a.CancelRequested,
		CreatedAt:       a.CreatedAt,
		StartedAt:       optNilInt64(a.StartedAt),
		FinishedAt:      optNilInt64(a.FinishedAt),
	}
}

// buildID reads a build id; anything that is not a UUID names no build.
func buildID(value string) (ids.BuildAttemptID, error) {
	u, err := uuid.Parse(value)
	if err != nil {
		return ids.BuildAttemptID{}, kerrors.New(kerrors.NotFound, "build `%s`", value)
	}
	return ids.From[ids.BuildAttempt](u), nil
}

// ListBuilds is the app's newest builds.
func (s *Server) ListBuilds(ctx context.Context, params gen.ListBuildsParams) (gen.ListBuildsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	limit := min(max(params.Limit.Or(defaultBuildsLimit), 1), maxBuildsLimit)
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	builds, err := t.BuildsOfTarget(ctx, app.app.Target, limit)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListBuildsOKApplicationJSON, 0, len(builds))
	for _, b := range builds {
		out = append(out, buildDto(b))
	}
	return &out, nil
}

// GetBuild is one build of the app.
func (s *Server) GetBuild(ctx context.Context, params gen.GetBuildParams) (gen.GetBuildRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	id, err := buildID(params.Build)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	attempt, err := buildOfTarget(ctx, t, app.app.Target, id, params.Build)
	if err != nil {
		return nil, err
	}
	dto := buildDto(attempt)
	return &dto, nil
}

// buildOfTarget is build id of tgt, or 404 naming it as the caller did.
func buildOfTarget(ctx context.Context, t *store.Tenant, tgt ids.TargetID, id ids.BuildAttemptID, name string) (store.BuildAttempt, error) {
	attempt, found, err := t.BuildOfTarget(ctx, tgt, id)
	if err != nil {
		return store.BuildAttempt{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.BuildAttempt{}, kerrors.New(kerrors.NotFound, "build `%s`", name)
	}
	return attempt, nil
}

// CancelBuild stops a build. A build that has not started is cancelled at
// once; a running one is stopped by deleting its Job, and ends `cancelled`
// once the Job is gone, or `succeeded` if its output was already pushed and
// verifies.
func (s *Server) CancelBuild(ctx context.Context, params gen.CancelBuildParams) (gen.CancelBuildRes, error) {
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
	id, err := buildID(params.Build)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	attempt, err := buildOfTarget(ctx, t, app.app.Target, id, params.Build)
	if err != nil {
		return nil, err
	}
	_, asked, err := t.RequestBuildCancel(ctx, app.app.Target, id)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !asked {
		return nil, kerrors.New(kerrors.Conflict, "build `%s` already finished (%s)", params.Build, attempt.Phase)
	}
	after, err := buildOfTarget(ctx, t, app.app.Target, id, params.Build)
	if err != nil {
		return nil, err
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := buildDto(after)
	return &dto, nil
}

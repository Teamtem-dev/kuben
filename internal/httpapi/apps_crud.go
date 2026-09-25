package api

// Apps on the SQL model: create, read, update and delete, and rolling
// restart, which acts on the materialized workloads directly (an app its
// cluster's agent delivers restarts through a run instead)
// (routes/apps/crud.rs).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// ListApps lists the apps of an environment.
func (s *Server) ListApps(ctx context.Context, params gen.ListAppsParams) (gen.ListAppsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	apps, err := t.Apps(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	items := gen.ListAppsOKApplicationJSON{}
	for _, app := range apps {
		dto := appDto(e.project.project.Slug, e.env.Slug, app, s.appView(app.Namespace, app.Slug))
		items = append(items, s.withExposure(dto))
	}
	return &items, nil
}

// CreateApp deploys a new app from a container image, or from a Git
// repository. A tag is resolved to a digest at its registry; a Git app
// deploys with its first build.
func (s *Server) CreateApp(ctx context.Context, req *gen.CreateApp, params gen.CreateAppParams) (gen.CreateAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	if err := DNSLabel("name", req.Name, 40); err != nil {
		return nil, err
	}
	spec, err := specFromCreate(req)
	if err != nil {
		return nil, err
	}
	var dto gen.AppDto
	if git, ok := req.Git.Get(); ok {
		dto, err = s.createGitApp(ctx, a, e, req.Name, spec, &git)
	} else {
		dto, err = s.createApp(ctx, a, e, req.Name, spec)
	}
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// GetApp is the app detail with its pods. Plain env values are included
// only for callers holding `secret-read`.
func (s *Server) GetApp(ctx context.Context, params gen.GetAppParams) (gen.GetAppRes, error) {
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
	_, denied := a.Require(perm.SecretRead, app.chain())
	withValues := denied == nil
	dto := s.withExposure(appDto(app.env.project.project.Slug, app.env.env.Slug, app.app, app.view))
	if spec, ok := desiredSpec(app.app); ok {
		dto.Env = []gen.EnvVarDto{}
		for _, e := range spec.Env {
			dto.Env = append(dto.Env, fromCRDEnv(e, withValues))
		}
	}
	detail := gen.AppDetail{App: dto, Pods: []gen.PodDto{}}
	for _, p := range s.deps.Projections.PodsOfApp(app.app.Namespace, app.app.Slug) {
		if p != nil {
			detail.Pods = append(detail.Pods, podDto(p))
		}
	}
	return &detail, nil
}

// UpdateApp updates an app (image changes require `app-deploy`). Every
// change is a new deployment run; a new tag is resolved to a digest at its
// registry.
func (s *Server) UpdateApp(ctx context.Context, req *gen.UpdateApp, params gen.UpdateAppParams) (gen.UpdateAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	need := perm.AppWrite
	image, newImage := req.Image.Get()
	if newImage {
		need = perm.AppDeploy
	}
	if _, err := a.Require(need, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	if app.app.Deleting {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is being deleted", params.App)
	}
	spec, ok := desiredSpec(app.app)
	if !ok {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` has no configuration yet", params.App)
	}
	before, err := json.Marshal(spec)
	if err != nil {
		return nil, kerrors.Wrap(err, "an app spec")
	}
	if err := applyUpdate(&spec, req); err != nil {
		return nil, err
	}
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	if err := s.ensureDomainsFree(ctx, app.env.project.org, app.app.Namespace, app.app.Slug, &spec); err != nil {
		return nil, err
	}
	if after, err := json.Marshal(spec); err == nil && bytes.Equal(after, before) {
		dto := appDto(app.env.project.project.Slug, app.env.env.Slug, app.app, app.view)
		return &dto, nil
	}
	artifact, err := s.updateArtifact(ctx, app, trimSpace(image), newImage)
	if err != nil {
		return nil, err
	}
	dto, err := s.deployChangeFor(ctx, a, app, &spec, artifact, store.ReasonDeploy)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// updateArtifact is what an update deploys: a new image, resolved, or the
// app's current release.
func (s *Server) updateArtifact(ctx context.Context, app appScope, image string, given bool) (deployArtifact, error) {
	current, hasImage := app.app.Image.Get()
	if given && (!hasImage || image != current) {
		resolved, err := s.resolve(ctx, app.env, image)
		if err != nil {
			return nil, err
		}
		return resolvedArtifact{image: resolved}, nil
	}
	release, ok := app.app.Release.Get()
	if !ok {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` has no release yet", app.app.Slug)
	}
	return releaseArtifact{id: release}, nil
}

// deployChangeFor records spec with artifact as app's next run for reason
// and returns the app as it is then.
func (s *Server) deployChangeFor(
	ctx context.Context, a access.Access, app appScope, spec *v1alpha1.AppSpec, artifact deployArtifact,
	reason store.RunReason,
) (gen.AppDto, error) {
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := s.deploy(ctx, t, a, change{
		project: app.env.project.project.ID, application: app.app.Application, target: app.app.Target,
		spec: spec, artifact: artifact, expected: app.app.DesiredGeneration, reason: reason,
		reference: app.reference(), chain: app.chain(), environment: app.env.env.ID, quota: app.env.env.Quota,
	}); err != nil {
		return gen.AppDto{}, err
	}
	record, found, err := t.App(ctx, app.env.env.ID, app.app.Slug)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.AppDto{}, kerrors.New(kerrors.NotFound, "app `%s`", app.app.Slug)
	}
	if err := t.Commit(ctx); err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return appDto(app.env.project.project.Slug, app.env.env.Slug, record, app.view), nil
}

// DeleteApp deletes an app and everything it owns. Volumes are kept unless
// `delete_volumes=true`.
func (s *Server) DeleteApp(ctx context.Context, params gen.DeleteAppParams) (gen.DeleteAppRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppWrite, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	marked, err := t.MarkTargetDeleting(ctx, app.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !marked {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is being deleted", params.App)
	}
	_, actor := a.Actor()
	subject := store.TargetSubject(app.env.project.project.ID, app.env.env.ID, app.app.Target, params.DeleteVolumes.Or(false))
	if _, err := t.Request(ctx, store.TargetDelete, subject, actor,
		requestAudit(a, store.TargetDelete, "app", app.reference())); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteAppNoContent{}, nil
}

// RestartApp restarts every process, rolling, without a spec change.
func (s *Server) RestartApp(ctx context.Context, params gen.RestartAppParams) (gen.RestartAppRes, error) {
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
	if app.app.Delivery == store.DeliveryAgent {
		// No App object to annotate: a run of the same release and
		// configuration stamps every pod template instead (M1.9).
		if err := s.startRerun(ctx, a, app, store.ReasonRestart, false); err != nil {
			return nil, err
		}
		return &gen.RestartAppAccepted{}, nil
	}
	c, err := s.cluster()
	if err != nil {
		return nil, err
	}
	now := time.UnixMilli(s.deps.Clock.NowMs()).UTC().Format("2006-01-02T15:04:05Z")
	// An annotation, not the spec: the materializer's drift check ignores
	// it.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{render.RestartedAt: now}}})
	if err != nil {
		return nil, kerrors.Wrap(err, "a restart patch")
	}
	gvr := v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.AppResource)
	if _, err := c.Dynamic.Resource(gvr).Namespace(app.app.Namespace).
		Patch(ctx, app.app.Slug, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, kubeError(err, params.App)
	}
	return &gen.RestartAppAccepted{}, nil
}

// HandOverApp hands an app over from the App controller to its cluster's
// agent: its next run goes through the agent, which adopts the app's
// workloads in place (no new pods), and every later run follows. Only
// toward the agent; the cluster needs a linked agent that carries
// applications.
func (s *Server) HandOverApp(ctx context.Context, params gen.HandOverAppParams) (gen.HandOverAppRes, error) {
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
	if app.app.Delivery == store.DeliveryAgent {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is delivered by its cluster's agent already", params.App)
	}
	if app.app.Deleting {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is being deleted", params.App)
	}
	if err := s.startRerun(ctx, a, app, store.ReasonHandover, true); err != nil {
		return nil, err
	}
	return &gen.HandOverAppAccepted{}, nil
}

// startRerun starts a run of app's current release and configuration for
// reason (a restart or a handover changes neither), handing the target
// over to its agent first when asked.
func (s *Server) startRerun(ctx context.Context, a access.Access, app appScope, reason store.RunReason, handOver bool) error {
	run, err := rerun(app, a, reason)
	if err != nil {
		return err
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := ensureMayDeploy(ctx, t, a, app.app.Target, app.chain()); err != nil {
		return err
	}
	if handOver {
		moved, err := t.HandOverToAgent(ctx, app.app.Target)
		if err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		if !moved {
			return kerrors.New(kerrors.Conflict, "the cluster of app `%s` has no linked agent that carries applications", app.app.Slug)
		}
	}
	started, err := t.StartDeployment(ctx, run, requestAudit(a, "deployment.accepted", "app", app.reference()),
		opt.None[store.IdempotencyKey]())
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := startedErr(started); err != nil {
		return err
	}
	return t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// rerun is a run of app's current release and configuration for reason.
func rerun(app appScope, a access.Access, reason store.RunReason) (store.StartDeployment, error) {
	release, hasRelease := app.app.Release.Get()
	revision, hasRevision := app.app.ConfigRevision.Get()
	if !hasRelease || !hasRevision {
		return store.StartDeployment{}, kerrors.New(kerrors.Conflict, "app `%s` has no release yet", app.app.Slug)
	}
	_, actor := a.Actor()
	expected := app.app.DesiredGeneration
	hash := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s/%d", reason, release, revision, uint64(expected)))
	return store.StartDeployment{
		Project: app.env.project.project.ID, Target: app.app.Target, Release: release, ConfigRevision: revision,
		ExpectedGeneration: expected, LifecycleUID: app.app.LifecycleUID, Reason: reason, RequestedBy: actor,
		InputHash: hash[:],
	}, nil
}

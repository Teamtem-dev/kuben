package api

// Preview environments (M5.1; previews.rs): pull request events, their
// lifecycle and the janitor.
//
// A pull request of a repository the source environment of a project
// builds from gets a preview:
//   - a new environment `pr<n>-<epoch>` with its own namespace;
//   - the source environment's policy without approvals;
//   - a copy of every app that builds from that repository, without secret
//     references or custom domains, bound to the pull request's head.
//
// New commits sync those apps. Closing the pull request deletes the
// environment. A reopened pull request gets a new epoch and so a new
// environment. Events older than what a preview (or its closed
// predecessor) saw are ignored, so a late delivery never brings a preview
// back.
//
// A fork's preview is untrusted: only projects that allow forks get one,
// and it never binds a secret or a registry login.
//
// The janitor deletes previews whose lifetime ran out. It also asks GitHub
// whether the pull request of a preview not confirmed for a while is still
// open, so a missed `closed` delivery is caught. An unclear answer never
// deletes anything.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/preview"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// PreviewsSubsystem is the janitor's health name.
const PreviewsSubsystem = "previews"

const (
	// previewSweep is how often the janitor looks for expired previews.
	previewSweep = time.Minute
	// previewVerifyEveryMs: a preview's pull request is confirmed open at
	// most this often.
	previewVerifyEveryMs int64 = 30 * 60_000
	previewBatch         int64 = 20
	// previewVerifyRounds: pull request states are read every tenth sweep.
	previewVerifyRounds = 10
	// maxNamespaceLen is the longest Kubernetes namespace name.
	maxNamespaceLen = 63
)

// PreviewOutcome is what a pull request event did to one project.
type PreviewOutcome struct {
	Project ids.ProjectID
	// Result names what happened: `opened`, `synced`, `closed`, `stale`,
	// `staleAfterClose`, `limit`, `disabled`, `noPreview`, `forkRefused`,
	// `projectGone`, `nameTooLong`, `sourceGone` or `noApps`.
	Result string
}

// PreviewDeps is what the preview lifecycle works with.
type PreviewDeps struct {
	Store *store.Store
	// GitHub answers whether a pull request is still open; without it the
	// janitor only deletes expired previews.
	GitHub opt.Val[*github.App]
	// OrgEnvironments is the organization's environment quota
	// (quota.org_environments), which a preview counts against.
	OrgEnvironments opt.Val[uint64]
	Clock           clock.Clock
	Logger          *slog.Logger
}

// Previews is the preview lifecycle and janitor of one process; every
// replica runs one janitor.
type Previews struct {
	store           *store.Store
	github          opt.Val[*github.App]
	orgEnvironments opt.Val[uint64]
	clock           clock.Clock
	logger          *slog.Logger
}

// NewPreviews is the preview lifecycle on d; the system clock and the
// default logger when d leaves them out.
func NewPreviews(d PreviewDeps) *Previews {
	p := &Previews{store: d.Store, github: d.GitHub, orgEnvironments: d.OrgEnvironments, clock: d.Clock, logger: d.Logger}
	if p.clock == nil {
		p.clock = clock.System{}
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

// previews is the lifecycle on the server's dependencies.
func (s *Server) previews() *Previews {
	return NewPreviews(PreviewDeps{
		Store: s.deps.Store, GitHub: s.deps.GitHub, OrgEnvironments: s.deps.Config.Quota.OrgEnvironments,
		Clock: s.deps.Clock, Logger: s.deps.Logger,
	})
}

// pullAudit is the audit record of what a pull request event did to
// target.
func pullAudit(event source.PullEvent, action, target string) store.NewAudit {
	return store.NewAudit{
		ActorKind:  "webhook",
		ActorID:    opt.Some(fmt.Sprintf("github:%d", event.InstallationID)),
		Action:     action,
		TargetKind: opt.Some("environment"),
		TargetRef:  opt.Some(target),
		Outcome:    "accepted",
		Data: opt.Some[any](map[string]any{
			"repository":     event.Repository.String(),
			"pullRequest":    event.Number,
			"head":           event.Head.String(),
			"headRepository": event.HeadRepository,
		}),
	}
}

// OnPull applies a pull request event to every project of org it concerns,
// each in a transaction of its own (previews::on_pull).
func (p *Previews) OnPull(ctx context.Context, org ids.OrgID, event source.PullEvent) ([]PreviewOutcome, error) {
	projects, err := p.previewProjects(ctx, org, event)
	if err != nil {
		return nil, err
	}
	out := []PreviewOutcome{}
	for _, project := range projects {
		result, err := p.applyIn(ctx, org, project, event)
		if err != nil {
			return nil, err
		}
		out = append(out, PreviewOutcome{Project: project, Result: result})
	}
	return out, nil
}

func (p *Previews) previewProjects(ctx context.Context, org ids.OrgID, event source.PullEvent) ([]ids.ProjectID, error) {
	t, err := p.store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx)                                                 //nolint:errcheck // read only
	return t.PreviewProjects(ctx, event.InstallationID, event.Repository) //nolint:wrapcheck // a store error, answered as internal
}

// applyIn applies event to project and commits, whatever the result.
func (p *Previews) applyIn(ctx context.Context, org ids.OrgID, project ids.ProjectID, event source.PullEvent) (string, error) {
	t, err := p.store.Tenant(ctx, org)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	result, err := p.apply(ctx, t, project, event)
	if err != nil {
		return "", err
	}
	if err := t.Commit(ctx); err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	return result, nil
}

func handleClosedPreview(ctx context.Context, t *store.Tenant, active store.PreviewRecord, isActive bool, event source.PullEvent) (string, error) {
	if !isActive {
		return "noPreview", nil
	}
	audit := pullAudit(event, "closePreview", active.Environment)
	if _, err := destroyPreview(ctx, t, active, store.CloseClosed, event.UpdatedAt, audit); err != nil {
		return "", err
	}
	return "closed", nil
}

func (p *Previews) updateActivePreview(ctx context.Context, t *store.Tenant, active store.PreviewRecord, settings store.PreviewPolicy, event source.PullEvent, now int64) (string, error) {
	touched, err := t.TouchPreview(ctx, active.EnvironmentID, event.HeadRepository, event.HeadBranch, event.Head,
		event.UpdatedAt, preview.Expiry(now, settings.TTLHours, opt.Some(active.ExpiresAt)))
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if !touched {
		return "stale", nil
	}
	if err := syncPreviewApps(ctx, t, active.EnvironmentID, event); err != nil {
		return "", err
	}
	return "synced", nil
}

func (p *Previews) apply(ctx context.Context, t *store.Tenant, project ids.ProjectID, event source.PullEvent) (string, error) {
	settings, found, err := t.PreviewPolicy(ctx, project)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || !settings.Enabled {
		return "disabled", nil
	}
	active, isActive, err := t.ActivePreview(ctx, project, event.Repository, event.Number)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if event.Action == source.PullClosed || !event.Open {
		return handleClosedPreview(ctx, t, active, isActive, event)
	}
	if event.FromFork() && !settings.AllowForks {
		return "forkRefused", nil
	}
	now := p.clock.NowMs()
	if isActive {
		return p.updateActivePreview(ctx, t, active, settings, event, now)
	}
	epoch, closedAt, err := t.PreviewEpoch(ctx, project, event.Repository, event.Number)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if at, ok := closedAt.Get(); ok && at >= event.UpdatedAt {
		return "staleAfterClose", nil
	}
	count, err := t.ActivePreviewCount(ctx, project)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if count >= uint64(settings.MaxActive) {
		if err := previewLimitIncident(ctx, t, project, event, settings.MaxActive); err != nil {
			return "", err
		}
		return "limit", nil
	}
	return p.create(ctx, t, settings, event, epoch, preview.Expiry(now, settings.TTLHours, opt.None[int64]()))
}

// previewLimitIncident records that a preview could not be made: the
// project has too many.
func previewLimitIncident(ctx context.Context, t *store.Tenant, project ids.ProjectID, event source.PullEvent, maxActive uint32) error {
	_, _, err := t.OpenIncident(ctx, store.NewIncident{
		Project:   opt.Some(project),
		Kind:      "preview.limit",
		Severity:  "warning",
		DedupeKey: "preview-limit:" + project.String(),
		Title: fmt.Sprintf("No preview for %s#%d: the project has %d already",
			event.Repository.String(), event.Number, maxActive),
		Detail: opt.Some("Close or destroy a preview, or raise the project's preview limit."),
	})
	return err //nolint:wrapcheck // a store error, answered as internal
}

// previewTarget is where a new preview goes: its environment name,
// Kubernetes resource name and namespace, and its source environment.
type previewTarget struct {
	slug      string
	resource  string
	namespace string
	source    store.EnvironmentRecord
}

// locate finds where a new preview of event in epoch goes; a result
// instead when it cannot be made.
func locate(ctx context.Context, t *store.Tenant, settings store.PreviewPolicy, event source.PullEvent, epoch uint64) (previewTarget, string, error) {
	projects, err := t.Projects(ctx)
	if err != nil {
		return previewTarget{}, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	i := slices.IndexFunc(projects, func(p store.Project) bool { return p.ID == settings.Project })
	if i < 0 {
		return previewTarget{}, "projectGone", nil
	}
	slug, ok := preview.Slug(event.Number, epoch)
	if !ok {
		return previewTarget{}, "nameTooLong", nil
	}
	resource := EnvironmentResourceName(projects[i].Slug, slug)
	namespace := render.NamespaceName(resource)
	if len(namespace) > maxNamespaceLen {
		return previewTarget{}, "nameTooLong", nil
	}
	environments, err := t.Environments(ctx, settings.Project)
	if err != nil {
		return previewTarget{}, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	j := slices.IndexFunc(environments, func(e store.EnvironmentRecord) bool {
		return e.ID == settings.SourceEnvironment && !e.Deleting
	})
	if j < 0 {
		return previewTarget{}, "sourceGone", nil
	}
	return previewTarget{
		slug: slug, resource: resource, namespace: namespace, source: environments[j],
	}, "", nil
}

func (p *Previews) create(
	ctx context.Context, t *store.Tenant, settings store.PreviewPolicy, event source.PullEvent, epoch uint64, expiresAt int64,
) (string, error) {
	project := settings.Project
	where, result, err := locate(ctx, t, settings, event, epoch)
	if err != nil || result != "" {
		return result, err
	}
	sources, err := t.PreviewSources(ctx, where.source.ID, event.InstallationID, event.Repository)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if len(sources) == 0 {
		return "noApps", nil
	}
	if err := admitEnvironmentUnder(ctx, t, p.orgEnvironments); err != nil {
		return "", err
	}
	revision, found, err := t.EnvironmentPolicy(ctx, where.source.ID)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	sourcePolicy := policy.Open()
	if found {
		sourcePolicy = revision.Policy
	}
	actor := fmt.Sprintf("github:%d", event.InstallationID)
	env, err := t.CreateEnvironmentTyped(ctx, project, where.slug, fmt.Sprintf("PR #%d", event.Number),
		store.Preview, where.source.Quota)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	cluster, err := t.EnsureCluster(ctx, primaryCluster)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	placement, err := t.CreatePlacement(ctx, project, env, cluster, where.namespace)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	_, set, err := t.SetEnvironmentPolicy(ctx, project, env, policy.ForPreview(sourcePolicy), actor)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if !set {
		return "", kerr.New(kerr.Internal, "the new preview environment is missing")
	}
	if err := t.InsertPreview(ctx, store.NewPreview{
		Environment: env, Project: project, InstallationID: event.InstallationID, Repository: event.Repository,
		Number: event.Number, Epoch: epoch, HeadRepository: event.HeadRepository, Branch: event.HeadBranch,
		Commit: event.Head, Trusted: !event.FromFork(), ExpiresAt: expiresAt, EventAt: event.UpdatedAt, CreatedBy: actor,
	}); err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if _, err := t.Request(ctx, store.EnvironmentApply, store.EnvironmentSubject(project, env), actor,
		pullAudit(event, "openPreview", where.resource)); err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := p.copyApps(ctx, t, project, placement, sources, event, actor); err != nil {
		return "", err
	}
	if err := syncPreviewApps(ctx, t, env, event); err != nil {
		return "", err
	}
	return "opened", nil
}

// previewConfig is preview.Config of a stored configuration; one that is
// not an object is copied as it is (Rust's Value::get_mut finds nothing).
func previewConfig(config any) (any, []string) {
	if object, ok := config.(map[string]any); ok && object != nil {
		return preview.Config(object)
	}
	return config, []string{}
}

// copyApps copies the source apps into the preview's placement, each bound
// to the pull request's head.
func (p *Previews) copyApps(
	ctx context.Context, t *store.Tenant, project ids.ProjectID, placement ids.PlacementID,
	sources []store.PreviewSource, event source.PullEvent, actor string,
) error {
	branch, err := source.ParseBranchName(fmt.Sprintf("pull/%d", event.Number))
	if err != nil {
		return kerr.New(kerr.Internal, "preview branch: %v", err)
	}
	for _, app := range sources {
		tgt, err := t.CreateTarget(ctx, project, app.Application, placement)
		if err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		config, removed := previewConfig(app.Config)
		if len(removed) > 0 {
			p.logger.Info("left out of the preview", "app", app.Slug, "removed", removed)
		}
		_, created, err := t.CreateConfigRevision(ctx, project, tgt, config, actor)
		if err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		if !created {
			return kerr.New(kerr.Internal, "the preview app is missing")
		}
		binding := store.NewBinding{
			InstallationID: event.InstallationID, Repository: event.Repository, Branch: branch,
			Recipe: app.Recipe, ImageRepository: app.ImageRepository, PullRequest: opt.Some(event.Number),
		}
		if _, err := t.BindSource(ctx, project, tgt, binding); err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	return nil
}

// syncPreviewApps asks every app of preview env to read the pull request's
// head.
func syncPreviewApps(ctx context.Context, t *store.Tenant, env ids.EnvironmentID, event source.PullEvent) error {
	targets, err := t.PreviewTargets(ctx, env)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, tgt := range targets {
		binding, found, err := t.BindingOfTarget(ctx, tgt)
		if err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		if !found {
			continue
		}
		audit := pullAudit(event, "syncSource", "")
		audit.TargetKind = opt.Some("app")
		audit.TargetRef = opt.Some(tgt.String())
		if _, err := t.RequestSync(ctx, binding, fmt.Sprintf("github:pull:%d", event.Number), audit); err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	return nil
}

// destroyPreview closes p for reason and deletes its environment. False
// when it was closed already.
func destroyPreview(ctx context.Context, t *store.Tenant, p store.PreviewRecord, reason store.CloseReason, eventAt int64, audit store.NewAudit) (bool, error) {
	closed, err := t.ClosePreview(ctx, p.EnvironmentID, reason, eventAt)
	if err != nil || !closed {
		return false, err //nolint:wrapcheck // a store error, answered as internal
	}
	marked, err := t.MarkEnvironmentDeleting(ctx, p.EnvironmentID)
	if err != nil {
		return false, err //nolint:wrapcheck // a store error, answered as internal
	}
	if marked {
		actor := audit.ActorID.Or("system")
		if _, err := t.Request(ctx, store.EnvironmentDelete, store.EnvironmentSubject(p.ProjectID, p.EnvironmentID),
			actor, audit); err != nil {
			return false, err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	return true, nil
}

// systemAudit is the audit record of the janitor acting on p.
func systemAudit(p store.PreviewRecord, action, why string) store.NewAudit {
	return store.NewAudit{
		ActorKind:  "system",
		ActorID:    opt.Some("system:previews"),
		Action:     action,
		TargetKind: opt.Some("environment"),
		TargetRef:  opt.Some(p.Environment),
		Outcome:    "accepted",
		Data:       opt.Some[any](map[string]any{"reason": why, "repository": p.Repository, "pullRequest": p.PRNumber}),
	}
}

// Sweep deletes the previews expired by now; the number deleted.
func (p *Previews) Sweep(ctx context.Context, now int64) (int, error) {
	orgs, err := p.store.PreviewOrgs(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	done := 0
	for _, org := range orgs {
		n, err := p.sweepOrg(ctx, org, now)
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (p *Previews) sweepOrg(ctx context.Context, org ids.OrgID, now int64) (int, error) {
	t, err := p.store.Tenant(ctx, org)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	due, err := t.DuePreviews(ctx, now, previewBatch)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	done := 0
	for _, pv := range due {
		destroyed, err := destroyPreview(ctx, t, pv, store.CloseExpired, 0, systemAudit(pv, "expirePreview", "expired"))
		if err != nil {
			return 0, err
		}
		if destroyed {
			done++
		}
	}
	if err := t.Commit(ctx); err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	return done, nil
}

// pullState is what GitHub said about a preview's pull request.
type pullState string

const (
	// pullOpen: the pull request is open; the preview is confirmed.
	pullOpen pullState = "open"
	// pullClosed: closed, merged or gone; the preview goes.
	pullClosed pullState = "closed"
	// pullUnknown: no clear answer, never a reason to delete; asked again
	// later.
	pullUnknown pullState = "unknown"
)

// pullStateOf reads GitHub's answer to "is the pull request open".
func pullStateOf(open bool, err error) pullState {
	var missing build.NotFound
	switch {
	case err == nil && open:
		return pullOpen
	case err == nil, errors.As(err, &missing):
		return pullClosed
	}
	return pullUnknown
}

// pullOf is the installation, pull request and repository of p; false when
// a stored value is out of range.
func pullOf(p store.PreviewRecord) (uint64, uint64, source.RepoName, bool) {
	repository, err := source.ParseRepoName(p.Repository)
	if p.InstallationID < 0 || p.PRNumber < 0 || err != nil {
		return 0, 0, source.RepoName{}, false
	}
	return uint64(p.InstallationID), uint64(p.PRNumber), repository, true //nolint:gosec // non-negative, checked above
}

// Verify deletes the previews whose pull request GitHub reports closed and
// confirms the open ones; the number deleted. Nothing without the GitHub
// App.
func (p *Previews) Verify(ctx context.Context, now int64) (int, error) {
	app, ok := p.github.Get()
	if !ok {
		return 0, nil
	}
	orgs, err := p.store.PreviewOrgs(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	done := 0
	for _, org := range orgs {
		stale, err := p.unverified(ctx, org, now)
		if err != nil {
			return done, err
		}
		for _, pv := range stale {
			installation, number, repository, ok := pullOf(pv)
			if !ok {
				continue
			}
			open, err := app.PullRequestOpen(ctx, installation, repository, number)
			state := pullStateOf(open, err)
			if state == pullUnknown {
				p.logger.Debug("the pull request state is unknown", "preview", pv.Environment, "error", err)
			}
			destroyed, err := p.settle(ctx, org, pv, now, state)
			if err != nil {
				return done, err
			}
			if destroyed {
				done++
			}
		}
	}
	return done, nil
}

func (p *Previews) unverified(ctx context.Context, org ids.OrgID, now int64) ([]store.PreviewRecord, error) {
	t, err := p.store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx)                                                    //nolint:errcheck // read only
	return t.UnverifiedPreviews(ctx, now-previewVerifyEveryMs, previewBatch) //nolint:wrapcheck // a store error naming its operation
}

// settle records what GitHub said about pv's pull request; true when the
// preview was deleted.
func (p *Previews) settle(ctx context.Context, org ids.OrgID, pv store.PreviewRecord, now int64, state pullState) (bool, error) {
	t, err := p.store.Tenant(ctx, org)
	if err != nil {
		return false, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	destroyed := false
	switch state {
	case pullOpen:
		err = t.PreviewVerified(ctx, pv.EnvironmentID, now)
	case pullClosed:
		destroyed, err = destroyPreview(ctx, t, pv, store.CloseClosed, now,
			systemAudit(pv, "closePreview", "the pull request is closed"))
	case pullUnknown:
		// Unclear: never a reason to delete. Try again later.
		err = t.PreviewVerified(ctx, pv.EnvironmentID, now-previewVerifyEveryMs/2)
	}
	if err != nil {
		return false, err
	}
	if err := t.Commit(ctx); err != nil {
		return false, err //nolint:wrapcheck // a store error naming its operation
	}
	return destroyed, nil
}

// RunPreviewJanitor runs p's janitor until ctx ends: a sweep every minute,
// and the pull request states every tenth round.
func RunPreviewJanitor(ctx context.Context, p *Previews, h *health.Health) error {
	h.OK(PreviewsSubsystem)
	tick := time.NewTicker(previewSweep)
	defer tick.Stop()
	var rounds uint64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		now := p.clock.NowMs()
		expired, err := p.Sweep(ctx, now)
		if err != nil {
			return err
		}
		closed := 0
		if rounds%previewVerifyRounds == 0 {
			if closed, err = p.Verify(ctx, now); err != nil {
				return err
			}
		}
		rounds++
		if expired+closed > 0 {
			p.logger.Info("previews deleted", "expired", expired, "closed", closed)
		}
	}
}

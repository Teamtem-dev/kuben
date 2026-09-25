package api

// An app's Git source (M3): which repository and branch it builds from,
// how, and where the image goes. Binding or changing the source asks for a
// sync, which reads the branch head from GitHub and queues a build of it
// (routes/apps/source.rs).

import (
	"context"
	"strings"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// strategyOf is From<StrategyDto> for BuildStrategy.
func strategyOf(s gen.StrategyDto) source.BuildStrategy {
	switch s {
	case gen.StrategyDtoDockerfile:
		return source.Dockerfile
	case gen.StrategyDtoRailpack:
		return source.Railpack
	case gen.StrategyDtoAuto:
	}
	return source.Auto
}

// strategyDto is From<BuildStrategy> for StrategyDto.
func strategyDto(s source.BuildStrategy) gen.StrategyDto {
	switch s.OrAuto() {
	case source.Dockerfile:
		return gen.StrategyDtoDockerfile
	case source.Railpack:
		return gen.StrategyDtoRailpack
	case source.Auto:
	}
	return gen.StrategyDtoAuto
}

// sourceDto is SourceDto::new: sync is the sync the request queued, if any.
func sourceDto(b store.SourceBinding, sync opt.Val[ids.OperationID]) gen.SourceDto {
	dockerfile := opt.None[string]()
	if d, ok := b.Recipe.Dockerfile.Get(); ok {
		dockerfile = opt.Some(d.String())
	}
	head := opt.None[string]()
	if h, ok := b.HeadSha.Get(); ok {
		head = opt.Some(h.String())
	}
	operation := opt.None[string]()
	if o, ok := sync.Get(); ok {
		operation = opt.Some(o.String())
	}
	return gen.SourceDto{
		InstallationId:  int64(b.InstallationID), //nolint:gosec // stored as BIGINT
		Repository:      b.Repository.String(),
		Branch:          b.Branch.String(),
		Strategy:        strategyDto(b.Recipe.Strategy),
		Context:         b.Recipe.Context.String(),
		Dockerfile:      optNilString(dockerfile),
		ImageRepository: b.ImageRepository,
		Head:            optNilString(head),
		SyncOperation:   optNilString(operation),
	}
}

// invalid is a validation failure saying err.
func invalid(err error) error {
	return kerrors.New(kerrors.Validation, "%s", err.Error())
}

// imageRepository is `registry/path` without tag or digest, normalized like
// Docker does.
func imageRepository(value string) (string, error) {
	last := value[strings.LastIndexByte(value, '/')+1:]
	if strings.Contains(value, "@") || strings.Contains(last, ":") {
		return "", kerrors.New(kerrors.Validation,
			"`%s` must name a repository without tag or digest; builds tag and pin it", value)
	}
	ref, err := oci.Parse(value)
	if err != nil {
		return "", invalid(err)
	}
	return ref.Repository(), nil
}

// newBinding is the binding body asks for, validated and normalized.
func newBinding(body *gen.PutSource) (store.NewBinding, error) {
	repository, err := source.ParseRepoName(body.Repository)
	if err != nil {
		return store.NewBinding{}, invalid(err)
	}
	branch, err := source.ParseBranchName(body.Branch)
	if err != nil {
		return store.NewBinding{}, invalid(err)
	}
	context, err := source.ParseRepoPath(body.Context.Or(""))
	if err != nil {
		return store.NewBinding{}, invalid(err)
	}
	dockerfile := opt.None[source.RepoPath]()
	if d, ok := body.Dockerfile.Get(); ok {
		path, err := source.ParseRepoPath(d)
		if err != nil {
			return store.NewBinding{}, invalid(err)
		}
		if !path.IsRoot() {
			dockerfile = opt.Some(path)
		}
	}
	image, err := imageRepository(body.ImageRepository)
	if err != nil {
		return store.NewBinding{}, err
	}
	return store.NewBinding{
		InstallationID: uint64(body.InstallationId), //nolint:gosec // the contract's minimum is 0
		Repository:     repository,
		Branch:         branch,
		Recipe: source.BuildRecipe{
			Strategy:   strategyOf(body.Strategy.Or(gen.StrategyDtoAuto)),
			Context:    context,
			Dockerfile: dockerfile,
		},
		ImageRepository: image,
	}, nil
}

// sourceAudit is the audit record of a sync a person asked for; its actor
// kind is how the request was authenticated (`session` or `token`).
func sourceAudit(a access.Access, action, reference string, data map[string]any) store.NewAudit {
	return store.NewAudit{
		ActorKind:  string(a.Current.Via),
		ActorID:    opt.Some(a.Current.User.ID.String()),
		Action:     action,
		TargetKind: opt.Some("app"),
		TargetRef:  opt.Some(reference),
		Outcome:    "accepted",
		Data:       opt.Some[any](data),
	}
}

// requestedBy is who asks for a sync: always the person, even through a
// token.
func requestedBy(a access.Access) string {
	return "user:" + a.Current.User.ID.String()
}

// bindAndSync binds the target tgt of project to binding and asks for a
// sync, in t.
func bindAndSync(
	ctx context.Context, t *store.Tenant, a access.Access, project ids.ProjectID, tgt ids.TargetID,
	binding store.NewBinding, reference string,
) (store.SourceBinding, ids.OperationID, error) {
	bound, err := t.BindSource(ctx, project, tgt, binding)
	if err != nil {
		return store.SourceBinding{}, ids.OperationID{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	var id ids.SourceBindingID
	switch b := bound.(type) {
	case store.BoundInstallationMissing:
		return store.SourceBinding{}, ids.OperationID{}, kerrors.New(kerrors.Validation,
			"installation %d is not linked to this organization", binding.InstallationID)
	case store.BoundNotFound:
		return store.SourceBinding{}, ids.OperationID{}, kerrors.New(kerrors.NotFound, "the app")
	case store.BoundCreated:
		id = b.ID
	case store.BoundChanged:
		id = b.ID
	case store.BoundUnchanged:
		id = b.ID
	}
	_, unchanged := bound.(store.BoundUnchanged)
	found, ok, err := t.Binding(ctx, id)
	if err != nil {
		return store.SourceBinding{}, ids.OperationID{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !ok {
		return store.SourceBinding{}, ids.OperationID{}, kerrors.New(kerrors.Internal, "the bound source is missing")
	}
	data := map[string]any{
		"repository": binding.Repository.String(),
		"branch":     binding.Branch.String(),
		"changed":    !unchanged,
	}
	sync, err := t.RequestSync(ctx, found, requestedBy(a), sourceAudit(a, "syncSource", reference, data))
	if err != nil {
		return store.SourceBinding{}, ids.OperationID{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return found, sync, nil
}

// createGitApp creates an app that builds from Git: its target and
// configuration now, its first release when the first build of the branch
// head is verified.
func (s *Server) createGitApp(
	ctx context.Context, a access.Access, e envScope, name string, spec v1alpha1.AppSpec, git *gen.PutSource,
) (gen.AppDto, error) {
	if _, err := s.github(); err != nil {
		return gen.AppDto{}, err
	}
	if err := validateSpec(&spec); err != nil {
		return gen.AppDto{}, err
	}
	binding, err := newBinding(git)
	if err != nil {
		return gen.AppDto{}, err
	}
	if e.deleting() {
		return gen.AppDto{}, kerrors.New(kerrors.Conflict, "environment `%s` is being deleted", e.env.Slug)
	}
	if err := s.ensureDomainsFree(ctx, e.project.org, e.namespace(), name, &spec); err != nil {
		return gen.AppDto{}, err
	}
	placement, ok := e.env.Placement.Get()
	if !ok {
		return gen.AppDto{}, kerrors.New(kerrors.Conflict, "environment `%s` has no placement", e.env.Slug)
	}
	project := e.project.project.ID
	what := "app `" + name + "`"
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	application, found, err := t.Application(ctx, project, name)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		if application, err = t.CreateApplication(ctx, project, name, name); err != nil {
			return gen.AppDto{}, duplicate(err, what)
		}
	}
	tgt, err := t.CreateTarget(ctx, project, application, placement)
	if err != nil {
		return gen.AppDto{}, duplicate(err, what)
	}
	config, err := configOf(&spec)
	if err != nil {
		return gen.AppDto{}, err
	}
	if _, made, err := t.CreateConfigRevision(ctx, project, tgt, config, actor); err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	} else if !made {
		return gen.AppDto{}, kerrors.New(kerrors.Internal, "the new app is missing")
	}
	reference := e.project.project.Slug + "/" + e.env.Slug + "/" + name
	if _, _, err := bindAndSync(ctx, t, a, project, tgt, binding, reference); err != nil {
		return gen.AppDto{}, err
	}
	record, found, err := t.App(ctx, e.env.ID, name)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.AppDto{}, kerrors.New(kerrors.Internal, "the new app is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return appDto(e.project.project.Slug, e.env.Slug, record, opt.None[projection.AppView]()), nil
}

// noGitSource is the 404 of an app without a Git source.
func noGitSource(app string) error {
	return kerrors.New(kerrors.NotFound, "a Git source of app `%s`", app)
}

// GetAppSource is the app's Git source.
func (s *Server) GetAppSource(ctx context.Context, params gen.GetAppSourceParams) (gen.GetAppSourceRes, error) {
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
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	binding, found, err := t.BindingOfTarget(ctx, app.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, noGitSource(params.App)
	}
	dto := sourceDto(binding, opt.None[ids.OperationID]())
	return &dto, nil
}

// PutAppSource builds the app from a Git repository and branch, or changes
// how. A change stops earlier builds from deploying themselves and builds
// the branch head again.
func (s *Server) PutAppSource(ctx context.Context, req *gen.PutSource, params gen.PutAppSourceParams) (gen.PutAppSourceRes, error) {
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
	if _, err := s.github(); err != nil {
		return nil, err
	}
	binding, err := newBinding(req)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	existing, found, err := t.BindingOfTarget(ctx, app.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if found && existing.PullRequest.IsSome() {
		return nil, kerrors.New(kerrors.Conflict,
			"a preview's app follows its pull request; change the source environment's app instead")
	}
	reference := params.Project + "/" + params.Environment + "/" + params.App
	bound, sync, err := bindAndSync(ctx, t, a, app.env.project.project.ID, app.app.Target, binding, reference)
	if err != nil {
		return nil, err
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := sourceDto(bound, opt.Some(sync))
	return &dto, nil
}

// SyncAppSource reads the branch head from GitHub now and builds it if it
// is new.
func (s *Server) SyncAppSource(ctx context.Context, params gen.SyncAppSourceParams) (gen.SyncAppSourceRes, error) {
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
		return nil, noGitSource(params.App)
	}
	reference := params.Project + "/" + params.Environment + "/" + params.App
	data := map[string]any{"repository": binding.Repository.String(), "branch": binding.Branch.String()}
	operation, err := t.RequestSync(ctx, binding, requestedBy(a), sourceAudit(a, "syncSource", reference, data))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := sourceDto(binding, opt.Some(operation))
	return &dto, nil
}

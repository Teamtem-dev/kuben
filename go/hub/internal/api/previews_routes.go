package api

// A project's preview settings and previews (M5.1; routes/previews.rs).

import (
	"context"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/preview"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

const (
	// defaultPreviewTTL is a preview's lifetime in hours when the settings
	// leave it out.
	defaultPreviewTTL uint32 = 72
	// defaultPreviewMax is how many previews a project may have when the
	// settings leave it out.
	defaultPreviewMax uint32 = 10
	maxPreviewsLimit  uint32 = 100
)

// nullString is a string member that is null.
func nullString() gen.OptNilString {
	var n gen.OptNilString
	n.SetToNull()
	return n
}

// previewPolicyDto is a project's stored preview settings; source names
// its source environment, when it is still live.
func previewPolicyDto(p store.PreviewPolicy, source opt.Val[string]) *gen.PreviewPolicyDto {
	return &gen.PreviewPolicyDto{
		Enabled:           p.Enabled,
		SourceEnvironment: optNilString(source),
		TtlHours:          int32(p.TTLHours),  //nolint:gosec // read from an INTEGER column
		MaxActive:         int32(p.MaxActive), //nolint:gosec // read from an INTEGER column
		AllowForks:        p.AllowForks,
		UpdatedBy:         gen.NewOptNilString(p.UpdatedBy),
		UpdatedAt:         gen.NewOptNilString(Timestamp(p.UpdatedAt)),
	}
}

// unsetPreviewPolicy is the settings of a project that has none.
func unsetPreviewPolicy() *gen.PreviewPolicyDto {
	return &gen.PreviewPolicyDto{
		Enabled:           false,
		SourceEnvironment: nullString(),
		TtlHours:          int32(defaultPreviewTTL),
		MaxActive:         int32(defaultPreviewMax),
		AllowForks:        false,
		UpdatedBy:         nullString(),
		UpdatedAt:         nullString(),
	}
}

// previewDto is PreviewDto::of: a preview as the API shows it at now.
func previewDto(p store.PreviewRecord, now int64) gen.PreviewDto {
	remaining := int64(0)
	if p.Active() {
		remaining = max(p.ExpiresAt-now, 0) / 1000
	}
	closedAt := nullString()
	if at, ok := p.ClosedAt.Get(); ok {
		closedAt = gen.NewOptNilString(Timestamp(at))
	}
	return gen.PreviewDto{
		Environment:      p.Environment,
		Repository:       p.Repository,
		PullRequest:      p.PRNumber,
		Epoch:            p.PreviewEpoch,
		HeadRepository:   p.HeadRepository,
		Branch:           p.Branch,
		Commit:           p.CommitSha,
		Trusted:          p.Trusted,
		State:            p.State,
		AutoDelete:       p.AutoDelete,
		ExpiresAt:        Timestamp(p.ExpiresAt),
		RemainingSeconds: remaining,
		CreatedAt:        Timestamp(p.CreatedAt),
		ClosedAt:         closedAt,
		CloseReason:      optNilString(p.CloseReason),
	}
}

// GetPreviewPolicy is a project's preview settings.
func (s *Server) GetPreviewPolicy(ctx context.Context, params gen.GetPreviewPolicyParams) (gen.GetPreviewPolicyRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	settings, found, err := t.PreviewPolicy(ctx, p.project.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	environments, err := t.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return unsetPreviewPolicy(), nil
	}
	name := opt.None[string]()
	for _, e := range environments {
		if e.ID == settings.SourceEnvironment {
			name = opt.Some(e.Slug)
			break
		}
	}
	return previewPolicyDto(settings, name), nil
}

// checkPreviewPolicy is the lifetime and the limit req asks for, checked.
func checkPreviewPolicy(req *gen.PutPreviewPolicy) (uint32, uint32, error) {
	// The contract types both as int32 with minimum 0: the decoder refused
	// negative values.
	ttl := defaultPreviewTTL
	if v, ok := req.TtlHours.Get(); ok {
		ttl = uint32(v) //nolint:gosec // non-negative: the decoder enforces minimum 0
	}
	maxActive := defaultPreviewMax
	if v, ok := req.MaxActive.Get(); ok {
		maxActive = uint32(v) //nolint:gosec // non-negative: the decoder enforces minimum 0
	}
	if ttl == 0 || ttl > preview.MaxTTLHours {
		return 0, 0, kerr.New(kerr.Validation, "ttlHours must be 1 to %d", preview.MaxTTLHours)
	}
	if maxActive < 1 || maxActive > maxPreviewsLimit {
		return 0, 0, kerr.New(kerr.Validation, "maxActive must be 1 to 100")
	}
	return ttl, maxActive, nil
}

// PutPreviewPolicy sets a project's preview settings.
func (s *Server) PutPreviewPolicy(ctx context.Context, req *gen.PutPreviewPolicy, params gen.PutPreviewPolicyParams) (gen.PutPreviewPolicyRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectWrite, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	ttl, maxActive, err := checkPreviewPolicy(req)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	src, found, err := t.Environment(ctx, p.project.ID, req.SourceEnvironment)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || src.Deleting {
		return nil, kerr.New(kerr.Validation, "no environment `%s`", req.SourceEnvironment)
	}
	if src.EnvType == "preview" {
		return nil, kerr.New(kerr.Validation, "a preview cannot be the source of previews")
	}
	_, actor := acc.Actor()
	settings := store.PreviewPolicy{
		Project: p.project.ID, Enabled: req.Enabled, SourceEnvironment: src.ID, TTLHours: ttl, MaxActive: maxActive,
		AllowForks: req.AllowForks.Or(false), UpdatedBy: actor, UpdatedAt: s.deps.Clock.NowMs(),
	}
	if err := t.SetPreviewPolicy(ctx, settings); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.AppendAudit(ctx, requestAudit(acc, "previews.policy.updated", "project", params.Project)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return previewPolicyDto(settings, opt.Some(src.Slug)), nil
}

// ListPreviews is a project's previews, newest first; closed ones too with
// `all`.
func (s *Server) ListPreviews(ctx context.Context, params gen.ListPreviewsParams) (gen.ListPreviewsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.EnvRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	now := s.deps.Clock.NowMs()
	found, err := t.Previews(ctx, p.project.ID, params.All.Or(false))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListPreviewsOKApplicationJSON, 0, len(found))
	for _, pv := range found {
		out = append(out, previewDto(pv, now))
	}
	return &out, nil
}

// activePreview is the active preview environment of project, which the
// caller may change; 404 when it is none.
func (s *Server) activePreview(ctx context.Context, acc access.Access, project, environment string) (envScope, store.PreviewRecord, error) {
	e, err := s.findEnvironment(ctx, acc, project, environment)
	if err != nil {
		return envScope{}, store.PreviewRecord{}, err
	}
	if _, err := acc.Require(perm.EnvWrite, e.chain()); err != nil {
		return envScope{}, store.PreviewRecord{}, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return envScope{}, store.PreviewRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	pv, found, err := t.PreviewOf(ctx, e.env.ID)
	if err != nil {
		return envScope{}, store.PreviewRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || !pv.Active() {
		return envScope{}, store.PreviewRecord{}, kerr.New(kerr.NotFound, "active preview `%s`", environment)
	}
	return e, pv, nil
}

// ExtendPreview gives a preview more time.
func (s *Server) ExtendPreview(ctx context.Context, req *gen.ExtendPreview, params gen.ExtendPreviewParams) (gen.ExtendPreviewRes, error) {
	// int32 with minimum 0 in the contract: the decoder refused negatives.
	hours := uint32(req.Hours) //nolint:gosec // non-negative: the decoder enforces minimum 0
	if hours == 0 || hours > preview.MaxTTLHours {
		return nil, kerr.New(kerr.Validation, "hours must be 1 to %d", preview.MaxTTLHours)
	}
	keep := req.Keep.Or(false)
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, current, err := s.activePreview(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	now := s.deps.Clock.NowMs()
	expiresAt := preview.Extend(now, current.ExpiresAt, hours)
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if _, err := t.ExtendPreview(ctx, e.env.ID, expiresAt, !keep); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	audit := requestAudit(acc, "preview.extended", "environment", e.resourceName())
	audit.Data = opt.Some[any](map[string]any{"hours": hours, "keep": keep})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	updated, found, err := t.PreviewOf(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerr.New(kerr.Internal, "the preview is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := previewDto(updated, now)
	return &dto, nil
}

// DestroyPreview destroys a preview now: its environment is deleted.
func (s *Server) DestroyPreview(ctx context.Context, params gen.DestroyPreviewParams) (gen.DestroyPreviewRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, current, err := s.activePreview(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	audit := requestAudit(acc, "preview.destroyed", "environment", e.resourceName())
	if _, err := destroyPreview(ctx, t, current, store.CloseManual, s.deps.Clock.NowMs(), audit); err != nil {
		return nil, err
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DestroyPreviewAccepted{}, nil
}

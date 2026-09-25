package api

// Public status pages (M5.3, routes/status.rs).
//
// A project can publish a page anyone may read without signing in. The page
// shows:
// - each app of the chosen environments, by its display name, with its
//   state (`operational`, `degraded` or `majorOutage`);
// - the incidents of those apps, open ones and those resolved in the last
//   week, with their severity and times.
//
// It shows nothing else: no namespaces, pods, addresses, images, incident
// texts or error codes. Answers are cached for a few seconds.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/status"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const weekMs int64 = 7 * 24 * 3_600_000

func statusPageDto(page store.StatusPage, envNames []string) *gen.StatusPageDto {
	return &gen.StatusPageDto{
		Slug:         page.Slug,
		Title:        page.Title,
		Enabled:      page.Enabled,
		Environments: envNames,
		Path:         fmt.Sprintf("/status/%s", page.Slug),
		UpdatedBy:    page.UpdatedBy,
		UpdatedAt:    Timestamp(page.UpdatedAt),
	}
}

// publicFacts reads the apps of page's environments, sorted by display
// name (stable, as Rust's sort_by), and their incidents open now or
// resolved since the week before now. The transaction ends before the
// page is built, as Rust dropped its tenant.
func (s *Server) publicFacts(
	ctx context.Context, page store.StatusPage, now int64,
) ([]store.AppRecord, []store.PublicIncident, error) {
	tenant, err := s.deps.Store.Tenant(ctx, page.Org)
	if err != nil {
		return nil, nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // read only

	var apps []store.AppRecord
	for _, env := range page.Environments {
		records, err := tenant.Apps(ctx, env)
		if err != nil {
			return nil, nil, err
		}
		apps = append(apps, records...)
	}
	slices.SortStableFunc(apps, func(a, b store.AppRecord) int {
		return strings.Compare(a.Name, b.Name)
	})

	targets := make([]uuid.UUID, 0, len(apps))
	for _, a := range apps {
		targets = append(targets, a.Target.UUID())
	}
	incidents, err := tenant.PublicIncidents(ctx, targets, now-weekMs)
	if err != nil {
		return nil, nil, err
	}
	return apps, incidents, nil
}

// saturatingU32 is Rust's u32::try_from(n).unwrap_or(u32::MAX) for a count.
func saturatingU32(n int) uint32 {
	return uint32(min(max(n, 0), math.MaxUint32))
}

// componentStatus is the state an app shows on a public page.
func (s *Server) componentStatus(a store.AppRecord, incidents []store.PublicIncident) status.Service {
	pods := s.deps.Projections.PodsOfApp(a.Namespace, a.Slug)
	var ready opt.Val[bool]
	switch a.Delivery {
	case store.DeliveryAgent:
		if r, ok := a.Runtime.Get(); ok {
			ready = opt.Some(r.Ready())
		}
	case store.DeliveryController:
		if v, ok := s.deps.Projections.App(a.Namespace, a.Slug); ok {
			ready = opt.Some(v.Ready)
		}
	}
	readyPods := 0
	for _, p := range pods {
		if p.Ready {
			readyPods++
		}
	}
	open := func(severity string) bool {
		return slices.ContainsFunc(incidents, func(i store.PublicIncident) bool {
			tid, ok := i.TargetID.Get()
			return i.ResolvedAt.IsNone() && i.Severity == severity && ok && tid == a.Target.UUID()
		})
	}
	return status.Component(status.Observed{
		Ready:            ready,
		Pods:             saturatingU32(len(pods)),
		ReadyPods:        saturatingU32(readyPods),
		CriticalIncident: open("critical"),
		WarningIncident:  open("warning"),
	})
}

// publicIncidents are the incidents of the page's apps, by display name.
func publicIncidents(apps []store.AppRecord, incidents []store.PublicIncident) []gen.PublicIncidentDto {
	names := make(map[uuid.UUID]string, len(apps))
	for _, a := range apps {
		names[a.Target.UUID()] = a.Name
	}
	dtos := make([]gen.PublicIncidentDto, 0, len(incidents))
	for _, i := range incidents {
		tid, ok := i.TargetID.Get()
		if !ok {
			continue
		}
		name, ok := names[tid]
		if !ok {
			continue
		}
		var resolvedAt opt.Val[string]
		if at, ok := i.ResolvedAt.Get(); ok {
			resolvedAt = opt.Some(Timestamp(at))
		}
		dtos = append(dtos, gen.PublicIncidentDto{
			Component:  name,
			Severity:   i.Severity,
			StartedAt:  Timestamp(i.OpenedAt),
			ResolvedAt: optNilString(resolvedAt),
		})
	}
	return dtos
}

func (s *Server) buildPublicStatus(ctx context.Context, page store.StatusPage) (*gen.PublicStatus, error) {
	now := s.deps.Clock.NowMs()
	apps, incidents, err := s.publicFacts(ctx, page, now)
	if err != nil {
		return nil, err
	}
	components := make([]gen.PublicComponent, 0, len(apps))
	statuses := make([]status.Service, 0, len(apps))
	for _, a := range apps {
		st := s.componentStatus(a, incidents)
		statuses = append(statuses, st)
		components = append(components, gen.PublicComponent{Name: a.Name, Status: string(st)})
	}
	return &gen.PublicStatus{
		Title:      page.Title,
		Status:     string(status.Page(statuses)),
		Components: components,
		Incidents:  publicIncidents(apps, incidents),
		UpdatedAt:  Timestamp(now),
	}, nil
}

// GetPublicStatus serves a project's public status page. No sign-in; nothing internal.
func (s *Server) GetPublicStatus(ctx context.Context, params gen.GetPublicStatusParams) (gen.GetPublicStatusRes, error) {
	if cached, ok := s.statusCache.Get(params.Slug); ok {
		httpx.SetHeader(ctx, "Cache-Control", fmt.Sprintf("public, max-age=%d", int(statusCacheTTL.Seconds())))
		return cached, nil
	}

	page, found, err := s.deps.Store.PublicStatusPage(ctx, params.Slug)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "status page `%s`", params.Slug)
	}

	built, err := s.buildPublicStatus(ctx, page)
	if err != nil {
		return nil, err
	}
	s.statusCache.Add(params.Slug, built)
	httpx.SetHeader(ctx, "Cache-Control", fmt.Sprintf("public, max-age=%d", int(statusCacheTTL.Seconds())))
	return built, nil
}

// GetStatusPage serves a project's status page settings.
func (s *Server) GetStatusPage(ctx context.Context, params gen.GetStatusPageParams) (gen.GetStatusPageRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectRead, p.chain()); err != nil {
		return nil, err
	}

	tenant, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // read only

	page, found, err := tenant.StatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "the status page of `%s`", params.Project)
	}

	environments, err := tenant.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}

	envNames := make([]string, 0, len(page.Environments))
	for _, id := range page.Environments {
		for _, e := range environments {
			if e.ID == id {
				envNames = append(envNames, e.Slug)
				break
			}
		}
	}
	return statusPageDto(page, envNames), nil
}

// validatePutStatusPage checks a page's slug, title and number of
// environments, and returns the trimmed title.
func validatePutStatusPage(req *gen.PutStatusPage) (string, error) {
	if err := DNSLabel("slug", req.Slug, 63); err != nil {
		return "", err
	}
	title := strings.TrimSpace(req.Title)
	titleRunes := utf8.RuneCountInString(title)
	if titleRunes == 0 || titleRunes > 100 || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return "", kerrors.New(kerrors.Validation, "title must be 1 to 100 printable characters")
	}
	if len(req.Environments) == 0 || len(req.Environments) > 20 {
		return "", kerrors.New(kerrors.Validation, "list 1 to 20 environments")
	}
	return title, nil
}

// PutStatusPage publishes or changes a project's status page.
func (s *Server) PutStatusPage(ctx context.Context, req *gen.PutStatusPage, params gen.PutStatusPageParams) (gen.PutStatusPageRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectWrite, p.chain()); err != nil {
		return nil, err
	}

	title, err := validatePutStatusPage(req)
	if err != nil {
		return nil, err
	}

	tenant, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // committed on success

	known, err := tenant.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}

	var envIDs []ids.EnvironmentID
	for _, name := range req.Environments {
		i := slices.IndexFunc(known, func(e store.EnvironmentRecord) bool {
			return e.Slug == name && !e.Deleting
		})
		if i < 0 {
			return nil, kerrors.New(kerrors.Validation, "no environment `%s`", name)
		}
		envIDs = append(envIDs, known[i].ID)
	}

	_, actor := acc.Actor()
	now := s.deps.Clock.NowMs()
	page := store.StatusPage{
		Project:      p.project.ID,
		Org:          p.org,
		Slug:         req.Slug,
		Title:        title,
		Enabled:      req.Enabled.Or(true),
		Environments: envIDs,
		UpdatedBy:    actor,
		UpdatedAt:    now,
	}

	if err := tenant.SetStatusPage(ctx, page); err != nil {
		return nil, duplicate(err, fmt.Sprintf("status page `%s`", req.Slug))
	}
	if err := tenant.AppendAudit(ctx, requestAudit(acc, "status-page.updated", "project", params.Project)); err != nil {
		return nil, err
	}
	if err := tenant.Commit(ctx); err != nil {
		return nil, err
	}

	s.statusCache.Remove(req.Slug)
	return statusPageDto(page, req.Environments), nil
}

// DeleteStatusPage takes a project's status page down.
func (s *Server) DeleteStatusPage(ctx context.Context, params gen.DeleteStatusPageParams) (gen.DeleteStatusPageRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectWrite, p.chain()); err != nil {
		return nil, err
	}

	tenant, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // committed on success

	existing, hasPage, err := tenant.StatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	deleted, err := tenant.DeleteStatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	if !deleted {
		return nil, kerrors.New(kerrors.NotFound, "the status page of `%s`", params.Project)
	}
	if err := tenant.AppendAudit(ctx, requestAudit(acc, "status-page.deleted", "project", params.Project)); err != nil {
		return nil, err
	}
	if err := tenant.Commit(ctx); err != nil {
		return nil, err
	}

	if hasPage {
		s.statusCache.Remove(existing.Slug)
	}
	return &gen.DeleteStatusPageNoContent{}, nil
}

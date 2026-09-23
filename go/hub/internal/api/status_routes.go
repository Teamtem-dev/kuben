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

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/status"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
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

func (s *Server) buildPublicStatus(ctx context.Context, page store.StatusPage) (*gen.PublicStatus, error) {
	tenant, err := s.deps.Store.Tenant(ctx, page.Org)
	if err != nil {
		return nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck

	var apps []store.AppRecord
	for _, env := range page.Environments {
		records, err := tenant.Apps(ctx, env)
		if err != nil {
			return nil, err
		}
		apps = append(apps, records...)
	}
	slices.SortFunc(apps, func(a, b store.AppRecord) int {
		return strings.Compare(a.Name, b.Name)
	})

	now := s.deps.Clock.NowMs()
	targets := make([]uuid.UUID, 0, len(apps))
	for _, a := range apps {
		targets = append(targets, a.Target.UUID())
	}
	incidents, err := tenant.PublicIncidents(ctx, targets, now-weekMs)
	if err != nil {
		return nil, err
	}
	_ = tenant.Rollback(ctx)

	names := make(map[uuid.UUID]string, len(apps))
	for _, a := range apps {
		names[a.Target.UUID()] = a.Name
	}

	components := make([]gen.PublicComponent, 0, len(apps))
	componentStatuses := make([]status.Service, 0, len(apps))
	for _, a := range apps {
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
			for _, i := range incidents {
				if i.ResolvedAt.IsNone() && i.Severity == severity {
					if tid, ok := i.TargetID.Get(); ok && tid == a.Target.UUID() {
						return true
					}
				}
			}
			return false
		}

		var podCount uint32 = math.MaxUint32
		if len(pods) <= math.MaxUint32 {
			podCount = uint32(len(pods))
		}
		var readyPodCount uint32 = math.MaxUint32
		if readyPods <= math.MaxUint32 {
			readyPodCount = uint32(readyPods)
		}

		observed := status.Observed{
			Ready:            ready,
			Pods:             podCount,
			ReadyPods:        readyPodCount,
			CriticalIncident: open("critical"),
			WarningIncident:  open("warning"),
		}
		cStatus := status.Component(observed)
		componentStatuses = append(componentStatuses, cStatus)
		components = append(components, gen.PublicComponent{
			Name:   a.Name,
			Status: string(cStatus),
		})
	}

	incidentDTOs := make([]gen.PublicIncidentDto, 0)
	for _, i := range incidents {
		tid, ok := i.TargetID.Get()
		if !ok {
			continue
		}
		name, ok := names[tid]
		if !ok {
			continue
		}
		var resAt opt.Val[string]
		if rAt, ok := i.ResolvedAt.Get(); ok {
			resAt = opt.Some(Timestamp(rAt))
		}
		incidentDTOs = append(incidentDTOs, gen.PublicIncidentDto{
			Component:  name,
			Severity:   i.Severity,
			StartedAt:  Timestamp(i.OpenedAt),
			ResolvedAt: optNilString(resAt),
		})
	}

	return &gen.PublicStatus{
		Title:      page.Title,
		Status:     string(status.Page(componentStatuses)),
		Components: components,
		Incidents:  incidentDTOs,
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
		return nil, kerr.New(kerr.NotFound, "status page `%s`", params.Slug)
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
	defer tenant.Rollback(ctx) //nolint:errcheck

	page, found, err := tenant.StatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerr.New(kerr.NotFound, "the status page of `%s`", params.Project)
	}

	environments, err := tenant.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	_ = tenant.Rollback(ctx)

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

	if err := DNSLabel("slug", req.Slug, 63); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	titleRunes := utf8.RuneCountInString(title)
	if titleRunes == 0 || titleRunes > 100 || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return nil, kerr.New(kerr.Validation, "title must be 1 to 100 printable characters")
	}
	if len(req.Environments) == 0 || len(req.Environments) > 20 {
		return nil, kerr.New(kerr.Validation, "list 1 to 20 environments")
	}

	tenant, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err
	}
	defer tenant.Rollback(ctx) //nolint:errcheck

	known, err := tenant.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}

	var envIDs []ids.EnvironmentID
	for _, name := range req.Environments {
		var found *store.EnvironmentRecord
		for _, e := range known {
			if e.Slug == name && !e.Deleting {
				found = &e
				break
			}
		}
		if found == nil {
			return nil, kerr.New(kerr.Validation, "no environment `%s`", name)
		}
		envIDs = append(envIDs, found.ID)
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
	defer tenant.Rollback(ctx) //nolint:errcheck

	existing, hasPage, err := tenant.StatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	deleted, err := tenant.DeleteStatusPage(ctx, p.project.ID)
	if err != nil {
		return nil, err
	}
	if !deleted {
		return nil, kerr.New(kerr.NotFound, "the status page of `%s`", params.Project)
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

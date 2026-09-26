package httpapi

// Incidents and webhook endpoints of an organization (M4.10;
// routes/incidents.rs).

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/notify"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// webhookEvents are the events an endpoint can subscribe to (`*` for all).
func webhookEvents() []string {
	return []string{
		"deployment.started", "deployment.succeeded", "deployment.failed", "deployment.cancelled",
		"build.started", "build.succeeded", "build.failed", "build.cancelled",
		"incident.opened", "incident.resolved", "ping",
	}
}

func optNilUUID(v opt.Val[uuid.UUID]) gen.OptNilUUID {
	if id, ok := v.Get(); ok {
		return gen.NewOptNilUUID(id)
	}
	var out gen.OptNilUUID
	out.SetToNull()
	return out
}

func optNilInt32(v opt.Val[int32]) gen.OptNilInt32 {
	if n, ok := v.Get(); ok {
		return gen.NewOptNilInt32(n)
	}
	var out gen.OptNilInt32
	out.SetToNull()
	return out
}

func incidentDto(i store.Incident) gen.IncidentDto {
	runbook := opt.None[string]()
	if r, ok := notify.Runbook(i.Kind); ok {
		runbook = opt.Some(r)
	}
	return gen.IncidentDto{
		ID:             i.ID,
		Kind:           i.Kind,
		Severity:       i.Severity,
		Title:          i.Title,
		Detail:         optNilString(i.Detail),
		Project:        optNilUUID(i.ProjectID),
		Environment:    optNilUUID(i.EnvironmentID),
		App:            optNilUUID(i.TargetID),
		OpenedAt:       Timestamp(i.OpenedAt),
		LastSeenAt:     Timestamp(i.LastSeenAt),
		Occurrences:    i.Occurrences,
		AcknowledgedAt: liftedAt(i.AcknowledgedAt),
		AcknowledgedBy: optNilString(i.AcknowledgedBy),
		ResolvedAt:     liftedAt(i.ResolvedAt),
		ResolvedBy:     optNilString(i.ResolvedBy),
		Runbook:        optNilString(runbook),
	}
}

// endpointDto is an endpoint without its secret (the member is left out).
func endpointDto(e store.Endpoint) gen.EndpointDto {
	return gen.EndpointDto{
		ID:         e.ID,
		Name:       e.Name,
		URL:        e.URL,
		Events:     e.Events,
		CreatedBy:  e.CreatedBy,
		CreatedAt:  Timestamp(e.CreatedAt),
		DisabledAt: liftedAt(e.DisabledAt),
		Failures:   e.Failures,
	}
}

func deliveryDto(d store.DeliveryRecord) gen.DeliveryDto {
	return gen.DeliveryDto{
		ID:         d.ID,
		Event:      d.Event,
		Status:     d.Status,
		Attempts:   d.Attempts,
		LastStatus: optNilInt32(d.LastStatus),
		LastError:  optNilString(d.LastError),
		CreatedAt:  Timestamp(d.CreatedAt),
		FinishedAt: liftedAt(d.FinishedAt),
	}
}

// checkEndpoint is the events body subscribes to, trimmed, sorted and
// without repeats, when its name, URL and events are acceptable
// (check_endpoint).
func checkEndpoint(body *gen.CreateEndpoint) ([]string, error) {
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 || strings.ContainsFunc(name, unicode.IsControl) {
		return nil, kerrors.New(kerrors.Validation, "name must be 1 to 64 printable characters")
	}
	if len(body.URL) > 2048 {
		return nil, kerrors.New(kerrors.Validation, "the URL is too long")
	}
	events := make([]string, 0, len(body.Events))
	for _, e := range body.Events {
		events = append(events, strings.TrimSpace(e))
	}
	slices.Sort(events)
	events = slices.Compact(events)
	if len(events) == 0 || len(events) > 32 {
		return nil, kerrors.New(kerrors.Validation, "subscribe to 1 to 32 events")
	}
	known := webhookEvents()
	for _, e := range events {
		if e != "*" && !slices.Contains(known, e) {
			return nil, kerrors.New(kerrors.Validation, "unknown event `%s`", e)
		}
	}
	return events, nil
}

// orgTenant is the caller's organization with p checked, and a
// transaction of it.
func (s *Server) orgTenant(ctx context.Context, a access.Access, p perm.Perm) (*store.Tenant, error) {
	org, err := orgOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(p, authz.OrgChain(org)); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return t, nil
}

// ListIncidents is the organization's incidents, newest first.
func (s *Server) ListIncidents(ctx context.Context, params gen.ListIncidentsParams) ([]gen.IncidentDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTenant(ctx, a, perm.OrgRead)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.Incidents(ctx, params.All.Or(false), min(max(params.Limit.Or(100), 1), 500))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.IncidentDto, 0, len(found))
	for _, i := range found {
		out = append(out, incidentDto(i))
	}
	return out, nil
}

// touchIncident acknowledges or resolves incident id.
func (s *Server) touchIncident(ctx context.Context, id uuid.UUID, resolve bool) error {
	a, err := s.access(ctx)
	if err != nil {
		return err
	}
	t, err := s.orgTenant(ctx, a, perm.AppDeploy)
	if err != nil {
		return err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	_, actor := a.Actor()
	touch, action, state := t.AcknowledgeIncident, "incident.acknowledged", " and unacknowledged"
	if resolve {
		touch, action, state = t.ResolveIncident, "incident.resolved", ""
	}
	done, err := touch(ctx, id, actor)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if !done {
		return kerrors.New(kerrors.Conflict, "incident `%s` is not open%s", id, state)
	}
	if err := t.AppendAudit(ctx, requestAudit(a, action, "incident", id.String())); err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	return t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// AcknowledgeIncident acknowledges an open incident: someone is on it.
func (s *Server) AcknowledgeIncident(ctx context.Context, params gen.AcknowledgeIncidentParams) (gen.AcknowledgeIncidentRes, error) {
	if err := s.touchIncident(ctx, params.ID, false); err != nil {
		return nil, err
	}
	return &gen.AcknowledgeIncidentNoContent{}, nil
}

// ResolveIncident resolves an open incident by hand (a success resolves it
// on its own).
func (s *Server) ResolveIncident(ctx context.Context, params gen.ResolveIncidentParams) (gen.ResolveIncidentRes, error) {
	if err := s.touchIncident(ctx, params.ID, true); err != nil {
		return nil, err
	}
	return &gen.ResolveIncidentNoContent{}, nil
}

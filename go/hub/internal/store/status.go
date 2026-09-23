package store

// Public status pages (M5.3, migration 0032); the port of repo/status.rs.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	pageOf = "SELECT project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at " +
		"FROM status_pages WHERE project_id = $1 AND org_id = $2"
	pageBySlug = "SELECT project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at " +
		"FROM status_pages WHERE slug = $1 AND enabled"
	upsertStatusPage = "INSERT INTO status_pages " +
		"(project_id, org_id, slug, title, enabled, environment_ids, updated_by, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
		"ON CONFLICT (project_id) DO UPDATE SET slug = EXCLUDED.slug, title = EXCLUDED.title, " +
		"enabled = EXCLUDED.enabled, environment_ids = EXCLUDED.environment_ids, " +
		"updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at " +
		"WHERE status_pages.org_id = EXCLUDED.org_id"
	deleteStatusPage = "DELETE FROM status_pages WHERE project_id = $1 AND org_id = $2"
	publicIncidents  = "SELECT id, target_id, severity, opened_at, resolved_at FROM incidents " +
		"WHERE org_id = $1 AND target_id = ANY($2) AND (resolved_at IS NULL OR resolved_at >= $3) " +
		"ORDER BY opened_at DESC LIMIT 50"
	openIncidentSQL = "INSERT INTO incidents " +
		"(id, org_id, project_id, environment_id, target_id, kind, severity, dedupe_key, title, detail, " +
		"opened_at, last_seen_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11) " +
		"ON CONFLICT (org_id, dedupe_key) WHERE resolved_at IS NULL DO UPDATE " +
		"SET last_seen_at = EXCLUDED.last_seen_at, occurrences = incidents.occurrences + 1, " +
		"detail = EXCLUDED.detail, severity = EXCLUDED.severity " +
		"RETURNING id, (xmax = 0) AS opened"
)

// NewIncident is an incident to open (or to count again).
type NewIncident struct {
	Project     opt.Val[ids.ProjectID]
	Environment opt.Val[ids.EnvironmentID]
	Target      opt.Val[ids.TargetID]
	Kind        string
	// Severity: `critical`, `warning` or `info`.
	Severity  string
	DedupeKey string
	Title     string
	Detail    opt.Val[string]
}

// StatusPage is a project's status page.
type StatusPage struct {
	Project      ids.ProjectID
	Org          ids.OrgID
	Slug         string
	Title        string
	Enabled      bool
	Environments []ids.EnvironmentID
	UpdatedBy    string
	UpdatedAt    int64
}

// PublicIncident is an incident as a status page may show it: when, how bad, which app.
type PublicIncident struct {
	ID         uuid.UUID
	TargetID   opt.Val[uuid.UUID]
	Severity   string
	OpenedAt   int64
	ResolvedAt opt.Val[int64]
}

// OpenIncident opens an incident, or counts it again while one with its key is open.
// It returns its id and whether it is new.
func (t *Tenant) OpenIncident(ctx context.Context, i NewIncident) (uuid.UUID, bool, error) {
	const op = "open an incident"
	id := uuid.Must(uuid.NewV7())
	now := t.store.now()
	var (
		projectID     *uuid.UUID
		environmentID *uuid.UUID
		targetID      *uuid.UUID
	)
	if p, ok := i.Project.Get(); ok {
		u := p.UUID()
		projectID = &u
	}
	if e, ok := i.Environment.Get(); ok {
		u := e.UUID()
		environmentID = &u
	}
	if tgt, ok := i.Target.Get(); ok {
		u := tgt.UUID()
		targetID = &u
	}
	var (
		resID  uuid.UUID
		opened bool
	)
	err := t.tx.QueryRow(ctx, openIncidentSQL,
		id, t.org.String(), projectID, environmentID, targetID,
		i.Kind, i.Severity, i.DedupeKey, i.Title, i.Detail.Ptr(),
		now,
	).Scan(&resID, &opened)
	if err != nil {
		return uuid.Nil, false, dbErr(op, err)
	}
	return resID, opened, nil
}

func scanStatusPage(op string) func(pgx.CollectableRow) (StatusPage, error) {
	return func(row pgx.CollectableRow) (StatusPage, error) {
		var (
			projectID      uuid.UUID
			orgIDStr       string
			slug           string
			title          string
			enabled        bool
			environmentIDs []uuid.UUID
			updatedBy      string
			updatedAt      int64
		)
		if err := row.Scan(&projectID, &orgIDStr, &slug, &title, &enabled, &environmentIDs, &updatedBy, &updatedAt); err != nil {
			return StatusPage{}, err
		}
		org, err := orgID(op, orgIDStr)
		if err != nil {
			return StatusPage{}, err
		}
		envs := make([]ids.EnvironmentID, 0, len(environmentIDs))
		for _, u := range environmentIDs {
			envs = append(envs, ids.From[ids.Environment](u))
		}
		return StatusPage{
			Project:      ids.From[ids.Project](projectID),
			Org:          org,
			Slug:         slug,
			Title:        title,
			Enabled:      enabled,
			Environments: envs,
			UpdatedBy:    updatedBy,
			UpdatedAt:    updatedAt,
		}, nil
	}
}

func scanPublicIncident(row pgx.CollectableRow) (PublicIncident, error) {
	var (
		id         uuid.UUID
		targetID   *uuid.UUID
		severity   string
		openedAt   int64
		resolvedAt *int64
	)
	if err := row.Scan(&id, &targetID, &severity, &openedAt, &resolvedAt); err != nil {
		return PublicIncident{}, err
	}
	return PublicIncident{
		ID:         id,
		TargetID:   opt.FromPtr(targetID),
		Severity:   severity,
		OpenedAt:   openedAt,
		ResolvedAt: opt.FromPtr(resolvedAt),
	}, nil
}

// StatusPage is project's status page, if it has one.
func (t *Tenant) StatusPage(ctx context.Context, project ids.ProjectID) (StatusPage, bool, error) {
	const op = "read a status page"
	return queryOpt(ctx, t.tx, op, pageOf, scanStatusPage(op), project.UUID(), t.org.String())
}

// SetStatusPage sets page (its slug must be free across the installation).
func (t *Tenant) SetStatusPage(ctx context.Context, page StatusPage) error {
	const op = "set a status page"
	envUUIDs := make([]uuid.UUID, 0, len(page.Environments))
	for _, e := range page.Environments {
		envUUIDs = append(envUUIDs, e.UUID())
	}
	updatedAt := page.UpdatedAt
	if updatedAt == 0 {
		updatedAt = t.store.now()
	}
	_, err := exec(ctx, t.tx, op, upsertStatusPage,
		page.Project.UUID(),
		t.org.String(),
		page.Slug,
		page.Title,
		page.Enabled,
		envUUIDs,
		page.UpdatedBy,
		updatedAt,
	)
	return err
}

// DeleteStatusPage removes project's status page. False when it has none.
func (t *Tenant) DeleteStatusPage(ctx context.Context, project ids.ProjectID) (bool, error) {
	const op = "delete a status page"
	n, err := exec(ctx, t.tx, op, deleteStatusPage, project.UUID(), t.org.String())
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// PublicIncidents are incidents of targets open now or resolved since since.
func (t *Tenant) PublicIncidents(ctx context.Context, targets []uuid.UUID, since int64) ([]PublicIncident, error) {
	const op = "list public incidents"
	return queryAll(ctx, t.tx, op, publicIncidents, scanPublicIncident, t.org.String(), targets, since)
}

// PublicStatusPage is the enabled status page published as slug.
func (s *Store) PublicStatusPage(ctx context.Context, slug string) (StatusPage, bool, error) {
	const op = "read a public status page"
	return queryOpt(ctx, s.db, op, pageBySlug, scanStatusPage(op), slug)
}

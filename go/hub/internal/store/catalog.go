package store

// Read models of the SQL-backed routes (ADR-032, M1.8b step 2): the live
// environments of an organization, as the API lists them. A partial port of
// repo/catalog.rs: environments only; apps, runs, run phases, config
// revisions, domains and application lookups follow with the deployment
// repositories.
//
// Deleted rows are never listed; rows being deleted are, marked.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const selectEnvironments = "SELECT e.id, e.project_id, e.slug, e.name, " +
	"COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) AS env_type, " +
	"e.quota::text AS quota, " +
	"e.protected, e.legacy_uid, e.deleting, e.created_at, pl.id AS placement_id, pl.namespace " +
	"FROM environments e " +
	"LEFT JOIN environment_placements pl " +
	"ON pl.environment_id = e.id AND pl.org_id = e.org_id AND pl.state <> 'retired' " +
	"WHERE e.org_id = $1 AND e.project_id = $2 AND e.deleted_at IS NULL " +
	"AND ($3::text IS NULL OR e.slug = $3) " +
	"ORDER BY e.slug"

// EnvironmentRecord is a live environment of a project.
type EnvironmentRecord struct {
	ID      ids.EnvironmentID
	Project ids.ProjectID
	Slug    string
	Name    string
	// EnvType is `standard`, `production` or `preview`.
	EnvType   string
	Quota     opt.Val[any]
	Protected bool
	// Placement and Namespace are the environment's placement and its
	// namespace, once it has one.
	Placement opt.Val[ids.PlacementID]
	Namespace opt.Val[string]
	LegacyUID opt.Val[uuid.UUID]
	Deleting  bool
	CreatedAt int64
}

func scanEnvironment(row pgx.CollectableRow) (EnvironmentRecord, error) {
	var e EnvironmentRecord
	var quota, namespace *string
	var legacy *uuid.UUID
	var placement *ids.PlacementID
	err := row.Scan(&e.ID, &e.Project, &e.Slug, &e.Name, &e.EnvType, &quota, &e.Protected, &legacy,
		&e.Deleting, &e.CreatedAt, &placement, &namespace)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	if e.Quota, err = jsonColumn(quota); err != nil {
		return EnvironmentRecord{}, err
	}
	e.Placement = opt.FromPtr(placement)
	e.Namespace = opt.FromPtr(namespace)
	e.LegacyUID = opt.FromPtr(legacy)
	return e, nil
}

// jsonColumn reads a JSON text column; invalid JSON is a decode error.
func jsonColumn(text *string) (opt.Val[any], error) {
	if text == nil {
		return opt.None[any](), nil
	}
	var v any
	if err := json.Unmarshal([]byte(*text), &v); err != nil {
		return opt.None[any](), decodeErr("read a JSON column", "%v", err)
	}
	return opt.Some(v), nil
}

func (t *Tenant) environmentRows(ctx context.Context, project ids.ProjectID, slug opt.Val[string]) ([]EnvironmentRecord, error) {
	return queryAll(ctx, t.tx, "list environments", selectEnvironments, scanEnvironment,
		t.org.String(), project, slug.Ptr())
}

// Environments is the live environments of project, ordered by slug.
func (t *Tenant) Environments(ctx context.Context, project ids.ProjectID) ([]EnvironmentRecord, error) {
	return t.environmentRows(ctx, project, opt.None[string]())
}

// Environment is the live environment slug of project.
func (t *Tenant) Environment(ctx context.Context, project ids.ProjectID, slug string) (EnvironmentRecord, bool, error) {
	return last(t.environmentRows(ctx, project, opt.Some(slug)))
}

// rustQuote is Rust's `{:?}` of a string for the messages that used it; see
// SUBSTITUTIONS.md ("Rust {:?} in messages").
func rustQuote(s string) string { return strconv.Quote(s) }

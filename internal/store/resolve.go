package store

// Name → SQL id resolution for the API's scope chain (ADR-032); the port of
// repo/resolve.rs.
//
// While the resource model and SQL coexist, a route still finds its project,
// environment and app in the resource projections, and asks here for the SQL
// rows behind them: by a recorded Kubernetes UID (`legacy_uid`, empty in
// practice: 1.x data is not carried over), else by slug, always inside the
// tenant's organization.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const (
	findProject = "SELECT id FROM projects " +
		"WHERE org_id = $1 AND deleted_at IS NULL AND (legacy_uid = $2 OR slug = $3) " +
		"ORDER BY legacy_uid = $2 DESC NULLS LAST " +
		"LIMIT 1"
	findEnvironment = "SELECT id FROM environments " +
		"WHERE org_id = $1 AND project_id = $2 AND deleted_at IS NULL AND (legacy_uid = $3 OR slug = $4) " +
		"ORDER BY legacy_uid = $3 DESC NULLS LAST " +
		"LIMIT 1"
	findTarget = "SELECT t.id, t.application_id FROM application_targets t " +
		"JOIN applications a ON a.id = t.application_id " +
		"JOIN environment_placements p ON p.id = t.placement_id " +
		"WHERE t.org_id = $1 AND t.project_id = $2 AND p.environment_id = $3 " +
		"AND t.deleted_at IS NULL AND a.deleted_at IS NULL " +
		"AND (t.legacy_uid = $4 OR a.slug = $5) " +
		"ORDER BY t.legacy_uid = $4 DESC NULLS LAST, t.created_at " +
		"LIMIT 1"
)

// Named is one level of a scope to resolve: its slug and, when it came from
// a resource projection, that resource's Kubernetes UID.
type Named struct {
	Slug      string
	LegacyUID opt.Val[uuid.UUID]
}

// SQLScope (Rust SqlScope) is the SQL ids behind a resolved scope; absent where SQL has no
// row yet.
type SQLScope struct {
	Project     opt.Val[ids.ProjectID]
	Environment opt.Val[ids.EnvironmentID]
	// Target is the application target: one application in this
	// environment.
	Target opt.Val[ids.TargetID]
	// Application is the application of that target.
	Application opt.Val[ids.ApplicationID]
}

// ResolveScope resolves a project and, below it, an environment and an
// application target. A level is looked up only when the level above was
// found; a row matching the Kubernetes UID wins over one matching the slug.
func (t *Tenant) ResolveScope(ctx context.Context, project Named, environment, app opt.Val[Named]) (SQLScope, error) {
	const op = "resolve a scope"
	org := t.org.String()
	var scope SQLScope
	projectID, ok, err := queryOpt(ctx, t.tx, op, findProject, scanID[ids.Project],
		org, project.LegacyUID.Ptr(), project.Slug)
	if err != nil {
		return SQLScope{}, err
	}
	if !ok {
		return scope, nil
	}
	scope.Project = opt.Some(projectID)
	env, ok := environment.Get()
	if !ok {
		return scope, nil
	}
	environmentID, ok, err := queryOpt(ctx, t.tx, op, findEnvironment, scanID[ids.Environment],
		org, projectID, env.LegacyUID.Ptr(), env.Slug)
	if err != nil {
		return SQLScope{}, err
	}
	if !ok {
		return scope, nil
	}
	scope.Environment = opt.Some(environmentID)
	a, ok := app.Get()
	if !ok {
		return scope, nil
	}
	type found struct {
		target      ids.TargetID
		application ids.ApplicationID
	}
	row, ok, err := queryOpt(ctx, t.tx, op, findTarget, func(row pgx.CollectableRow) (found, error) {
		var f found
		err := row.Scan(&f.target, &f.application)
		return f, err
	}, org, projectID, environmentID, a.LegacyUID.Ptr(), a.Slug)
	if err != nil {
		return SQLScope{}, err
	}
	if ok {
		scope.Target = opt.Some(row.target)
		scope.Application = opt.Some(row.application)
	}
	return scope, nil
}

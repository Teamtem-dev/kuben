package store

// Product schema (ADR-025, ADR-026): projects, environments, clusters,
// placements, applications and targets; the port of repo/product.rs.
//
// Every statement runs in a [Tenant] transaction, which names the
// organization for row-level security (`kuben.org_id`, I21) and filters by
// it explicitly as well: authorization never relies on row-level security
// alone. Tenant-aware foreign keys refuse a row that points into another
// organization or project, whatever the caller passes.

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

const (
	setTenant              = "SELECT set_config('kuben.org_id', $1, true)"
	insertProject          = "INSERT INTO projects (id, org_id, slug, name, created_at) VALUES ($1, $2, $3, $4, $5)"
	insertProjectDescribed = "INSERT INTO projects (id, org_id, slug, name, description, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6)"
	selectProjects = "SELECT p.id, p.slug, p.name, p.description, p.created_at, p.legacy_uid, p.deleting, " +
		"(SELECT count(*) FROM environments e WHERE e.project_id = p.id AND e.deleted_at IS NULL) AS environments " +
		"FROM projects p " +
		"WHERE p.org_id = $1 AND p.deleted_at IS NULL AND ($2::text IS NULL OR p.slug = $2) " +
		"ORDER BY p.slug"
	insertEnvironment = "INSERT INTO environments " +
		"(id, org_id, project_id, slug, name, protected, env_type, quota, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)"
	insertCluster = "INSERT INTO clusters (id, org_id, name, created_at) VALUES ($1, $2, $3, $4)"
	ensureCluster = "INSERT INTO clusters (id, org_id, name, created_at) VALUES ($1, $2, $3, $4) " +
		"ON CONFLICT (org_id, name) DO UPDATE SET name = EXCLUDED.name RETURNING id"
	insertPlacement = "INSERT INTO environment_placements " +
		"(id, org_id, project_id, environment_id, cluster_id, namespace, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)"
	insertApplication = "INSERT INTO applications (id, org_id, project_id, slug, name, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6)"
	// A new target is delivered by its cluster's agent when that agent is
	// linked, unrevoked and negotiated the runtime feature (migration 0012).
	insertTarget = "INSERT INTO application_targets " +
		"(id, org_id, project_id, application_id, placement_id, lifecycle_uid, created_at, delivery) " +
		"SELECT $1, $2, $3, $4, $5, $6, $7, " +
		"CASE WHEN EXISTS (SELECT 1 FROM environment_placements p " +
		"JOIN cluster_agents a ON a.cluster_id = p.cluster_id AND a.org_id = p.org_id " +
		"WHERE p.id = $5 AND a.revoked_at IS NULL " +
		"AND a.features @> jsonb_build_array($8::text)) " +
		"THEN 'agent' ELSE 'controller' END"
	selectTargetState = "SELECT lifecycle_uid, deleting, desired_generation, source_epoch, " +
		"build_config_revision, deploy_policy FROM application_targets WHERE id = $1 AND org_id = $2"
)

// RuntimeFeature is the agent feature that makes the agent deliver an
// application's runtime (repo/agents.rs RUNTIME_FEATURE).
const RuntimeFeature = "applicationRuntime"

// Project is a live project of the current organization.
type Project struct {
	ID          ids.ProjectID
	Slug        string
	Name        string
	Description opt.Val[string]
	CreatedAt   int64
	LegacyUID   opt.Val[uuid.UUID]
	Deleting    bool
	// Environments is the number of its live environments.
	Environments uint32
}

// EnvironmentKind is what an environment is for; production is protected.
type EnvironmentKind string

// The kinds, with their stored names.
const (
	Standard   EnvironmentKind = "standard"
	Production EnvironmentKind = "production"
	Preview    EnvironmentKind = "preview"
)

func (k EnvironmentKind) String() string { return string(k) }

// PlacementID is a placement id read back from SQL, for callers that only
// hold the UUID.
func PlacementID(id uuid.UUID) ids.PlacementID { return ids.From[ids.Placement](id) }

// Tenant is a transaction scoped to one organization. Row-level security
// shows it only that organization's rows. It must end with [Tenant.Commit]
// or [Tenant.Rollback]; `defer t.Rollback(ctx)` after [Store.Tenant] is the
// Go form of Rust's rollback on drop.
type Tenant struct {
	org   ids.OrgID
	tx    pgx.Tx
	store *Store
}

// Tenant begins a transaction for org. The tenant setting is
// transaction-local, so a pooled connection never carries it into the next
// transaction.
func (s *Store) Tenant(ctx context.Context, org ids.OrgID) (*Tenant, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, dbErr("begin a tenant transaction", err)
	}
	if _, err := tx.Exec(ctx, setTenant, org.String()); err != nil {
		return nil, errors.Join(dbErr("set the tenant", err), dbErr("roll back", tx.Rollback(ctx)))
	}
	return &Tenant{org: org, tx: tx, store: s}, nil
}

// Org is the organization the transaction is scoped to.
func (t *Tenant) Org() ids.OrgID { return t.org }

// Commit commits the transaction.
func (t *Tenant) Commit(ctx context.Context) error {
	return dbErr("commit", t.tx.Commit(ctx))
}

// Rollback rolls the transaction back; after a commit it does nothing.
func (t *Tenant) Rollback(ctx context.Context) error {
	if err := t.tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return dbErr("roll back", err)
	}
	return nil
}

// CreateProject creates a project without a description.
func (t *Tenant) CreateProject(ctx context.Context, slug, name string) (ids.ProjectID, error) {
	id := ids.New[ids.Project]()
	_, err := exec(ctx, t.tx, "create a project", insertProject, id, t.org.String(), slug, name, t.store.now())
	if err != nil {
		return ids.ProjectID{}, err
	}
	return id, nil
}

// CreateProjectDescribed creates a project with an optional description.
func (t *Tenant) CreateProjectDescribed(ctx context.Context, slug, name string, description opt.Val[string]) (ids.ProjectID, error) {
	id := ids.New[ids.Project]()
	_, err := exec(ctx, t.tx, "create a project", insertProjectDescribed,
		id, t.org.String(), slug, name, description.Ptr(), t.store.now())
	if err != nil {
		return ids.ProjectID{}, err
	}
	return id, nil
}

func (t *Tenant) projectRows(ctx context.Context, slug opt.Val[string]) ([]Project, error) {
	return queryAll(ctx, t.tx, "list projects", selectProjects, scanProject, t.org.String(), slug.Ptr())
}

func scanProject(row pgx.CollectableRow) (Project, error) {
	var p Project
	var description *string
	var legacy *uuid.UUID
	var environments int64
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &description, &p.CreatedAt, &legacy, &p.Deleting, &environments); err != nil {
		return Project{}, err
	}
	p.Description = opt.FromPtr(description)
	p.LegacyUID = opt.FromPtr(legacy)
	// Rust: u32::try_from(n).unwrap_or(u32::MAX).
	p.Environments = math.MaxUint32
	if environments >= 0 && environments <= math.MaxUint32 {
		p.Environments = uint32(environments)
	}
	return p, nil
}

// Projects is the organization's live projects, ordered by slug.
func (t *Tenant) Projects(ctx context.Context) ([]Project, error) {
	return t.projectRows(ctx, opt.None[string]())
}

// Project is the organization's live project slug.
func (t *Tenant) Project(ctx context.Context, slug string) (Project, bool, error) {
	return last(t.projectRows(ctx, opt.Some(slug)))
}

// last is Rust's `Vec::pop` of a query's rows.
func last[T any](rows []T, err error) (T, bool, error) {
	var zero T
	if err != nil || len(rows) == 0 {
		return zero, false, err
	}
	return rows[len(rows)-1], true, nil
}

// CreateEnvironment creates a production (protected) or standard
// environment.
func (t *Tenant) CreateEnvironment(ctx context.Context, project ids.ProjectID, slug, name string, protected bool) (ids.EnvironmentID, error) {
	kind := Standard
	if protected {
		kind = Production
	}
	return t.CreateEnvironmentTyped(ctx, project, slug, name, kind, opt.None[any]())
}

// CreateEnvironmentTyped creates an environment of kind; production
// environments are protected. quota is stored as JSON.
func (t *Tenant) CreateEnvironmentTyped(
	ctx context.Context, project ids.ProjectID, slug, name string, kind EnvironmentKind, quota opt.Val[any],
) (ids.EnvironmentID, error) {
	const op = "create an environment"
	id := ids.New[ids.Environment]()
	quotaText := opt.None[string]()
	if q, ok := quota.Get(); ok {
		// serde_json's Value::to_string: compact, keys sorted.
		text, err := wire.CanonicalValue(q)
		if err != nil {
			return ids.EnvironmentID{}, dbErr(op, err)
		}
		quotaText = opt.Some(text)
	}
	_, err := exec(ctx, t.tx, op, insertEnvironment,
		id, t.org.String(), project, slug, name, kind == Production, kind.String(), quotaText.Ptr(), t.store.now())
	if err != nil {
		return ids.EnvironmentID{}, err
	}
	return id, nil
}

// EnsureCluster is the organization's cluster name, created on first use.
func (t *Tenant) EnsureCluster(ctx context.Context, name string) (ids.ClusterID, error) {
	var id ids.ClusterID
	err := queryOne(ctx, t.tx, "ensure a cluster", ensureCluster, []any{&id},
		ids.New[ids.Cluster](), t.org.String(), name, t.store.now())
	if err != nil {
		return ids.ClusterID{}, err
	}
	return id, nil
}

// CreateCluster creates a cluster.
func (t *Tenant) CreateCluster(ctx context.Context, name string) (ids.ClusterID, error) {
	id := ids.New[ids.Cluster]()
	if _, err := exec(ctx, t.tx, "create a cluster", insertCluster, id, t.org.String(), name, t.store.now()); err != nil {
		return ids.ClusterID{}, err
	}
	return id, nil
}

// CreatePlacement binds environment to namespace in cluster. The binding is
// final: moving is a new placement and a cutover.
func (t *Tenant) CreatePlacement(
	ctx context.Context, project ids.ProjectID, environment ids.EnvironmentID, cluster ids.ClusterID, namespace string,
) (ids.PlacementID, error) {
	id := ids.New[ids.Placement]()
	_, err := exec(ctx, t.tx, "create a placement", insertPlacement,
		id, t.org.String(), project, environment, cluster, namespace, t.store.now())
	if err != nil {
		return ids.PlacementID{}, err
	}
	return id, nil
}

// CreateApplication creates an application in project.
func (t *Tenant) CreateApplication(ctx context.Context, project ids.ProjectID, slug, name string) (ids.ApplicationID, error) {
	id := ids.New[ids.Application]()
	_, err := exec(ctx, t.tx, "create an application", insertApplication,
		id, t.org.String(), project, slug, name, t.store.now())
	if err != nil {
		return ids.ApplicationID{}, err
	}
	return id, nil
}

// CreateTarget puts application on placement, both of project, with a
// fresh lifecycle UID and generation 0.
func (t *Tenant) CreateTarget(
	ctx context.Context, project ids.ProjectID, application ids.ApplicationID, placement ids.PlacementID,
) (ids.TargetID, error) {
	const op = "create a target"
	id := ids.New[ids.Target]()
	lifecycle, err := uuid.NewV7()
	if err != nil {
		return ids.TargetID{}, dbErr(op, err)
	}
	_, err = exec(ctx, t.tx, op, insertTarget,
		id, t.org.String(), project, application, placement, lifecycle, t.store.now(), RuntimeFeature)
	if err != nil {
		return ids.TargetID{}, err
	}
	return id, nil
}

type targetRow struct {
	lifecycleUID        uuid.UUID
	deleting            bool
	desiredGeneration   int64
	sourceEpoch         int64
	buildConfigRevision int64
	deployPolicy        string
}

// TargetState is the control state of tgt; false when the organization has
// no such target.
func (t *Tenant) TargetState(ctx context.Context, tgt ids.TargetID) (target.State, bool, error) {
	const op = "read a target's state"
	r, ok, err := queryOpt(ctx, t.tx, op, selectTargetState, func(row pgx.CollectableRow) (targetRow, error) {
		var r targetRow
		err := row.Scan(&r.lifecycleUID, &r.deleting, &r.desiredGeneration, &r.sourceEpoch,
			&r.buildConfigRevision, &r.deployPolicy)
		return r, err
	}, tgt, t.org.String())
	if err != nil || !ok {
		return target.State{}, false, err
	}
	generation, err := counter(op, r.desiredGeneration)
	if err != nil {
		return target.State{}, false, err
	}
	epoch, err := counter(op, r.sourceEpoch)
	if err != nil {
		return target.State{}, false, err
	}
	revision, err := counter(op, r.buildConfigRevision)
	if err != nil {
		return target.State{}, false, err
	}
	policy, err := deployPolicy(op, r.deployPolicy)
	if err != nil {
		return target.State{}, false, err
	}
	return target.State{
		LifecycleUID:        r.lifecycleUID,
		Deleting:            r.deleting,
		DesiredGeneration:   target.Generation(generation),
		SourceEpoch:         target.SourceEpoch(epoch),
		BuildConfigRevision: revision,
		Policy:              policy,
	}, true, nil
}

// deployPolicy reads a stored deploy policy.
func deployPolicy(op, value string) (target.DeployPolicy, error) {
	switch p := target.DeployPolicy(value); p {
	case target.Auto, target.Manual, target.Pinned:
		return p, nil
	}
	return "", decodeErr(op, "unknown deploy policy %s", rustQuote(value))
}

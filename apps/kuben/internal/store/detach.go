package store

// Export and detach (M4.11, migration 0029); the port of repo/detach.rs:
// what an app's export is made of, and the record of apps handed over out
// of Kuben.

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

const (
	exportRun = "SELECT r.id, r.generation, r.phase, r.reason, r.release_id, " +
		"rel.artifacts::text AS artifacts, rel.process_contract::text AS process_contract, " +
		"rel.portable_config::text AS portable_config, rel.source::text AS source, " +
		"c.revision AS config_revision, c.config::text AS config, " +
		"p.renderer_version, p.capability_snapshot::text AS capabilities, p.resources::text AS resources " +
		"FROM deployment_runs r " +
		"JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id " +
		"JOIN target_config_revisions c ON c.id = r.config_revision_id AND c.org_id = r.org_id " +
		"LEFT JOIN render_plans p ON p.id = r.render_plan_id AND p.org_id = r.org_id " +
		"WHERE r.target_id = $1 AND r.org_id = $2 AND r.render_plan_id IS NOT NULL " +
		"ORDER BY (r.phase = 'succeeded') DESC, r.generation DESC LIMIT 1"
	inFlight = "SELECT EXISTS (SELECT 1 FROM deployment_runs r " +
		"JOIN operations o ON o.id = r.operation_id " +
		"WHERE r.target_id = $1 AND r.org_id = $2 AND NOT o.done)"
	exportSecrets = "SELECT s.name, b.revision, s.kind, s.registry FROM run_secret_bindings b " +
		"JOIN secrets s ON s.id = b.secret_id AND s.org_id = b.org_id " +
		"WHERE b.run_id = $1 AND b.org_id = $2 ORDER BY s.name"
	insertDetached = "INSERT INTO detached_apps " +
		"(target_id, org_id, project_id, environment_id, app, namespace, reason, requested_by, requested_at, export) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)"
	completeDetached = "UPDATE detached_apps SET completed_at = $3 " +
		"WHERE target_id = $1 AND org_id = $2 AND completed_at IS NULL"
	releaseDetached = "UPDATE detached_apps SET released_at = $3, released_by = $4 " +
		"WHERE target_id = $1 AND org_id = $2 AND released_at IS NULL"
	selectDetached = "SELECT target_id, project_id, environment_id, app, namespace, reason, requested_by, " +
		"requested_at, completed_at, released_at, released_by FROM detached_apps"
	detachedOne = selectDetached + " WHERE target_id = $1 AND org_id = $2"
	detachedIn  = selectDetached + " WHERE environment_id = $1 AND org_id = $2 ORDER BY requested_at DESC"
	held        = "SELECT count(*) FROM detached_apps " +
		"WHERE environment_id = $1 AND org_id = $2 AND released_at IS NULL"
	exportOf = "SELECT export::text FROM detached_apps WHERE target_id = $1 AND org_id = $2"
)

// ExportMaterial is the newest delivered run of an app: what its export is
// made of.
type ExportMaterial struct {
	Run             ids.DeploymentRunID
	Generation      int64
	Phase           string
	Reason          string
	Release         uuid.UUID
	Artifacts       any
	ProcessContract any
	PortableConfig  any
	Source          opt.Val[any]
	ConfigRevision  int64
	Config          any
	RendererVersion string
	// Capabilities is the plan's capability snapshot, null without a plan.
	Capabilities any
	// Resources is the frozen render plan: Kubernetes objects in apply
	// order, null without a plan.
	Resources any
	Secrets   []SecretReference
}

// SecretReference is a secret revision a run uses.
type SecretReference struct {
	Name     string
	Revision int64
	// Kind is `opaque`, or `registry` for a pull secret.
	Kind     string
	Registry opt.Val[string]
}

// Object is the name of the Kubernetes Secret holding the revision:
// `<name>.r<revision>`.
func (r SecretReference) Object() string {
	return r.Name + ".r" + strconv.FormatInt(r.Revision, 10)
}

// NewDetach is a detach request.
type NewDetach struct {
	Project     ids.ProjectID
	Environment ids.EnvironmentID
	Target      ids.TargetID
	App         string
	Namespace   string
	Reason      string
	RequestedBy string
	Export      any
}

// DetachedApp is an app detached from Kuben.
type DetachedApp struct {
	TargetID      uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	App           string
	Namespace     string
	Reason        string
	RequestedBy   string
	RequestedAt   int64
	CompletedAt   opt.Val[int64]
	ReleasedAt    opt.Val[int64]
	ReleasedBy    opt.Val[string]
}

// ExportMaterial is what tgt's export is made of: its newest succeeded run
// with a frozen plan, else its newest run with one; false before any.
func (t *Tenant) ExportMaterial(ctx context.Context, tgt ids.TargetID) (ExportMaterial, bool, error) {
	const op = "read an export's material"
	org := t.org.String()
	m, found, err := queryOpt(ctx, t.tx, op, exportRun, scanExport, tgt, org)
	if err != nil || !found {
		return ExportMaterial{}, false, err
	}
	m.Secrets, err = queryAll(ctx, t.tx, op, exportSecrets, func(row pgx.CollectableRow) (SecretReference, error) {
		var r SecretReference
		var registry *string
		err := row.Scan(&r.Name, &r.Revision, &r.Kind, &registry)
		r.Registry = opt.FromPtr(registry)
		return r, err
	}, m.Run, org)
	if err != nil {
		return ExportMaterial{}, false, err
	}
	return m, true, nil
}

func scanExport(row pgx.CollectableRow) (ExportMaterial, error) {
	const op = "read an export's material"
	var m ExportMaterial
	var artifacts, contract, portable, config string
	var source, renderer, capabilities, resources *string
	if err := row.Scan(&m.Run, &m.Generation, &m.Phase, &m.Reason, &m.Release, &artifacts, &contract,
		&portable, &source, &m.ConfigRevision, &config, &renderer, &capabilities, &resources); err != nil {
		return ExportMaterial{}, err
	}
	var err error
	for _, field := range []struct {
		text string
		into *any
	}{{artifacts, &m.Artifacts}, {contract, &m.ProcessContract}, {portable, &m.PortableConfig}, {config, &m.Config}} {
		if *field.into, err = jsonValue(op, field.text); err != nil {
			return ExportMaterial{}, err
		}
	}
	if m.Source, err = jsonColumn(source); err != nil {
		return ExportMaterial{}, err
	}
	m.RendererVersion = opt.FromPtr(renderer).Or("")
	if m.Capabilities, err = nullable(op, capabilities); err != nil {
		return ExportMaterial{}, err
	}
	if m.Resources, err = nullable(op, resources); err != nil {
		return ExportMaterial{}, err
	}
	return m, nil
}

// nullable reads optional JSON text as a value, null when absent.
func nullable(op string, text *string) (any, error) {
	if text == nil {
		return nil, nil //nolint:nilnil // nil is the JSON null of an absent document
	}
	return jsonValue(op, *text)
}

// RunInFlight is whether a run of tgt has not finished.
func (t *Tenant) RunInFlight(ctx context.Context, tgt ids.TargetID) (bool, error) {
	var busy bool
	err := queryOne(ctx, t.tx, "check for a run in flight", inFlight, []any{&busy}, tgt, t.org.String())
	return busy, err
}

// RecordDetach records a detach request (the target is marked deleting
// separately).
func (t *Tenant) RecordDetach(ctx context.Context, d NewDetach) error {
	const op = "record a detach"
	export, err := canonical(op, d.Export)
	if err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, insertDetached, d.Target, t.org.String(), d.Project, d.Environment,
		d.App, d.Namespace, d.Reason, d.RequestedBy, t.store.now(), export)
	return err
}

func scanDetached(row pgx.CollectableRow) (DetachedApp, error) {
	var d DetachedApp
	var completed, released *int64
	var by *string
	if err := row.Scan(&d.TargetID, &d.ProjectID, &d.EnvironmentID, &d.App, &d.Namespace, &d.Reason,
		&d.RequestedBy, &d.RequestedAt, &completed, &released, &by); err != nil {
		return DetachedApp{}, err
	}
	d.CompletedAt, d.ReleasedAt, d.ReleasedBy = opt.FromPtr(completed), opt.FromPtr(released), opt.FromPtr(by)
	return d, nil
}

// DetachedApp is the detach record of tgt, if it was detached.
func (t *Tenant) DetachedApp(ctx context.Context, tgt ids.TargetID) (DetachedApp, bool, error) {
	return queryOpt(ctx, t.tx, "read a detached app", detachedOne, scanDetached, tgt, t.org.String())
}

// DetachedApps is the apps detached from environment, newest first.
func (t *Tenant) DetachedApps(ctx context.Context, environment ids.EnvironmentID) ([]DetachedApp, error) {
	return queryAll(ctx, t.tx, "list detached apps", detachedIn, scanDetached, environment, t.org.String())
}

// DetachedExport is the export frozen when tgt was detached.
func (t *Tenant) DetachedExport(ctx context.Context, tgt ids.TargetID) (opt.Val[any], error) {
	const op = "read a detached export"
	text, found, err := queryOpt(ctx, t.tx, op, exportOf, pgx.RowTo[string], tgt, t.org.String())
	if err != nil || !found {
		return opt.None[any](), err
	}
	v, err := jsonValue(op, text)
	if err != nil {
		return opt.None[any](), err
	}
	return opt.Some(v), nil
}

// CompleteDetach marks the detach of tgt complete: its objects are
// orphaned.
func (t *Tenant) CompleteDetach(ctx context.Context, tgt ids.TargetID) (bool, error) {
	n, err := exec(ctx, t.tx, "complete a detach", completeDetached, tgt, t.org.String(), t.store.now())
	return n == 1, err
}

// ReleaseDetached records that someone took over the detached tgt for good:
// its environment's deletion no longer keeps the namespace for it. False
// when it was released already or is not detached.
func (t *Tenant) ReleaseDetached(ctx context.Context, tgt ids.TargetID, by string) (bool, error) {
	n, err := exec(ctx, t.tx, "release a detached app", releaseDetached, tgt, t.org.String(), t.store.now(), by)
	return n == 1, err
}

// DetachedHeld is the number of unreleased detached apps in environment.
func (t *Tenant) DetachedHeld(ctx context.Context, environment ids.EnvironmentID) (uint64, error) {
	var n int64
	if err := queryOne(ctx, t.tx, "count held detached apps", held, []any{&n}, environment, t.org.String()); err != nil {
		return 0, err
	}
	return uint64(max(n, 0)), nil //nolint:gosec // clamped like Rust's unwrap_or_default
}

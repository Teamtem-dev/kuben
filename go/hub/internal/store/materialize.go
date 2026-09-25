package store

// Reads and records of the materializer (ADR-032); the port of
// repo/materialize.rs.
//
// [Tenant.Materialization] gathers everything the materializer renders a
// claimed deployment run from: the run, its target and placement, the
// project, environment and application names, the release and the
// configuration revision. [Store.RecordMaterialization] records the App
// object a write produced, conditional on the claim's fence and never over a
// newer generation. [Store.RecordDrift] records a change someone else made
// to that object.

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	selectMaterialization = "SELECT r.id AS run_id, r.phase, r.generation, r.lifecycle_uid, r.render_plan_id, " +
		"r.restarted_at, r.approval_expires_at, r.reason = 'emergency' AS emergency, " +
		"t.paused_at IS NOT NULL AS paused, " +
		"pr.id AS project_id, pr.slug AS project_slug, pr.name AS project_name, " +
		"pr.description AS project_description, " +
		"e.id AS environment_id, e.slug AS environment_slug, e.name AS environment_name, e.protected, " +
		"COALESCE(e.env_type, CASE WHEN e.protected THEN 'production' ELSE 'standard' END) AS env_type, " +
		"e.quota::text AS quota, " +
		"p.namespace, p.cluster_id, t.delivery, a.id AS application_id, a.slug AS application_slug, " +
		"a.name AS application_name, " +
		"t.id AS target_id, t.desired_generation, (t.deleting OR e.deleting OR pr.deleting) AS deleting, " +
		"rel.id AS release_id, rel.artifacts::text AS artifacts, rel.source::text AS source, " +
		"c.id AS config_revision_id, c.revision AS config_revision, c.config::text AS config " +
		"FROM deployment_runs r " +
		"JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = r.org_id " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = r.org_id " +
		"JOIN environments e ON e.id = p.environment_id AND e.org_id = r.org_id " +
		"JOIN projects pr ON pr.id = r.project_id AND pr.org_id = r.org_id " +
		"JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id " +
		"JOIN target_config_revisions c ON c.id = r.config_revision_id AND c.org_id = r.org_id " +
		"WHERE r.operation_id = $1 AND r.org_id = $2"
	recordMaterialization = "INSERT INTO target_materializations " +
		"(target_id, org_id, project_id, generation, operation_id, resource_uid, resource_generation, written_at) " +
		"SELECT $1, $2, $3, $4, o.id, $5, $6, kuben_now_ms() FROM operations o " +
		"WHERE o.id = $7 AND o.fence = $8 AND NOT o.done AND o.org_id = $2 AND o.target_id = $1 " +
		"ON CONFLICT (target_id) DO UPDATE " +
		"SET generation = EXCLUDED.generation, operation_id = EXCLUDED.operation_id, " +
		"resource_uid = EXCLUDED.resource_uid, resource_generation = EXCLUDED.resource_generation, " +
		"written_at = EXCLUDED.written_at " +
		"WHERE target_materializations.generation <= EXCLUDED.generation"
	materializedTarget = "SELECT target_id, org_id, project_id, generation, operation_id, " +
		"resource_uid, resource_generation, drift_count, drift_detected_at, drift::text AS drift " +
		"FROM target_materializations WHERE target_id = $1 AND org_id = $2"
	materializedResource = "SELECT target_id, org_id, project_id, generation, operation_id, " +
		"resource_uid, resource_generation, drift_count, drift_detected_at, drift::text AS drift " +
		"FROM target_materializations WHERE resource_uid = $1"
	recordDrift = "UPDATE target_materializations " +
		"SET resource_uid = COALESCE($3, resource_uid), resource_generation = COALESCE($4, resource_generation), " +
		"drift_count = drift_count + 1, drift_detected_at = kuben_now_ms(), drift = $5::jsonb " +
		"WHERE target_id = $1 AND generation = $2"
	runPlanID  = "SELECT render_plan_id FROM deployment_runs WHERE id = $1 AND org_id = $2"
	setRunPlan = "UPDATE deployment_runs SET render_plan_id = $3, updated_at = kuben_now_ms() " +
		"WHERE id = $1 AND org_id = $2 AND render_plan_id IS NULL AND EXISTS (SELECT 1 FROM operations o " +
		"WHERE o.id = deployment_runs.operation_id AND o.id = $4 AND o.fence = $5 AND NOT o.done)"
	runPlan = "SELECT p.id, p.renderer_version, p.capability_snapshot::text AS capability_snapshot, " +
		"p.resources::text AS resources FROM deployment_runs r " +
		"JOIN render_plans p ON p.id = r.render_plan_id AND p.org_id = r.org_id " +
		"WHERE r.id = $1 AND r.org_id = $2"
)

// Materialization is everything a deployment run is rendered from.
type Materialization struct {
	Org       ids.OrgID
	Run       ids.DeploymentRunID
	Operation ids.OperationID
	Phase     run.Phase
	// Generation is the target generation the run owns.
	Generation         target.Generation
	LifecycleUID       uuid.UUID
	Project            ids.ProjectID
	ProjectSlug        string
	ProjectName        string
	ProjectDescription opt.Val[string]
	Environment        ids.EnvironmentID
	EnvironmentSlug    string
	EnvironmentName    string
	Protected          bool
	// EnvType is `standard`, `production` or `preview`.
	EnvType string
	Quota   opt.Val[any]
	// Namespace is the namespace of the target's placement.
	Namespace string
	// Cluster is the cluster of the target's placement.
	Cluster ids.ClusterID
	// Delivery is how the target's runs reach the cluster.
	Delivery        Delivery
	Application     ids.ApplicationID
	ApplicationSlug string
	ApplicationName string
	Target          ids.TargetID
	// DesiredGeneration is the target's generation now. A live object that
	// claims a higher one was not written by Kuben.
	DesiredGeneration target.Generation
	// Deleting: the target, its environment or its project is being
	// deleted.
	Deleting bool
	Release  ids.ReleaseID
	// Artifacts is process name → digest.
	Artifacts map[string]artifact.Digest
	// ImageRepository is `releases.source.image_repository`: where the
	// digests live.
	ImageRepository      opt.Val[string]
	ConfigRevision       ids.ConfigRevisionID
	ConfigRevisionNumber uint64
	// Config is the App spec without its image (the config revision
	// contract).
	Config any
	// RenderPlan is the run's frozen RenderPlan, once the materializer has
	// frozen it.
	RenderPlan opt.Val[ids.RenderPlanID]
	// RestartedAt is the restart stamp the run renders with (unix ms): the
	// time of the target's latest restart run, if it had one.
	RestartedAt opt.Val[int64]
	// ApprovalExpiresAt is when a run waiting for approval is cancelled
	// (unix ms).
	ApprovalExpiresAt opt.Val[int64]
	// Secrets is the secret revisions the run renders, by referenced name
	// (M4.4).
	Secrets []SecretBinding
	// Emergency: the run is an emergency rollback, which a pause does not
	// hold.
	Emergency bool
	// Paused: the target's delivery is paused (M4.9).
	Paused bool
}

// RunPlan is a run's frozen RenderPlan (ADR-026).
type RunPlan struct {
	ID                 ids.RenderPlanID
	RendererVersion    string
	CapabilitySnapshot any
	// Resources is the normalized resources, an array of objects.
	Resources any
}

// Materialized is what the materializer last wrote for a target.
type Materialized struct {
	Org        ids.OrgID
	Project    ids.ProjectID
	Target     ids.TargetID
	Generation target.Generation
	Operation  ids.OperationID
	// ResourceUID is the Kubernetes UID of the App object.
	ResourceUID string
	// ResourceGeneration is the object's `metadata.generation` after the
	// last write by Kuben.
	ResourceGeneration int64
	DriftCount         uint64
	DriftDetectedAt    opt.Val[int64]
	Drift              opt.Val[any]
}

// Replacement is the App object that replaced a drifted one: its UID and
// `metadata.generation` (a deleted object comes back with a new UID).
type Replacement struct {
	UID        string
	Generation int64
}

type materializationRow struct {
	run                              ids.DeploymentRunID
	phase                            string
	generation                       int64
	lifecycleUID                     uuid.UUID
	renderPlan                       *ids.RenderPlanID
	restartedAt, approvalExpiresAt   *int64
	emergency, paused                bool
	project                          ids.ProjectID
	projectSlug, projectName         string
	projectDescription               *string
	environment                      ids.EnvironmentID
	environmentSlug, environmentName string
	protected                        bool
	envType                          string
	quota                            *string
	namespace                        string
	cluster                          ids.ClusterID
	delivery                         string
	application                      ids.ApplicationID
	applicationSlug, applicationName string
	target                           ids.TargetID
	desiredGeneration                int64
	deleting                         bool
	release                          ids.ReleaseID
	artifacts                        string
	source                           *string
	configRevision                   ids.ConfigRevisionID
	configRevisionNumber             int64
	config                           string
}

func scanMaterialization(row pgx.CollectableRow) (materializationRow, error) {
	var r materializationRow
	err := row.Scan(&r.run, &r.phase, &r.generation, &r.lifecycleUID, &r.renderPlan, &r.restartedAt,
		&r.approvalExpiresAt, &r.emergency, &r.paused, &r.project, &r.projectSlug, &r.projectName,
		&r.projectDescription, &r.environment, &r.environmentSlug, &r.environmentName, &r.protected, &r.envType,
		&r.quota, &r.namespace, &r.cluster, &r.delivery, &r.application, &r.applicationSlug, &r.applicationName,
		&r.target, &r.desiredGeneration, &r.deleting, &r.release, &r.artifacts, &r.source, &r.configRevision,
		&r.configRevisionNumber, &r.config)
	return r, err
}

// artifactsOf reads a release's `artifacts` (process → digest).
func artifactsOf(text string) (map[string]artifact.Digest, error) {
	const op = "read a materialization"
	var byProcess map[string]string
	if err := json.Unmarshal([]byte(text), &byProcess); err != nil {
		return nil, decodeErr(op, "%v", err)
	}
	out := make(map[string]artifact.Digest, len(byProcess))
	for process, d := range byProcess {
		digest, err := artifact.ParseDigest(d)
		if err != nil {
			return nil, decodeErr(op, "%v", err)
		}
		out[process] = digest
	}
	return out, nil
}

// imageRepository is `source.image_repository` when it is a string.
func imageRepository(source *string) (opt.Val[string], error) {
	const op = "read a materialization"
	if source == nil {
		return opt.None[string](), nil
	}
	v, err := jsonValue(op, *source)
	if err != nil {
		return opt.None[string](), err
	}
	if m, ok := v.(map[string]any); ok {
		if repo, ok := m["image_repository"].(string); ok {
			return opt.Some(repo), nil
		}
	}
	return opt.None[string](), nil
}

func (r materializationRow) materialization(org ids.OrgID, operation ids.OperationID) (Materialization, error) {
	const op = "read a materialization"
	artifacts, err := artifactsOf(r.artifacts)
	if err != nil {
		return Materialization{}, err
	}
	repository, err := imageRepository(r.source)
	if err != nil {
		return Materialization{}, err
	}
	phase, err := parsePhase(op, r.phase)
	if err != nil {
		return Materialization{}, err
	}
	generation, err := counter(op, r.generation)
	if err != nil {
		return Materialization{}, err
	}
	quota, err := jsonColumn(r.quota)
	if err != nil {
		return Materialization{}, err
	}
	delivery, err := parseDelivery(op, r.delivery)
	if err != nil {
		return Materialization{}, err
	}
	desired, err := counter(op, r.desiredGeneration)
	if err != nil {
		return Materialization{}, err
	}
	number, err := counter(op, r.configRevisionNumber)
	if err != nil {
		return Materialization{}, err
	}
	config, err := jsonValue(op, r.config)
	if err != nil {
		return Materialization{}, err
	}
	return Materialization{
		Org: org, Run: r.run, Operation: operation, Phase: phase, Generation: target.Generation(generation),
		LifecycleUID: r.lifecycleUID, Project: r.project, ProjectSlug: r.projectSlug, ProjectName: r.projectName,
		ProjectDescription: opt.FromPtr(r.projectDescription), Environment: r.environment,
		EnvironmentSlug: r.environmentSlug, EnvironmentName: r.environmentName, Protected: r.protected,
		EnvType: r.envType, Quota: quota, Namespace: r.namespace, Cluster: r.cluster, Delivery: delivery,
		Application: r.application, ApplicationSlug: r.applicationSlug, ApplicationName: r.applicationName,
		Target: r.target, DesiredGeneration: target.Generation(desired), Deleting: r.deleting, Release: r.release,
		Artifacts: artifacts, ImageRepository: repository, ConfigRevision: r.configRevision,
		ConfigRevisionNumber: number, Config: config, RenderPlan: opt.FromPtr(r.renderPlan),
		RestartedAt: opt.FromPtr(r.restartedAt), ApprovalExpiresAt: opt.FromPtr(r.approvalExpiresAt),
		Secrets: []SecretBinding{}, Emergency: r.emergency, Paused: r.paused,
	}, nil
}

func scanMaterialized(row pgx.CollectableRow) (Materialized, error) {
	const op = "read a materialization record"
	var m Materialized
	var org string
	var generation, driftCount int64
	var detected *int64
	var drift *string
	if err := row.Scan(&m.Target, &org, &m.Project, &generation, &m.Operation, &m.ResourceUID,
		&m.ResourceGeneration, &driftCount, &detected, &drift); err != nil {
		return Materialized{}, err
	}
	var err error
	if m.Org, err = orgID(op, org); err != nil {
		return Materialized{}, err
	}
	g, err := counter(op, generation)
	if err != nil {
		return Materialized{}, err
	}
	if m.DriftCount, err = counter(op, driftCount); err != nil {
		return Materialized{}, err
	}
	if m.Drift, err = jsonColumn(drift); err != nil {
		return Materialized{}, err
	}
	m.Generation = target.Generation(g)
	m.DriftDetectedAt = opt.FromPtr(detected)
	return m, nil
}

// Materialization is the deployment run of operation with everything it is
// rendered from; false when the organization has no such run.
func (t *Tenant) Materialization(ctx context.Context, operation ids.OperationID) (Materialization, bool, error) {
	row, ok, err := queryOpt(ctx, t.tx, "read a materialization", selectMaterialization, scanMaterialization,
		operation, t.org.String())
	if err != nil || !ok {
		return Materialization{}, false, err
	}
	m, err := row.materialization(t.org, operation)
	if err != nil {
		return Materialization{}, false, err
	}
	if m.Secrets, err = t.RunSecretBindings(ctx, m.Run); err != nil {
		return Materialization{}, false, err
	}
	return m, true, nil
}

// Materialized is what the materializer last wrote for tgt, if anything.
func (t *Tenant) Materialized(ctx context.Context, tgt ids.TargetID) (Materialized, bool, error) {
	return queryOpt(ctx, t.tx, "read a materialization record", materializedTarget, scanMaterialized,
		tgt, t.org.String())
}

// FreezeRunPlan freezes runID's RenderPlan for the holder of claim
// (ADR-026): the plan the run has already, or this one, set once and never
// replaced. False when the claim was fenced off or the run is gone; commit
// only when true.
func (t *Tenant) FreezeRunPlan(
	ctx context.Context, claim Claim, runID ids.DeploymentRunID, rendererVersion string, capabilitySnapshot, resources any,
) (ids.RenderPlanID, bool, error) {
	const op = "freeze a run's plan"
	org := t.org.String()
	existing, found, err := queryOpt(ctx, t.tx, op, runPlanID, pgx.RowTo[*ids.RenderPlanID], runID, org)
	if err != nil || !found {
		return ids.RenderPlanID{}, false, err
	}
	if existing != nil {
		return *existing, true, nil
	}
	plan, err := t.FreezeRenderPlan(ctx, rendererVersion, capabilitySnapshot, resources)
	if err != nil {
		return ids.RenderPlanID{}, false, err
	}
	n, err := exec(ctx, t.tx, op, setRunPlan, runID, org, plan, claim.ID, claim.Fence)
	if err != nil || n != 1 {
		return ids.RenderPlanID{}, false, err
	}
	return plan, true, nil
}

// RunRenderPlan is the RenderPlan frozen for runID, if any.
func (t *Tenant) RunRenderPlan(ctx context.Context, runID ids.DeploymentRunID) (RunPlan, bool, error) {
	const op = "read a run's plan"
	return queryOpt(ctx, t.tx, op, runPlan, func(row pgx.CollectableRow) (RunPlan, error) {
		var p RunPlan
		var snapshot, resources string
		if err := row.Scan(&p.ID, &p.RendererVersion, &snapshot, &resources); err != nil {
			return RunPlan{}, err
		}
		var err error
		if p.CapabilitySnapshot, err = jsonValue(op, snapshot); err != nil {
			return RunPlan{}, err
		}
		if p.Resources, err = jsonValue(op, resources); err != nil {
			return RunPlan{}, err
		}
		return p, nil
	}, runID, t.org.String())
}

// RecordMaterialization records that the holder of claim wrote m's target
// as the App object resourceUID, now at `metadata.generation`
// resourceGeneration. False when the claim was fenced off or a newer
// generation is recorded.
func (s *Store) RecordMaterialization(
	ctx context.Context, claim Claim, m Materialization, resourceUID string, resourceGeneration int64,
) (bool, error) {
	const op = "record a materialization"
	generation, err := signed(op, uint64(m.Generation))
	if err != nil {
		return false, err
	}
	n, err := exec(ctx, s.db, op, recordMaterialization,
		m.Target, m.Org.String(), m.Project, generation, resourceUID, resourceGeneration, claim.ID, claim.Fence)
	return n == 1, err
}

// MaterializedResource is the target whose App object has the Kubernetes
// UID resourceUID.
func (s *Store) MaterializedResource(ctx context.Context, resourceUID string) (Materialized, bool, error) {
	return queryOpt(ctx, s.db, "read a materialization record", materializedResource, scanMaterialized, resourceUID)
}

// RecordDrift records drift found on the App object of m, and the object
// that replaced it, if one did. False when the target has moved on to
// another generation since m was read.
func (s *Store) RecordDrift(ctx context.Context, m Materialized, replaced opt.Val[Replacement], drift any) (bool, error) {
	const op = "record drift"
	generation, err := signed(op, uint64(m.Generation))
	if err != nil {
		return false, err
	}
	text, err := canonical(op, drift)
	if err != nil {
		return false, err
	}
	uid, resourceGeneration := opt.None[string](), opt.None[int64]()
	if r, ok := replaced.Get(); ok {
		uid, resourceGeneration = opt.Some(r.UID), opt.Some(r.Generation)
	}
	n, err := exec(ctx, s.db, op, recordDrift, m.Target, generation, uid.Ptr(), resourceGeneration.Ptr(), text)
	return n == 1, err
}

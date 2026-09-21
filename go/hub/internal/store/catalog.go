package store

// Read models of the SQL-backed routes (ADR-032, M1.8b step 2): the live
// environments and apps of an organization, as the API lists them; the port
// of repo/catalog.rs.
//
// Deleted rows are never listed; rows being deleted are, marked. An app is a
// target: one application on the environment's placement. Its desired state
// is its newest configuration revision and the release of its newest
// deployment run.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
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

const (
	selectApps = "SELECT t.id AS target_id, a.id AS application_id, a.slug, a.name, pl.namespace, " +
		"t.legacy_uid, t.deleting, t.lifecycle_uid, t.desired_generation, t.created_at, t.paused_at, t.pause_reason, " +
		"c.id AS config_revision_id, c.config::text AS config, r.id AS release_id, " +
		"COALESCE(r.source ->> 'image', (r.source ->> 'image_repository') || '@' || (r.artifacts ->> 'web')) AS image, " +
		"t.delivery, o.generation AS observed_generation, o.phase AS observed_phase, o.reason AS observed_reason, " +
		"o.message AS observed_message, o.observed_at, rp.host AS observed_host, rp.tls AS observed_tls " +
		"FROM application_targets t " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"LEFT JOIN LATERAL (SELECT id, config FROM target_config_revisions " +
		"WHERE target_id = t.id ORDER BY revision DESC LIMIT 1) c ON TRUE " +
		"LEFT JOIN LATERAL (SELECT rel.id, rel.source, rel.artifacts FROM deployment_runs d " +
		"JOIN releases rel ON rel.id = d.release_id " +
		"WHERE d.target_id = t.id ORDER BY d.generation DESC LIMIT 1) r ON TRUE " +
		"LEFT JOIN runtime_observations o ON o.target_id = t.id AND o.org_id = t.org_id " +
		"LEFT JOIN LATERAL (SELECT jsonb_path_query_first(p.resources, " +
		"'$[*] ? (@.kind == \"HTTPRoute\").spec.hostnames[0]') #>> '{}' AS host, " +
		"p.capability_snapshot ->> 'clusterIssuer' IS NOT NULL AS tls " +
		"FROM deployment_runs d " +
		"JOIN render_plans p ON p.id = d.render_plan_id AND p.org_id = d.org_id " +
		"WHERE d.target_id = t.id AND d.generation = o.generation) rp ON TRUE " +
		"WHERE t.org_id = $1 AND pl.environment_id = $2 AND t.deleted_at IS NULL AND a.deleted_at IS NULL " +
		"AND ($3::text IS NULL OR a.slug = $3) " +
		"ORDER BY a.slug"
	selectRuns = "SELECT d.id, d.generation, d.reason, d.phase, d.outcome, d.requested_by, d.created_at, " +
		"d.release_id, d.config_revision_id, " +
		"COALESCE(rel.source ->> 'image', (rel.source ->> 'image_repository') || '@' || (rel.artifacts ->> 'web')) AS image " +
		"FROM deployment_runs d JOIN releases rel ON rel.id = d.release_id AND rel.org_id = d.org_id " +
		"WHERE d.target_id = $1 AND d.org_id = $2 " +
		"ORDER BY d.generation DESC LIMIT $3"
	selectRunPhases = "SELECT p.run_id, p.phase, p.entered_at FROM deployment_run_phases p " +
		"JOIN deployment_runs d ON d.id = p.run_id AND d.org_id = p.org_id " +
		"WHERE d.target_id = $1 AND p.org_id = $2 AND p.run_id = ANY($3) " +
		"ORDER BY p.run_id, p.seq"
	selectConfigRevision = "SELECT config::text FROM target_config_revisions " +
		"WHERE id = $1 AND target_id = $2 AND org_id = $3"
	selectDomains = "SELECT pl.namespace, a.slug, d.value ->> 'host' AS host " +
		"FROM application_targets t " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"JOIN LATERAL (SELECT config FROM target_config_revisions " +
		"WHERE target_id = t.id ORDER BY revision DESC LIMIT 1) c ON TRUE " +
		"CROSS JOIN LATERAL jsonb_array_elements(COALESCE(c.config -> 'domains', '[]'::jsonb)) AS d(value) " +
		"WHERE t.org_id = $1 AND t.deleted_at IS NULL"
	selectApplication = "SELECT id FROM applications " +
		"WHERE org_id = $1 AND project_id = $2 AND slug = $3 AND deleted_at IS NULL"
)

// Pause is why and since when a target's delivery is paused (M4.9).
type Pause struct {
	At     int64
	Reason string
}

// AppRecord is a live app: one application on an environment's placement.
type AppRecord struct {
	Target            ids.TargetID
	Application       ids.ApplicationID
	Slug              string
	Name              string
	Namespace         string
	LegacyUID         opt.Val[uuid.UUID]
	Deleting          bool
	LifecycleUID      uuid.UUID
	DesiredGeneration target.Generation
	CreatedAt         int64
	// Paused is set while delivery is paused (M4.9).
	Paused opt.Val[Pause]
	// ConfigRevision and Config are the newest configuration revision: the
	// App spec without its image.
	ConfigRevision opt.Val[ids.ConfigRevisionID]
	Config         opt.Val[any]
	// Release is the release of the newest run, and Image its image as it
	// was given (a tag the digest was resolved from), else
	// `repository@digest`.
	Release opt.Val[ids.ReleaseID]
	Image   opt.Val[string]
	// Delivery is how the target's runs reach its cluster.
	Delivery Delivery
	// Runtime is what the target's agent last reported, once it reported
	// (M1.9).
	Runtime opt.Val[RuntimeStatus]
}

// RuntimeStatus is an agent-delivered target as its agent last saw it in
// the cluster (ADR-027): the observation, and the public URL of the plan it
// observed.
type RuntimeStatus struct {
	Generation target.Generation
	// Phase is `accepted`, `applying`, `ready`, `failed`, `rejected` or
	// `unknown`.
	Phase   string
	Reason  opt.Val[string]
	Message opt.Val[string]
	// ObservedAt is unix ms.
	ObservedAt int64
	// URL is the first hostname of the observed plan's route, over https
	// when the plan's cluster issues certificates: what the App controller
	// would report for the same plan.
	URL opt.Val[string]
}

// Ready reports whether the observed generation is rolled out and ready.
func (r RuntimeStatus) Ready() bool { return r.Phase == "ready" }

// RunRecord is one deployment run of an app, as its release history shows
// it.
type RunRecord struct {
	Run        ids.DeploymentRunID
	Generation target.Generation
	// Reason is `deploy`, `rollback` or `promotion`.
	Reason string
	Phase  run.Phase
	// Outcome is how the run ended, written once: `succeeded`, `failed` or
	// `cancelled`; absent while it runs, or when a newer run superseded it
	// first. A run superseded after it failed keeps `failed`.
	Outcome        opt.Val[string]
	RequestedBy    string
	CreatedAt      int64
	Release        ids.ReleaseID
	ConfigRevision ids.ConfigRevisionID
	// Image is the release's image as it was given, else
	// `repository@digest`.
	Image opt.Val[string]
}

// PhaseEntry is a phase a run entered and when (unix ms).
type PhaseEntry struct {
	Phase string
	At    int64
}

// AppDomain is a hostname of a live app, from its newest configuration.
type AppDomain struct {
	Namespace string
	App       string
	Host      string
}

type appRow struct {
	targetID                                   ids.TargetID
	applicationID                              ids.ApplicationID
	slug, name, namespace                      string
	legacyUID                                  *uuid.UUID
	deleting                                   bool
	lifecycleUID                               uuid.UUID
	desiredGeneration, createdAt               int64
	pausedAt                                   *int64
	pauseReason                                *string
	configRevisionID                           *ids.ConfigRevisionID
	config                                     *string
	releaseID                                  *ids.ReleaseID
	image                                      *string
	delivery                                   string
	observedGeneration                         *int64
	observedPhase, observedReason, observedMsg *string
	observedAt                                 *int64
	observedHost                               *string
	observedTLS                                *bool
}

func scanApp(row pgx.CollectableRow) (AppRecord, error) {
	const op = "list apps"
	var r appRow
	if err := row.Scan(&r.targetID, &r.applicationID, &r.slug, &r.name, &r.namespace, &r.legacyUID, &r.deleting,
		&r.lifecycleUID, &r.desiredGeneration, &r.createdAt, &r.pausedAt, &r.pauseReason, &r.configRevisionID,
		&r.config, &r.releaseID, &r.image, &r.delivery, &r.observedGeneration, &r.observedPhase,
		&r.observedReason, &r.observedMsg, &r.observedAt, &r.observedHost, &r.observedTLS); err != nil {
		return AppRecord{}, err
	}
	generation, err := counter(op, r.desiredGeneration)
	if err != nil {
		return AppRecord{}, err
	}
	config, err := jsonColumn(r.config)
	if err != nil {
		return AppRecord{}, err
	}
	delivery, err := parseDelivery(op, r.delivery)
	if err != nil {
		return AppRecord{}, err
	}
	a := AppRecord{
		Target:            r.targetID,
		Application:       r.applicationID,
		Slug:              r.slug,
		Name:              r.name,
		Namespace:         r.namespace,
		LegacyUID:         opt.FromPtr(r.legacyUID),
		Deleting:          r.deleting,
		LifecycleUID:      r.lifecycleUID,
		DesiredGeneration: target.Generation(generation),
		CreatedAt:         r.createdAt,
		ConfigRevision:    opt.FromPtr(r.configRevisionID),
		Config:            config,
		Release:           opt.FromPtr(r.releaseID),
		Image:             opt.FromPtr(r.image),
		Delivery:          delivery,
	}
	if r.pausedAt != nil {
		reason := ""
		if r.pauseReason != nil {
			reason = *r.pauseReason
		}
		a.Paused = opt.Some(Pause{At: *r.pausedAt, Reason: reason})
	}
	if a.Runtime, err = r.runtime(); err != nil {
		return AppRecord{}, err
	}
	return a, nil
}

// runtime is the agent's observation, when it reported one.
func (r appRow) runtime() (opt.Val[RuntimeStatus], error) {
	const op = "list apps"
	if r.observedGeneration == nil || r.observedPhase == nil || r.observedAt == nil {
		return opt.None[RuntimeStatus](), nil
	}
	generation, err := counter(op, *r.observedGeneration)
	if err != nil {
		return opt.None[RuntimeStatus](), err
	}
	status := RuntimeStatus{
		Generation: target.Generation(generation),
		Phase:      *r.observedPhase,
		Reason:     opt.FromPtr(r.observedReason),
		Message:    opt.FromPtr(r.observedMsg),
		ObservedAt: *r.observedAt,
	}
	if r.observedHost != nil {
		scheme := "http"
		if r.observedTLS != nil && *r.observedTLS {
			scheme = "https"
		}
		status.URL = opt.Some(scheme + "://" + *r.observedHost)
	}
	return opt.Some(status), nil
}

func scanRun(row pgx.CollectableRow) (RunRecord, error) {
	const op = "list runs"
	var r RunRecord
	var generation int64
	var phase string
	var outcome, image *string
	if err := row.Scan(&r.Run, &generation, &r.Reason, &phase, &outcome, &r.RequestedBy, &r.CreatedAt,
		&r.Release, &r.ConfigRevision, &image); err != nil {
		return RunRecord{}, err
	}
	g, err := counter(op, generation)
	if err != nil {
		return RunRecord{}, err
	}
	if r.Phase, err = parsePhase(op, phase); err != nil {
		return RunRecord{}, err
	}
	r.Generation = target.Generation(g)
	r.Outcome = opt.FromPtr(outcome)
	r.Image = opt.FromPtr(image)
	return r, nil
}

// parsePhase reads a stored run phase; anything else is a decode error.
func parsePhase(op, value string) (run.Phase, error) {
	p, err := run.ParsePhase(value)
	if err != nil {
		return "", decodeErr(op, "unknown run phase %s", rustQuote(value))
	}
	return p, nil
}

// Runs is the newest limit deployment runs of tgt, newest first.
func (t *Tenant) Runs(ctx context.Context, tgt ids.TargetID, limit int64) ([]RunRecord, error) {
	return queryAll(ctx, t.tx, "list runs", selectRuns, scanRun, tgt, t.org.String(), limit)
}

// RunPhases is the phases each of runs of tgt entered, in order, with the
// time (unix ms) it entered them.
func (t *Tenant) RunPhases(ctx context.Context, tgt ids.TargetID, runs []ids.DeploymentRunID) (map[ids.DeploymentRunID][]PhaseEntry, error) {
	type phaseRow struct {
		run ids.DeploymentRunID
		PhaseEntry
	}
	list := make([]uuid.UUID, 0, len(runs))
	for _, r := range runs {
		list = append(list, r.UUID())
	}
	rows, err := queryAll(ctx, t.tx, "list run phases", selectRunPhases, func(row pgx.CollectableRow) (phaseRow, error) {
		var r phaseRow
		err := row.Scan(&r.run, &r.Phase, &r.At)
		return r, err
	}, tgt, t.org.String(), list)
	if err != nil {
		return nil, err
	}
	out := make(map[ids.DeploymentRunID][]PhaseEntry)
	for _, r := range rows {
		out[r.run] = append(out[r.run], r.PhaseEntry)
	}
	return out, nil
}

// ConfigRevision is the content of configuration revision revision of tgt;
// false when tgt has no such revision.
func (t *Tenant) ConfigRevision(ctx context.Context, tgt ids.TargetID, revision ids.ConfigRevisionID) (any, bool, error) {
	text, ok, err := queryOpt(ctx, t.tx, "read a configuration revision", selectConfigRevision, pgx.RowTo[string],
		revision, tgt, t.org.String())
	if err != nil || !ok {
		return nil, false, err
	}
	v, err := jsonColumn(&text)
	if err != nil {
		return nil, false, err
	}
	config, _ := v.Get()
	return config, true, nil
}

// Domains is the hostnames of every live app of the organization, from its
// newest configuration.
func (t *Tenant) Domains(ctx context.Context) ([]AppDomain, error) {
	return queryAll(ctx, t.tx, "list domains", selectDomains, func(row pgx.CollectableRow) (AppDomain, error) {
		var d AppDomain
		err := row.Scan(&d.Namespace, &d.App, &d.Host)
		return d, err
	}, t.org.String())
}

// Application is the live application slug of project: every
// environment's app of that name belongs to it.
func (t *Tenant) Application(ctx context.Context, project ids.ProjectID, slug string) (ids.ApplicationID, bool, error) {
	return queryOpt(ctx, t.tx, "find an application", selectApplication, scanID[ids.Application],
		t.org.String(), project, slug)
}

func (t *Tenant) appRows(ctx context.Context, environment ids.EnvironmentID, slug opt.Val[string]) ([]AppRecord, error) {
	return queryAll(ctx, t.tx, "list apps", selectApps, scanApp, t.org.String(), environment, slug.Ptr())
}

// Apps is the live apps of environment, ordered by slug.
func (t *Tenant) Apps(ctx context.Context, environment ids.EnvironmentID) ([]AppRecord, error) {
	return t.appRows(ctx, environment, opt.None[string]())
}

// App is the live app slug of environment.
func (t *Tenant) App(ctx context.Context, environment ids.EnvironmentID, slug string) (AppRecord, bool, error) {
	return last(t.appRows(ctx, environment, opt.Some(slug)))
}

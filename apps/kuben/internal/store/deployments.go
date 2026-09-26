package store

// Releases, target configuration revisions, render plans and deployment runs
// (ADR-026; plan §8.1, §8.5, §8.7, §8.9); the port of repo/deployments.rs.
//
// [Tenant.StartDeployment] is the transactional acceptance of plan §8.2. It
// locks the target, answers a replayed `Idempotency-Key` with the first
// receipt, decides with the pure rules of [target.State], and only then
// writes the operation, the raised generation, the superseding of older runs
// and the new run, all in the caller's transaction.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
)

// RunKind is the operation kind of an accepted deployment run.
const RunKind = "deployment"

// runTopic is the outbox topic of an accepted deployment run.
const runTopic = "deployment.accepted"

const (
	insertRelease = "INSERT INTO releases " +
		"(id, org_id, project_id, application_id, artifacts, process_contract, portable_config, " +
		"renderer_schema, source, content_hash, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7::jsonb, $8, $9::jsonb, " +
		"sha256(convert_to($10, 'UTF8')), $11, $12) " +
		"ON CONFLICT (application_id, content_hash) DO NOTHING " +
		"RETURNING id"
	selectReleaseByContent = "SELECT id FROM releases " +
		"WHERE org_id = $1 AND project_id = $2 AND application_id = $3 " +
		"AND content_hash = sha256(convert_to($4, 'UTF8'))"
	nextConfigRevision = "UPDATE application_targets SET config_revision_seq = config_revision_seq + 1 " +
		"WHERE id = $1 AND org_id = $2 AND project_id = $3 RETURNING config_revision_seq"
	insertConfigRevision = "INSERT INTO target_config_revisions " +
		"(id, org_id, project_id, target_id, revision, config, config_hash, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6::jsonb, sha256(convert_to($7, 'UTF8')), $8, $9)"
	insertRenderPlan = "INSERT INTO render_plans " +
		"(id, org_id, content_digest, renderer_version, capability_snapshot, resources, created_at) " +
		"VALUES ($1, $2, sha256(convert_to($3, 'UTF8')), $4, $5::jsonb, $6::jsonb, $7) " +
		"ON CONFLICT (org_id, content_digest) DO NOTHING " +
		"RETURNING id"
	selectRenderPlanByContent = "SELECT id FROM render_plans " +
		"WHERE org_id = $1 AND content_digest = sha256(convert_to($2, 'UTF8'))"
	lockTarget = "SELECT application_id, lifecycle_uid, deleting, desired_generation, source_epoch, " +
		"build_config_revision, deploy_policy FROM application_targets " +
		"WHERE id = $1 AND org_id = $2 AND project_id = $3 " +
		"FOR UPDATE"
	liveReceipt = "SELECT request_hash, operation_id FROM idempotency_receipts " +
		"WHERE org_id = $1 AND actor = $2 AND operation = $3 AND key = $4 AND expires_at > $5"
	releaseOfApplication = "SELECT EXISTS (SELECT 1 FROM releases " +
		"WHERE id = $1 AND org_id = $2 AND project_id = $3 AND application_id = $4)"
	revisionOfTarget = "SELECT EXISTS (SELECT 1 FROM target_config_revisions " +
		"WHERE id = $1 AND org_id = $2 AND target_id = $3)"
	planOfOrg       = "SELECT EXISTS (SELECT 1 FROM render_plans WHERE id = $1 AND org_id = $2)"
	raiseGeneration = "UPDATE application_targets SET desired_generation = $3, deploy_policy = $4 " +
		"WHERE id = $1 AND desired_generation = $2"
	supersedeOlder = "UPDATE deployment_runs SET phase = 'superseded', updated_at = $3 " +
		"WHERE target_id = $1 AND generation < $2 AND phase <> ALL($4) RETURNING operation_id"
	// wakeSuperseded settles superseded runs at once, even when parked for
	// an approval.
	wakeSuperseded = "UPDATE operations SET next_attempt_at = kuben_now_ms() " +
		"WHERE id = ANY($1) AND NOT done AND next_attempt_at > kuben_now_ms()"
	// insertRun is the Rust INSERT_RUN with casts on $1, $5, $6, $7, $9,
	// $10 and $15 where they are inserted: PostgreSQL infers a parameter's
	// type from its first use, which for these is a `::text` cast or
	// `$15 > 0` (integer, not the smallint column); sqlx sent the types, pgx
	// lets the server infer them. The casts are to the types Rust bound.
	insertRun = "INSERT INTO deployment_runs " +
		"(id, org_id, project_id, application_id, target_id, release_id, config_revision_id, render_plan_id, " +
		"generation, lifecycle_uid, reason, requested_by, operation_id, created_at, updated_at, restarted_at, " +
		"approvals_required, approval_expires_at, policy_revision, approval_plan_hash, emergency_reason) " +
		"VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6::uuid, $7::uuid, $8, $9::bigint, $10::uuid, $11, $12, $13, " +
		"$14, $14, " +
		"CASE WHEN $11 = 'restart' THEN $14 ELSE (SELECT d.restarted_at FROM deployment_runs d " +
		"WHERE d.target_id = $5 AND d.org_id = $2 ORDER BY d.generation DESC LIMIT 1) END, " +
		"$15::smallint, $16, $17, " +
		"CASE WHEN $15 > 0 THEN sha256(convert_to(concat_ws('/', $1::text, $5::text, $6::text, $7::text, " +
		"$9::text, $10::text, $11::text), 'UTF8')) END, $18)"
	awaitApproval     = "UPDATE deployment_runs SET phase = $2, updated_at = $3 WHERE id = $1 AND phase = $4"
	parkOperation     = "UPDATE operations SET next_attempt_at = $2 WHERE id = $1 AND NOT done"
	selectRun         = "SELECT phase, generation FROM deployment_runs WHERE id = $1 AND org_id = $2"
	lockRunUnderFence = "SELECT r.phase FROM deployment_runs r " +
		"JOIN operations o ON o.id = r.operation_id " +
		"WHERE r.id = $1 AND o.id = $2 AND o.fence = $3 AND NOT o.done " +
		"FOR UPDATE OF r"
	setRunPhase = "UPDATE deployment_runs SET phase = $2, updated_at = $3, " +
		"outcome = COALESCE(outcome, $4), recovery_outcome = COALESCE(recovery_outcome, $5) WHERE id = $1"
	latestConfigRevision = "SELECT id FROM target_config_revisions " +
		"WHERE target_id = $1 AND org_id = $2 ORDER BY revision DESC LIMIT 1"
	runOfTarget = "SELECT id, operation_id, generation, phase, approvals_required, " +
		"approval_expires_at, approval_plan_hash FROM deployment_runs " +
		"WHERE id = $1 AND target_id = $2 AND org_id = $3"
	runOfOperation = "SELECT id, operation_id, generation, phase, approvals_required, " +
		"approval_expires_at, approval_plan_hash FROM deployment_runs " +
		"WHERE operation_id = $1 AND org_id = $2"
)

// PortableRelease is the input for [Tenant.CreateRelease]: portable and
// immutable (I03). The JSON values are stored as serde_json wrote them;
// callers that must keep a float such as `1.0` byte-exact decode with
// json.Number.
type PortableRelease struct {
	Application ids.ApplicationID
	// Artifacts is process name → digest; at least one.
	Artifacts       map[string]artifact.Digest
	ProcessContract any
	// PortableConfig is non-secret configuration that travels with the
	// release.
	PortableConfig any
	RendererSchema int32
	// Source is the source and build provenance references.
	Source    opt.Val[any]
	CreatedBy string
}

// RunReason is why a run exists (plan §8.7).
type RunReason string

// The reasons, with their stored names.
const (
	ReasonDeploy RunReason = "deploy"
	// ReasonRollback runs an older release; it pins the target until
	// automatic deploys resume.
	ReasonRollback RunReason = "rollback"
	// ReasonPromotion runs the same release and digests on another target,
	// without a build.
	ReasonPromotion RunReason = "promotion"
	// ReasonRestart runs the same release and configuration with a new
	// restart stamp: every pod is replaced (an app its cluster's agent
	// delivers, M1.9).
	ReasonRestart RunReason = "restart"
	// ReasonHandover runs the same release and configuration, now through
	// the cluster's agent, which adopts the workloads the App controller made
	// (M1.9).
	ReasonHandover RunReason = "handover"
	// ReasonBuild deploys a verified build of the target's current source
	// head, by the compare-and-set of [target.State.TryAutodeploy] (M3).
	ReasonBuild RunReason = "build"
	// ReasonEmergency is a rollback by a person with a reason that passes
	// approvals, freezes, the scan gate and a pause (M4.9,
	// [Tenant.StartEmergencyRollback]).
	ReasonEmergency RunReason = "emergency"
	// ReasonRotation runs the same release and configuration with the
	// current revisions of the secrets it references (M4.4).
	ReasonRotation RunReason = "rotation"
)

func (r RunReason) String() string { return string(r) }

// ChangeKind is what the run changes, for the environment's approval rules.
func (r RunReason) ChangeKind() policy.ChangeKind {
	switch r {
	case ReasonDeploy:
		return policy.Deploy
	case ReasonRollback:
		return policy.Rollback
	case ReasonPromotion:
		return policy.Promotion
	case ReasonRestart:
		return policy.Restart
	case ReasonHandover:
		return policy.Handover
	case ReasonBuild:
		return policy.Build
	case ReasonRotation:
		return policy.Rotation
	case ReasonEmergency:
		return policy.Emergency
	}
	return policy.ChangeKind(r)
}

// CarriesNewCode reports whether the run may deliver a release the target
// does not run yet: what the scan gate judges. Restarts, handovers and
// rotations keep the release, and an emergency must not wait for a feed.
func (r RunReason) CarriesNewCode() bool {
	switch r {
	case ReasonRestart, ReasonHandover, ReasonRotation:
		return false
	case ReasonDeploy, ReasonRollback, ReasonPromotion, ReasonBuild, ReasonEmergency:
		return true
	}
	return true
}

// StartDeployment is the input for [Tenant.StartDeployment].
type StartDeployment struct {
	Project        ids.ProjectID
	Target         ids.TargetID
	Release        ids.ReleaseID
	ConfigRevision ids.ConfigRevisionID
	RenderPlan     opt.Val[ids.RenderPlanID]
	// ExpectedGeneration is the generation the caller saw: there is no
	// implicit last-writer-wins.
	ExpectedGeneration target.Generation
	// LifecycleUID is the lifecycle UID the caller saw: work for a recreated
	// target is refused.
	LifecycleUID uuid.UUID
	Reason       RunReason
	RequestedBy  string
	// InputHash is the hash of the canonical request, compared on
	// `Idempotency-Key` replays.
	InputHash []byte
}

// Started is the outcome of [Tenant.StartDeployment]. Only StartedAccepted
// wrote anything.
//
//sumtype:decl
type Started interface{ started() }

type (
	// StartedAccepted is an accepted run.
	StartedAccepted struct {
		Operation  ids.OperationID
		Run        ids.DeploymentRunID
		Generation target.Generation
		// ApprovalsRequired are the approvals the environment's policy
		// requires before delivery; the run waits in `awaitingApproval` when
		// this is not zero.
		ApprovalsRequired uint8
	}
	// StartedReplayed means the same key and request were accepted before.
	StartedReplayed struct{ Operation ids.OperationID }
	// StartedKeyReused means the key was used for another request.
	StartedKeyReused struct{ Operation ids.OperationID }
	// StartedRejected means the target moved on, is pinned, deleting or was
	// recreated.
	StartedRejected struct{ Reject target.Reject }
	// StartedNotFound means no such target, or the release, revision or plan is
	// not its own.
	StartedNotFound struct{}
	// StartedSecretRevoked means the current revision of a secret the
	// configuration references is revoked; a new value must be set first.
	StartedSecretRevoked struct{}
	// StartedVulnerabilityBlocked means the environment's scan gate refuses the
	// release (M4.6).
	StartedVulnerabilityBlocked struct{}
	// StartedFrozen means the environment is frozen (M4.9); only an emergency
	// rollback passes.
	StartedFrozen struct{}
	// StartedUntrusted means an untrusted preview (a fork's, M5.1) would bind a
	// secret or a registry login.
	StartedUntrusted struct{}
)

func (StartedAccepted) started()             {}
func (StartedReplayed) started()             {}
func (StartedKeyReused) started()            {}
func (StartedRejected) started()             {}
func (StartedNotFound) started()             {}
func (StartedSecretRevoked) started()        {}
func (StartedVulnerabilityBlocked) started() {}
func (StartedFrozen) started()               {}
func (StartedUntrusted) started()            {}

// Advance is the outcome of [Store.AdvanceRun].
//
//sumtype:decl
type Advance interface{ advance() }

type (
	// AdvanceMoved means the run is in Phase now.
	AdvanceMoved struct{ Phase run.Phase }
	// AdvanceIllegal means the event is not allowed in the current phase;
	// nothing changed.
	AdvanceIllegal struct{ Err *ops.IllegalTransitionError }
	// AdvanceFenced means the claim's fence moved on or the operation is settled:
	// stop.
	AdvanceFenced struct{}
)

func (AdvanceMoved) advance()   {}
func (AdvanceIllegal) advance() {}
func (AdvanceFenced) advance()  {}

// ConfigRevisionRecord is a recorded configuration revision and its number.
type ConfigRevisionRecord struct {
	ID       ids.ConfigRevisionID
	Revision uint64
}

// RunState is the phase and generation of a run.
type RunState struct {
	Phase      run.Phase
	Generation target.Generation
}

type lockedTarget struct {
	application  ids.ApplicationID
	lifecycleUID uuid.UUID
	deleting     bool
	generation   int64
	epoch        int64
	revision     int64
	policy       string
}

func (r lockedTarget) state() (target.State, error) {
	const op = "start a deployment"
	generation, err := counter(op, r.generation)
	if err != nil {
		return target.State{}, err
	}
	epoch, err := counter(op, r.epoch)
	if err != nil {
		return target.State{}, err
	}
	revision, err := counter(op, r.revision)
	if err != nil {
		return target.State{}, err
	}
	p, err := deployPolicy(op, r.policy)
	if err != nil {
		return target.State{}, err
	}
	return target.State{
		LifecycleUID:        r.lifecycleUID,
		Deleting:            r.deleting,
		DesiredGeneration:   target.Generation(generation),
		SourceEpoch:         target.SourceEpoch(epoch),
		BuildConfigRevision: revision,
		Policy:              p,
	}, nil
}

// releaseContent is the text a release is addressed by: serde_json's
// `json!({...}).to_string()` of its portable parts, keys sorted.
func releaseContent(release PortableRelease) (string, error) {
	artifacts := release.Artifacts
	if artifacts == nil {
		artifacts = map[string]artifact.Digest{}
	}
	return canonical("create a release", map[string]any{
		"artifacts":        artifacts,
		"process_contract": release.ProcessContract,
		"portable_config":  release.PortableConfig,
		"renderer_schema":  release.RendererSchema,
	})
}

// planContent is the text a render plan is addressed by.
func planContent(rendererVersion string, capabilitySnapshot, resources any) (string, error) {
	return canonical("freeze a render plan", map[string]any{
		"renderer_version":    rendererVersion,
		"capability_snapshot": capabilitySnapshot,
		"resources":           resources,
	})
}

// CreateRelease records release in project. It returns its id and true, or
// the id of an identical release of the same application and false.
func (t *Tenant) CreateRelease(ctx context.Context, project ids.ProjectID, release PortableRelease) (ids.ReleaseID, bool, error) {
	const op = "create a release"
	artifacts := release.Artifacts
	if artifacts == nil {
		artifacts = map[string]artifact.Digest{}
	}
	content, err := releaseContent(release)
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	artifactsText, err := canonical(op, artifacts)
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	contract, err := canonical(op, release.ProcessContract)
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	config, err := canonical(op, release.PortableConfig)
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	source, err := canonicalOpt(op, release.Source)
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	org := t.org.String()
	id, inserted, err := queryOpt(ctx, t.tx, op, insertRelease, scanID[ids.Release],
		ids.New[ids.Release](), org, project, release.Application, artifactsText, contract, config,
		release.RendererSchema, source.Ptr(), content, release.CreatedBy, t.store.now())
	if err != nil {
		return ids.ReleaseID{}, false, err
	}
	if inserted {
		return id, true, nil
	}
	var existing ids.ReleaseID
	if err := queryOne(ctx, t.tx, op, selectReleaseByContent, []any{&existing},
		org, project, release.Application, content); err != nil {
		return ids.ReleaseID{}, false, err
	}
	return existing, false, nil
}

// CreateConfigRevision records the next configuration revision of tgt;
// false when the organization has no such target. config is stored as
// serde_json wrote it.
func (t *Tenant) CreateConfigRevision(
	ctx context.Context, project ids.ProjectID, tgt ids.TargetID, config any, createdBy string,
) (ConfigRevisionRecord, bool, error) {
	const op = "create a configuration revision"
	org := t.org.String()
	text, err := canonical(op, config)
	if err != nil {
		return ConfigRevisionRecord{}, false, err
	}
	revision, ok, err := queryOpt(ctx, t.tx, op, nextConfigRevision, pgx.RowTo[int64], tgt, org, project)
	if err != nil || !ok {
		return ConfigRevisionRecord{}, false, err
	}
	id := ids.New[ids.ConfigRevision]()
	if _, err := exec(ctx, t.tx, op, insertConfigRevision,
		id, org, project, tgt, revision, text, text, createdBy, t.store.now()); err != nil {
		return ConfigRevisionRecord{}, false, err
	}
	n, err := counter(op, revision)
	if err != nil {
		return ConfigRevisionRecord{}, false, err
	}
	return ConfigRevisionRecord{ID: id, Revision: n}, true, nil
}

// FreezeRenderPlan freezes a render plan, addressed by its content: the
// same content in the same organization is the same plan.
func (t *Tenant) FreezeRenderPlan(ctx context.Context, rendererVersion string, capabilitySnapshot, resources any) (ids.RenderPlanID, error) {
	const op = "freeze a render plan"
	content, err := planContent(rendererVersion, capabilitySnapshot, resources)
	if err != nil {
		return ids.RenderPlanID{}, err
	}
	snapshot, err := canonical(op, capabilitySnapshot)
	if err != nil {
		return ids.RenderPlanID{}, err
	}
	resourcesText, err := canonical(op, resources)
	if err != nil {
		return ids.RenderPlanID{}, err
	}
	org := t.org.String()
	id, inserted, err := queryOpt(ctx, t.tx, op, insertRenderPlan, scanID[ids.RenderPlan],
		ids.New[ids.RenderPlan](), org, content, rendererVersion, snapshot, resourcesText, t.store.now())
	if err != nil {
		return ids.RenderPlanID{}, err
	}
	if inserted {
		return id, nil
	}
	if err := queryOne(ctx, t.tx, op, selectRenderPlanByContent, []any{&id}, org, content); err != nil {
		return ids.RenderPlanID{}, err
	}
	return id, nil
}

// decider is the rule of [target.State] a start applies.
type decider func(state *target.State, lifecycleUID uuid.UUID, expected target.Generation) (target.Generation, target.Reject)

// StartDeployment accepts a deployment run (plan §8.2): nothing is written
// unless the result is [StartedAccepted], and then the operation, its audit
// record and outbox message, the raised generation, the superseding of
// older runs and the run commit together with this transaction.
func (t *Tenant) StartDeployment(ctx context.Context, req StartDeployment, audit NewAudit, idempotency opt.Val[IdempotencyKey]) (Started, error) {
	if req.Reason == ReasonEmergency {
		return nil, protocolErr("start a deployment", "an emergency rollback needs its reason")
	}
	reason := req.Reason
	return t.startWith(ctx, req, audit, idempotency, opt.None[string](),
		func(state *target.State, lifecycleUID uuid.UUID, expected target.Generation) (target.Generation, target.Reject) {
			switch reason {
			case ReasonRollback, ReasonEmergency:
				return state.Rollback(lifecycleUID, expected)
			case ReasonDeploy, ReasonPromotion, ReasonRestart, ReasonHandover, ReasonBuild, ReasonRotation:
				return state.DeployExplicit(lifecycleUID, expected)
			}
			return state.DeployExplicit(lifecycleUID, expected)
		})
}

// StartEmergencyRollback accepts an emergency rollback to req.Release
// (M4.9): a person's break-glass that passes approvals, a freeze, the scan
// gate and a pause. It pins the target like any rollback. why is kept on the
// run.
func (t *Tenant) StartEmergencyRollback(ctx context.Context, req StartDeployment, why string, audit NewAudit) (Started, error) {
	req.Reason = ReasonEmergency
	return t.startWith(ctx, req, audit, opt.None[IdempotencyKey](), opt.Some(why),
		func(state *target.State, lifecycleUID uuid.UUID, expected target.Generation) (target.Generation, target.Reject) {
			return state.Rollback(lifecycleUID, expected)
		})
}

// StartBuildDeployment accepts the automatic deploy of a verified build
// (M3). The target must still be on sourceEpoch and buildConfigRevision and
// follow automatic deploys; otherwise the result is [StartedRejected] and
// nothing is written. req.ExpectedGeneration is the generation read under
// the same row lock, so only these checks decide.
func (t *Tenant) StartBuildDeployment(
	ctx context.Context, req StartDeployment, sourceEpoch target.SourceEpoch, buildConfigRevision uint64, audit NewAudit,
) (Started, error) {
	return t.startWith(ctx, req, audit, opt.None[IdempotencyKey](), opt.None[string](),
		func(state *target.State, lifecycleUID uuid.UUID, expected target.Generation) (target.Generation, target.Reject) {
			return state.TryAutodeploy(target.AutodeployRequest{
				LifecycleUID:        lifecycleUID,
				SourceEpoch:         sourceEpoch,
				BuildConfigRevision: buildConfigRevision,
				ExpectedGeneration:  expected,
			})
		})
}

func (t *Tenant) startWith(
	ctx context.Context, req StartDeployment, audit NewAudit, idempotency opt.Val[IdempotencyKey],
	emergency opt.Val[string], decide decider,
) (Started, error) {
	d, refused, err := t.decideRun(ctx, req, idempotency, emergency, decide)
	if err != nil || refused != nil {
		return refused, err
	}
	runID := ids.New[ids.DeploymentRun]()
	accepted, err := t.Accept(ctx, NewOperation{
		Kind:         RunKind,
		Target:       opt.Some(OperationTarget{Project: req.Project, Target: req.Target}),
		LifecycleUID: opt.Some(req.LifecycleUID),
		Generation:   opt.Some(uint64(d.generation)),
		InputHash:    req.InputHash,
		Payload: map[string]any{
			"run":             runID,
			"release":         req.Release,
			"config_revision": req.ConfigRevision,
			"render_plan":     req.RenderPlan,
			"reason":          req.Reason.String(),
		},
		RequestedBy: req.RequestedBy,
		Topic:       runTopic,
	}, audit, idempotency)
	if err != nil {
		return nil, err
	}
	var operation ids.OperationID
	switch a := accepted.(type) {
	case AcceptedNew:
		operation = a.ID
	case AcceptedReplayed:
		return StartedReplayed{Operation: a.ID}, nil
	case AcceptedKeyReused:
		return StartedKeyReused{Operation: a.ID}, nil
	}
	approvals, err := t.recordRun(ctx, newRun{
		req: req, application: d.application, run: runID, operation: operation, generation: d.generation,
		policy: d.policy, emergency: emergency,
	})
	if err != nil {
		return nil, err
	}
	if err := t.bindRunSecrets(ctx, runID, d.secrets); err != nil {
		return nil, err
	}
	return StartedAccepted{Operation: operation, Run: runID, Generation: d.generation, ApprovalsRequired: approvals}, nil
}

// decision is what the checks of a start decided: the run may be recorded.
type decision struct {
	application ids.ApplicationID
	generation  target.Generation
	policy      target.DeployPolicy
	secrets     []wanted
}

// decideRun locks the target, answers a replay, and applies the target's
// rules and the run's checks. It returns the decision, or why nothing is
// written.
func (t *Tenant) decideRun(
	ctx context.Context, req StartDeployment, idempotency opt.Val[IdempotencyKey], emergency opt.Val[string],
	decide decider,
) (decision, Started, error) {
	const op = "start a deployment"
	// The row lock serializes every decision about this target.
	row, ok, err := queryOpt(ctx, t.tx, op, lockTarget, func(row pgx.CollectableRow) (lockedTarget, error) {
		var r lockedTarget
		err := row.Scan(&r.application, &r.lifecycleUID, &r.deleting, &r.generation, &r.epoch, &r.revision, &r.policy)
		return r, err
	}, req.Target, t.org.String(), req.Project)
	if err != nil {
		return decision{}, nil, err
	}
	if !ok {
		return decision{}, StartedNotFound{}, nil
	}
	if replay, ok, err := t.liveReceipt(ctx, idempotency, req.InputHash); err != nil || ok {
		return decision{}, replay, err
	}
	owns, err := t.ownsInputs(ctx, req, row.application)
	if err != nil {
		return decision{}, nil, err
	}
	if !owns {
		return decision{}, StartedNotFound{}, nil
	}
	state, err := row.state()
	if err != nil {
		return decision{}, nil, err
	}
	generation, reject := decide(&state, req.LifecycleUID, req.ExpectedGeneration)
	if reject != nil {
		return decision{}, StartedRejected{Reject: reject}, nil //nolint:nilerr // a refusal is an answer, not a failure
	}
	secrets, refused, err := t.checkRun(ctx, req, emergency)
	if err != nil || refused != nil {
		return decision{}, refused, err
	}
	return decision{application: row.application, generation: generation, policy: state.Policy, secrets: secrets}, nil, nil
}

// checkRun applies the checks after the target's rules: revoked secrets, an
// untrusted preview, a freeze and the scan gate. It returns the secrets the
// run binds, or why it is refused.
func (t *Tenant) checkRun(ctx context.Context, req StartDeployment, emergency opt.Val[string]) ([]wanted, Started, error) {
	secrets, err := t.wantedSecrets(ctx, req.ConfigRevision, req.Target, req.Release)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range secrets {
		if s.revoked {
			return nil, StartedSecretRevoked{}, nil
		}
	}
	if len(secrets) > 0 {
		untrusted, err := t.UntrustedTarget(ctx, req.Target)
		if err != nil {
			return nil, nil, err
		}
		if untrusted {
			return nil, StartedUntrusted{}, nil
		}
	}
	if !req.Reason.CarriesNewCode() || emergency.IsSome() {
		return secrets, nil, nil
	}
	if _, frozen, err := t.ActiveFreeze(ctx, req.Target, t.store.now()); err != nil || frozen {
		if err != nil {
			return nil, nil, err
		}
		return nil, StartedFrozen{}, nil
	}
	verdict, ok, err := t.ScanVerdict(ctx, req.Target, req.Release, t.store.now())
	if err != nil {
		return nil, nil, err
	}
	if block, blocked := verdict.(scan.Block); ok && blocked {
		t.store.log.InfoContext(ctx, "the scan gate refused a run",
			slog.String("target", req.Target.String()), slog.String("release", req.Release.String()),
			slog.Any("reasons", block.Reasons))
		return nil, StartedVulnerabilityBlocked{}, nil
	}
	return secrets, nil, nil
}

// liveReceipt answers a replay with the first receipt, even if the
// generation it expected has moved on since.
func (t *Tenant) liveReceipt(ctx context.Context, idempotency opt.Val[IdempotencyKey], inputHash []byte) (Started, bool, error) {
	k, ok := idempotency.Get()
	if !ok {
		return nil, false, nil
	}
	type receipt struct {
		hash      []byte
		operation ids.OperationID
	}
	r, ok, err := queryOpt(ctx, t.tx, "read an idempotency receipt", liveReceipt,
		func(row pgx.CollectableRow) (receipt, error) {
			var r receipt
			err := row.Scan(&r.hash, &r.operation)
			return r, err
		}, t.org.String(), k.Actor, RunKind, k.Key, t.store.now())
	if err != nil || !ok {
		return nil, false, err
	}
	if bytes.Equal(r.hash, inputHash) {
		return StartedReplayed{Operation: r.operation}, true, nil
	}
	return StartedKeyReused{Operation: r.operation}, true, nil
}

// newRun is an accepted run to record.
type newRun struct {
	req         StartDeployment
	application ids.ApplicationID
	run         ids.DeploymentRunID
	operation   ids.OperationID
	generation  target.Generation
	policy      target.DeployPolicy
	emergency   opt.Val[string]
}

// recordRun raises the target's generation, supersedes its older unsettled
// runs and inserts the accepted run under the environment's policy: a run
// that needs approvals waits in `awaitingApproval`, its operation parked
// until the approval window closes (a decision wakes it). It returns the
// approvals required.
func (t *Tenant) recordRun(ctx context.Context, r newRun) (uint8, error) {
	const op = "record a deployment run"
	revision, hasPolicy, err := t.PolicyOfTarget(ctx, r.req.Target)
	if err != nil {
		return 0, err
	}
	var approvals uint8
	if hasPolicy {
		approvals = revision.Policy.ApprovalsFor(r.req.Reason.ChangeKind())
	}
	generation, err := t.raiseGeneration(ctx, r)
	if err != nil {
		return 0, err
	}
	now := t.store.now()
	expiresAt := opt.None[int64]()
	if hasPolicy && approvals > 0 {
		expiresAt = opt.Some(clock.SaturatingAdd(now, int64(revision.Policy.ApprovalTTLSecs)*1000))
	}
	if err := t.supersedeOlder(ctx, r.req.Target, generation, now); err != nil {
		return 0, err
	}
	policyRevision := opt.None[int64]()
	if hasPolicy {
		n, err := signed(op, revision.Revision)
		if err != nil {
			return 0, err
		}
		policyRevision = opt.Some(n)
	}
	if _, err := exec(ctx, t.tx, op, insertRun,
		r.run, t.org.String(), r.req.Project, r.application, r.req.Target, r.req.Release, r.req.ConfigRevision,
		r.req.RenderPlan.Ptr(), generation, r.req.LifecycleUID, r.req.Reason.String(), r.req.RequestedBy,
		r.operation, now, int16(approvals), expiresAt.Ptr(), policyRevision.Ptr(), r.emergency.Ptr()); err != nil {
		return 0, err
	}
	if at, ok := expiresAt.Get(); ok {
		if err := t.awaitApproval(ctx, r, at, now); err != nil {
			return 0, err
		}
	}
	return approvals, nil
}

// raiseGeneration moves the locked target from the generation the request
// expected to the run's, with the policy the decision left, and returns the
// run's generation as stored.
func (t *Tenant) raiseGeneration(ctx context.Context, r newRun) (int64, error) {
	const op = "raise a target's generation"
	expected, err := signed(op, uint64(r.req.ExpectedGeneration))
	if err != nil {
		return 0, err
	}
	generation, err := signed(op, uint64(r.generation))
	if err != nil {
		return 0, err
	}
	raised, err := exec(ctx, t.tx, op, raiseGeneration, r.req.Target, expected, generation, r.policy.String())
	if err != nil {
		return 0, err
	}
	if raised != 1 {
		// The row is locked by this transaction; anything else is a bug.
		return 0, dbErr(op, pgx.ErrNoRows)
	}
	return generation, nil
}

// awaitApproval moves a run that needs approvals to `awaitingApproval` and
// parks its operation until the approval window closes at.
func (t *Tenant) awaitApproval(ctx context.Context, r newRun, at, now int64) error {
	const op = "park a run for approval"
	waiting, err := run.Planned.Apply(run.EventRequireApproval)
	if err != nil {
		return protocolErr(op, "%v", err)
	}
	if _, err := exec(ctx, t.tx, op, awaitApproval, r.run, waiting.String(), now, run.Planned.String()); err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, parkOperation, r.operation, at)
	return err
}

// supersedeOlder supersedes the unsettled runs of tgt older than generation
// and wakes their operations.
func (t *Tenant) supersedeOlder(ctx context.Context, tgt ids.TargetID, generation, now int64) error {
	const op = "supersede older runs"
	var settled []string
	for _, p := range run.Phases() {
		if p.IsFinal() {
			settled = append(settled, p.String())
		}
	}
	superseded, err := queryAll(ctx, t.tx, op, supersedeOlder, pgx.RowTo[uuid.UUID], tgt, generation, now, settled)
	if err != nil || len(superseded) == 0 {
		return err
	}
	_, err = exec(ctx, t.tx, op, wakeSuperseded, superseded)
	return err
}

// ownsInputs reports whether the release belongs to the target's
// application, the revision to the target and the plan to the organization.
func (t *Tenant) ownsInputs(ctx context.Context, req StartDeployment, application ids.ApplicationID) (bool, error) {
	const op = "check a run's inputs"
	org := t.org.String()
	var release, revision bool
	if err := queryOne(ctx, t.tx, op, releaseOfApplication, []any{&release},
		req.Release, org, req.Project, application); err != nil {
		return false, err
	}
	if err := queryOne(ctx, t.tx, op, revisionOfTarget, []any{&revision},
		req.ConfigRevision, org, req.Target); err != nil {
		return false, err
	}
	plan := true
	if p, ok := req.RenderPlan.Get(); ok {
		if err := queryOne(ctx, t.tx, op, planOfOrg, []any{&plan}, p, org); err != nil {
			return false, err
		}
	}
	return release && revision && plan, nil
}

// RunPhase is the phase and generation of runID; false when the
// organization has no such run.
func (t *Tenant) RunPhase(ctx context.Context, runID ids.DeploymentRunID) (RunState, bool, error) {
	const op = "read a run's phase"
	type row struct {
		phase      string
		generation int64
	}
	r, ok, err := queryOpt(ctx, t.tx, op, selectRun, func(rw pgx.CollectableRow) (row, error) {
		var r row
		err := rw.Scan(&r.phase, &r.generation)
		return r, err
	}, runID, t.org.String())
	if err != nil || !ok {
		return RunState{}, false, err
	}
	phase, err := parsePhase(op, r.phase)
	if err != nil {
		return RunState{}, false, err
	}
	generation, err := counter(op, r.generation)
	if err != nil {
		return RunState{}, false, err
	}
	return RunState{Phase: phase, Generation: target.Generation(generation)}, true, nil
}

// RunSummary is a deployment run as the API shows it.
type RunSummary struct {
	Run        ids.DeploymentRunID
	Operation  ids.OperationID
	Generation target.Generation
	Phase      run.Phase
	// ApprovalsRequired are the approvals the run needs before delivery
	// (M4.1).
	ApprovalsRequired uint8
	// ApprovalExpiresAt is when a run waiting for approval is cancelled.
	ApprovalExpiresAt opt.Val[int64]
	// PlanHash is what approvers confirm they saw: sha-256 of the run's
	// inputs.
	PlanHash opt.Val[[32]byte]
}

func scanRunSummary(row pgx.CollectableRow) (RunSummary, error) {
	const op = "read a run"
	var s RunSummary
	var generation int64
	var phase string
	var approvals int16
	var expires *int64
	var hash []byte
	if err := row.Scan(&s.Run, &s.Operation, &generation, &phase, &approvals, &expires, &hash); err != nil {
		return RunSummary{}, err
	}
	g, err := counter(op, generation)
	if err != nil {
		return RunSummary{}, err
	}
	if s.Phase, err = parsePhase(op, phase); err != nil {
		return RunSummary{}, err
	}
	if approvals < 0 || approvals > 255 {
		return RunSummary{}, decodeErr(op, errOutOfRange)
	}
	if hash != nil {
		if len(hash) != 32 {
			return RunSummary{}, decodeErr(op, "a plan hash is 32 bytes")
		}
		s.PlanHash = opt.Some([32]byte(hash))
	}
	s.Generation = target.Generation(g)
	s.ApprovalsRequired = uint8(approvals) //nolint:gosec // checked above
	s.ApprovalExpiresAt = opt.FromPtr(expires)
	return s, nil
}

// LatestConfigRevision is the newest configuration revision of tgt, if it
// has one.
func (t *Tenant) LatestConfigRevision(ctx context.Context, tgt ids.TargetID) (ids.ConfigRevisionID, bool, error) {
	return queryOpt(ctx, t.tx, "read the latest configuration revision", latestConfigRevision,
		scanID[ids.ConfigRevision], tgt, t.org.String())
}

// RunOfTarget is run runID of tgt; false when it is not one of its runs.
func (t *Tenant) RunOfTarget(ctx context.Context, tgt ids.TargetID, runID ids.DeploymentRunID) (RunSummary, bool, error) {
	return queryOpt(ctx, t.tx, "read a run of a target", runOfTarget, scanRunSummary, runID, tgt, t.org.String())
}

// RunOfOperation is the run an accepted deployment operation created, for a
// replayed request.
func (t *Tenant) RunOfOperation(ctx context.Context, operation ids.OperationID) (RunSummary, bool, error) {
	return queryOpt(ctx, t.tx, "read the run of an operation", runOfOperation, scanRunSummary,
		operation, t.org.String())
}

// AdvanceRun applies event to runID for the worker holding claim on its
// operation, by the rules of [run.Phase.Apply].
func (s *Store) AdvanceRun(ctx context.Context, claim Claim, runID ids.DeploymentRunID, event run.Event) (a Advance, err error) {
	const op = "advance a run"
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	stored, ok, err := queryOpt(ctx, tx, op, lockRunUnderFence, pgx.RowTo[string], runID, claim.ID, claim.Fence)
	if err != nil {
		return nil, err
	}
	if !ok {
		return AdvanceFenced{}, nil
	}
	phase, err := parsePhase(op, stored)
	if err != nil {
		return nil, err
	}
	next, err := phase.Apply(event)
	if err != nil {
		var illegal *ops.IllegalTransitionError
		if errors.As(err, &illegal) {
			return AdvanceIllegal{Err: illegal}, nil
		}
		return nil, err
	}
	// Written once, apart from the phase: a failed deploy that a newer run
	// supersedes (it can no longer recover) stays a failed deploy.
	outcome, recovery := outcomes(next)
	if _, err := exec(ctx, tx, op, setRunPhase, runID, next.String(), s.now(), outcome.Ptr(), recovery.Ptr()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, dbErr(op, err)
	}
	return AdvanceMoved{Phase: next}, nil
}

// outcomes is what reaching phase records as the run's outcome and recovery
// outcome (migration 0006 keeps both apart from the phase; each is written
// once).
func outcomes(phase run.Phase) (outcome, recovery opt.Val[string]) {
	switch phase {
	case run.Succeeded:
		return opt.Some("succeeded"), opt.None[string]()
	case run.Failed:
		return opt.Some("failed"), opt.None[string]()
	case run.Cancelled:
		return opt.Some("cancelled"), opt.None[string]()
	case run.Recovered:
		return opt.None[string](), opt.Some("recovered")
	case run.RecoveryFailed:
		return opt.None[string](), opt.Some("recoveryFailed")
	case run.Planned, run.AwaitingApproval, run.PendingDelivery, run.AcceptedByCluster, run.Preflight, run.Blocked,
		run.Applying, run.Verifying, run.Superseded, run.CancelRequested, run.RecoveryRequested, run.Recovering,
		run.ManualActionRequired:
	}
	return opt.None[string](), opt.None[string]()
}

// Test support: set a run's phase and read its release, without the
// materializer (Rust: behind the `testing` feature, for the API's tests).
const (
	forceRunPhase = "UPDATE deployment_runs SET phase = $2 WHERE id = $1 AND org_id = $3"
	runRelease    = "SELECT release_id FROM deployment_runs WHERE id = $1 AND org_id = $2"
)

// ForceRunPhase sets runID's phase without the state machine. Tests only.
func (t *Tenant) ForceRunPhase(ctx context.Context, runID ids.DeploymentRunID, phase run.Phase) error {
	_, err := exec(ctx, t.tx, "force a run's phase", forceRunPhase, runID, phase.String(), t.org.String())
	return err
}

// RunRelease is the release of runID, if the organization has the run.
// Tests only.
func (t *Tenant) RunRelease(ctx context.Context, runID ids.DeploymentRunID) (ids.ReleaseID, bool, error) {
	return queryOpt(ctx, t.tx, "read a run's release", runRelease, scanID[ids.Release], runID, t.org.String())
}

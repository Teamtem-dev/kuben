package store

// Environment policies and deployment approvals (M4.1, migration 0019;
// repo/policies.rs).
//
// A decision locks its run, applies [policy.Decide] and, in the same
// transaction, records the decision and moves the run: enough approvals
// release it to delivery, one rejection cancels it. Either way its operation
// is woken, so the materializer acts at once.

import (
	"context"
	"errors"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
)

// The Rust built these from POLICY_COLUMNS with format!; the text is the
// same.
const (
	selectEnvironmentPolicy = "SELECT revision, required_approvals, deploy_role, approve_role, approval_ttl_secs, " +
		"created_by, created_at, scan_mode, scan_severity, scan_require, scan_max_age_secs " +
		"FROM environment_policies " +
		"WHERE environment_id = $1 AND org_id = $2 ORDER BY revision DESC LIMIT 1"
	selectPolicyOfTarget = "SELECT p.revision, p.required_approvals, p.deploy_role, p.approve_role, " +
		"p.approval_ttl_secs, p.created_by, p.created_at, p.scan_mode, p.scan_severity, p.scan_require, " +
		"p.scan_max_age_secs FROM application_targets t " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"JOIN environment_policies p ON p.environment_id = pl.environment_id AND p.org_id = t.org_id " +
		"WHERE t.id = $1 AND t.org_id = $2 ORDER BY p.revision DESC LIMIT 1"
	lockEnvironment = "SELECT id FROM environments " +
		"WHERE id = $1 AND org_id = $2 AND project_id = $3 FOR UPDATE"
	nextPolicyRevision = "SELECT COALESCE(max(revision), 0) + 1 FROM environment_policies " +
		"WHERE environment_id = $1 AND org_id = $2"
	insertPolicy = "INSERT INTO environment_policies " +
		"(org_id, project_id, environment_id, revision, required_approvals, deploy_role, approve_role, " +
		"approval_ttl_secs, created_by, created_at, scan_mode, scan_severity, scan_require, scan_max_age_secs) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)"
	lockRun = "SELECT phase, requested_by, approvals_required, approval_expires_at, " +
		"approval_plan_hash, operation_id FROM deployment_runs " +
		"WHERE id = $1 AND target_id = $2 AND org_id = $3 FOR UPDATE"
	selectRunApproval = "SELECT phase, requested_by, approvals_required, approval_expires_at, " +
		"approval_plan_hash, operation_id FROM deployment_runs " +
		"WHERE id = $1 AND target_id = $2 AND org_id = $3"
	selectDecisions = "SELECT approver, decision, comment, decided_at FROM run_approvals " +
		"WHERE run_id = $1 AND org_id = $2 ORDER BY decided_at, approver"
	insertDecision = "INSERT INTO run_approvals " +
		"(run_id, org_id, approver, decision, plan_hash, comment, decided_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7)"
	setRunPhaseAfterDecision = "UPDATE deployment_runs SET phase = $2, updated_at = $3, " +
		"outcome = COALESCE(outcome, $4) WHERE id = $1"
	wakeOperation = "UPDATE operations SET next_attempt_at = kuben_now_ms() WHERE id = $1 AND NOT done"
)

// PolicyRevision is one revision of an environment's policy.
type PolicyRevision struct {
	Revision  uint64
	Policy    policy.EnvironmentPolicy
	CreatedBy string
	CreatedAt int64
}

// errOutOfRange is Rust's TryFromIntError, as a decode error says it.
const errOutOfRange = "out of range integral type conversion attempted"

func scanPolicy(row pgx.CollectableRow) (PolicyRevision, error) {
	const op = "read an environment policy"
	var (
		revision                int64
		approvals               int16
		deployRole, approveRole string
		ttl, maxAge             int32
		p                       PolicyRevision
		scanMode, scanSeverity  string
		requireScan             bool
	)
	if err := row.Scan(&revision, &approvals, &deployRole, &approveRole, &ttl, &p.CreatedBy, &p.CreatedAt,
		&scanMode, &scanSeverity, &requireScan, &maxAge); err != nil {
		return PolicyRevision{}, err
	}
	var err error
	if p.Revision, err = counter(op, revision); err != nil {
		return PolicyRevision{}, err
	}
	if approvals < 0 || approvals > math.MaxUint8 || ttl < 0 || maxAge < 0 {
		return PolicyRevision{}, decodeErr(op, errOutOfRange)
	}
	if p.Policy.DeployRole, err = parseRole(op, deployRole); err != nil {
		return PolicyRevision{}, err
	}
	if p.Policy.ApproveRole, err = parseRole(op, approveRole); err != nil {
		return PolicyRevision{}, err
	}
	mode, err := scan.ParseGateMode(scanMode)
	if err != nil {
		return PolicyRevision{}, decodeErr(op, "unknown scan mode %s", rustQuote(scanMode))
	}
	severity, err := scan.ParseSeverity(scanSeverity)
	if err != nil {
		return PolicyRevision{}, decodeErr(op, "unknown severity %s", rustQuote(scanSeverity))
	}
	// The ranges are checked above.
	p.Policy.RequiredApprovals = uint8(approvals) //nolint:gosec // checked above
	p.Policy.ApprovalTTLSecs = uint32(ttl)        //nolint:gosec // checked above
	p.Policy.Scan = scan.Gate{
		Mode: mode, Severity: severity, RequireScan: requireScan, MaxAgeSecs: uint32(maxAge), //nolint:gosec // checked above
	}
	return p, nil
}

// EnvironmentPolicy is the newest policy revision of environment, if it has
// one.
func (t *Tenant) EnvironmentPolicy(ctx context.Context, environment ids.EnvironmentID) (PolicyRevision, bool, error) {
	return queryOpt(ctx, t.tx, "read an environment policy", selectEnvironmentPolicy, scanPolicy,
		environment, t.org.String())
}

// PolicyOfTarget is the newest policy revision of the environment tgt is
// placed in, if it has one.
func (t *Tenant) PolicyOfTarget(ctx context.Context, tgt ids.TargetID) (PolicyRevision, bool, error) {
	return queryOpt(ctx, t.tx, "read a target's policy", selectPolicyOfTarget, scanPolicy, tgt, t.org.String())
}

// int4 is a u32 as PostgreSQL's INTEGER; beyond it is an encode error, as
// sqlx's i32::try_from was.
func int4(op string, value uint32) (int32, error) {
	if value > math.MaxInt32 {
		return 0, DatabaseError{Op: op, Err: errors.New("error occurred while encoding a value: " + errOutOfRange)}
	}
	return int32(value), nil //nolint:gosec // checked above
}

// SetEnvironmentPolicy records p as the newest revision of environment and
// returns its number; false when the project has no such environment. The
// caller validates the policy and decides who may change it.
func (t *Tenant) SetEnvironmentPolicy(
	ctx context.Context, project ids.ProjectID, environment ids.EnvironmentID, p policy.EnvironmentPolicy, createdBy string,
) (uint64, bool, error) {
	const op = "set an environment policy"
	org := t.org.String()
	_, locked, err := queryOpt(ctx, t.tx, op, lockEnvironment, scanID[ids.Environment], environment, org, project)
	if err != nil || !locked {
		return 0, false, err
	}
	var revision int64
	if err := queryOne(ctx, t.tx, op, nextPolicyRevision, []any{&revision}, environment, org); err != nil {
		return 0, false, err
	}
	ttl, err := int4(op, p.ApprovalTTLSecs)
	if err != nil {
		return 0, false, err
	}
	maxAge, err := int4(op, p.Scan.MaxAgeSecs)
	if err != nil {
		return 0, false, err
	}
	if _, err := exec(ctx, t.tx, op, insertPolicy, org, project, environment, revision,
		int16(p.RequiredApprovals), p.DeployRole.String(), p.ApproveRole.String(), ttl, createdBy,
		t.store.now(), p.Scan.Mode.String(), p.Scan.Severity.String(), p.Scan.RequireScan, maxAge); err != nil {
		return 0, false, err
	}
	n, err := counter(op, revision)
	return n, err == nil, err
}

// ApprovalRecord is one recorded approval decision.
type ApprovalRecord struct {
	Approver  string
	Decision  policy.Decision
	Comment   opt.Val[string]
	DecidedAt int64
}

// RunApproval is a run's approval state, as the API shows it.
type RunApproval struct {
	Phase       run.Phase
	RequestedBy string
	Required    uint8
	ExpiresAt   opt.Val[int64]
	// PlanHash is what an approver confirms; present (possibly empty) only
	// while the run was parked for approval.
	PlanHash  opt.Val[[]byte]
	Decisions []ApprovalRecord
}

// Approved reports the number of distinct approvals recorded, saturating at
// 255 as Rust's u8::try_from(n).unwrap_or(u8::MAX).
func (a RunApproval) Approved() uint8 { return approvals(a.Decisions) }

// approvals counts the approving decisions, saturating at 255.
func approvals(decisions []ApprovalRecord) uint8 {
	var n uint8
	for _, d := range decisions {
		if d.Decision == policy.Approve && n < math.MaxUint8 {
			n++
		}
	}
	return n
}

// Decided is the outcome of [Tenant.DecideRun].
//
//sumtype:decl
type Decided interface{ decided() }

type (
	// DecidedRecorded means recorded; the run moved or waits for more approvals.
	DecidedRecorded struct{ Tally policy.Tally }
	// DecidedRefused means nothing was recorded.
	DecidedRefused struct{ Err policy.ApprovalError }
	// DecidedNotFound means no such run of the target.
	DecidedNotFound struct{}
)

func (DecidedRecorded) decided() {}
func (DecidedRefused) decided()  {}
func (DecidedNotFound) decided() {}

type runApprovalRow struct {
	phase             string
	requestedBy       string
	approvalsRequired int16
	approvalExpiresAt *int64
	// approvalPlanHash is nil for SQL NULL; pgx reads an empty bytea as a
	// non-nil empty slice, so Rust's Some(empty) and None stay apart.
	approvalPlanHash []byte
	operationID      ids.OperationID
}

// planHashOf is a nullable bytea column as Rust's Option<Vec<u8>>.
func planHashOf(column []byte) opt.Val[[]byte] {
	if column == nil {
		return opt.None[[]byte]()
	}
	return opt.Some(column)
}

func scanRunApprovalRow(row pgx.CollectableRow) (runApprovalRow, error) {
	var r runApprovalRow
	if err := row.Scan(&r.phase, &r.requestedBy, &r.approvalsRequired, &r.approvalExpiresAt, &r.approvalPlanHash, &r.operationID); err != nil {
		return runApprovalRow{}, err
	}
	return r, nil
}

func scanDecision(row pgx.CollectableRow) (ApprovalRecord, error) {
	const op = "read approval decisions"
	var (
		rec      ApprovalRecord
		decision string
		comment  *string
	)
	if err := row.Scan(&rec.Approver, &decision, &comment, &rec.DecidedAt); err != nil {
		return ApprovalRecord{}, err
	}
	rec.Comment = opt.FromPtr(comment)
	switch decision {
	case "approved":
		rec.Decision = policy.Approve
	case "rejected":
		rec.Decision = policy.Reject
	default:
		return ApprovalRecord{}, decodeErr(op, "unknown decision %s", rustQuote(decision))
	}
	return rec, nil
}

func (t *Tenant) decisions(ctx context.Context, runID ids.DeploymentRunID) ([]ApprovalRecord, error) {
	const op = "read approval decisions"
	return queryAll(ctx, t.tx, op, selectDecisions, scanDecision, runID, t.org.String())
}

// RunApproval returns the approval state of run of target, false if not found.
func (t *Tenant) RunApproval(ctx context.Context, target ids.TargetID, runID ids.DeploymentRunID) (RunApproval, bool, error) {
	const op = "read a run's approval"
	row, found, err := queryOpt(ctx, t.tx, op, selectRunApproval, scanRunApprovalRow, runID, target, t.org.String())
	if err != nil || !found {
		return RunApproval{}, false, err
	}
	phase, err := run.ParsePhase(row.phase)
	if err != nil {
		return RunApproval{}, false, decodeErr(op, "%v", err)
	}
	if row.approvalsRequired < 0 || row.approvalsRequired > math.MaxUint8 {
		return RunApproval{}, false, decodeErr(op, errOutOfRange)
	}
	decisions, err := t.decisions(ctx, runID)
	if err != nil {
		return RunApproval{}, false, err
	}
	return RunApproval{
		Phase:       phase,
		RequestedBy: row.requestedBy,
		Required:    uint8(row.approvalsRequired),
		ExpiresAt:   opt.FromPtr(row.approvalExpiresAt),
		PlanHash:    planHashOf(row.approvalPlanHash),
		Decisions:   decisions,
	}, true, nil
}

// DecideRun records approver's decision on run of target, having seen planHash,
// and moves the run when the decision settles it.
func (t *Tenant) DecideRun(
	ctx context.Context,
	target ids.TargetID,
	runID ids.DeploymentRunID,
	approver string,
	decision policy.Decision,
	planHash []byte,
	comment opt.Val[string],
) (Decided, error) {
	const op = "decide an approval"
	org := t.org.String()
	row, found, err := queryOpt(ctx, t.tx, op, lockRun, scanRunApprovalRow, runID, target, org)
	if err != nil {
		return nil, err
	}
	if !found {
		return DecidedNotFound{}, nil
	}
	phase, err := run.ParsePhase(row.phase)
	if err != nil {
		return nil, decodeErr(op, "%v", err)
	}
	expected, hasHash := planHashOf(row.approvalPlanHash).Get()
	if row.approvalExpiresAt == nil || !hasHash || phase != run.AwaitingApproval {
		return DecidedRefused{Err: policy.ErrNotAwaiting}, nil
	}
	decisions, err := t.decisions(ctx, runID)
	if err != nil {
		return nil, err
	}
	if row.approvalsRequired < 0 || row.approvalsRequired > math.MaxUint8 {
		return nil, decodeErr(op, errOutOfRange)
	}
	pending := policy.Pending{
		RequestedBy: row.requestedBy,
		Required:    uint8(row.approvalsRequired),
		Approved:    approvals(decisions),
		ExpiresAt:   *row.approvalExpiresAt,
		PlanHash:    expected,
	}
	decidedBefore := slices.ContainsFunc(decisions, func(d ApprovalRecord) bool { return d.Approver == approver })
	now := t.store.now()
	tally, err := policy.Decide(pending, approver, decidedBefore, decision, planHash, now)
	if err != nil {
		var appErr policy.ApprovalError
		if errors.As(err, &appErr) {
			return DecidedRefused{Err: appErr}, nil
		}
		return nil, err
	}
	if _, err := exec(ctx, t.tx, op, insertDecision, runID, org, approver, decision.Stored(), planHash, comment.Ptr(), now); err != nil {
		return nil, err
	}
	if err := t.settleRun(ctx, runID, row.operationID, phase, tally, now); err != nil {
		return nil, err
	}
	return DecidedRecorded{Tally: tally}, nil
}

// settleRun moves a run a decision settled and wakes its operation: enough
// approvals release it to delivery, a rejection cancels it. A run still
// waiting stays as it is.
func (t *Tenant) settleRun(
	ctx context.Context, runID ids.DeploymentRunID, operation ids.OperationID, phase run.Phase, tally policy.Tally, now int64,
) error {
	const op = "decide an approval"
	var (
		event   run.Event
		outcome opt.Val[string]
	)
	switch tally.(type) {
	case policy.Waiting:
		return nil
	case policy.Approved:
		event = run.EventApproved
	case policy.Rejected:
		event = run.EventRejected
		outcome = opt.Some("cancelled")
	}
	next, err := phase.Apply(event)
	if err != nil {
		return protocolErr(op, "%v", err)
	}
	if _, err := exec(ctx, t.tx, op, setRunPhaseAfterDecision, runID, next.String(), now, outcome.Ptr()); err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, wakeOperation, operation)
	return err
}

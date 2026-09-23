package store

// Environment policies (M4.1, migration 0019); a partial port of
// repo/policies.rs: the policy revisions a deployment reads and recording
// a new one. Deciding approvals follows with the policy routes.

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
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

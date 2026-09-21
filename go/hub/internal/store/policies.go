package store

// Environment policies (M4.1, migration 0019); a partial port of
// repo/policies.rs: the policy revisions a deployment reads. Setting a
// policy and deciding approvals follow with the policy routes.

import (
	"context"
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

package api

// Environment protection policy routes (M4.1, routes/policy.rs): approvals,
// who deploys, who approves and how long a change waits, and the vulnerability
// gate (M4.6). Every change is a new revision; weakening protection is an
// owner's decision.

import (
	"context"
	"math"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// scanGateFromDto is Rust's TryFrom<&ScanGateDto> for ScanGate. The
// generated decoder already refused a negative maxAgeSecs (minimum: 0).
func scanGateFromDto(d gen.ScanGateDto) (scan.Gate, error) {
	mode, err := scan.ParseGateMode(d.Mode)
	if err != nil {
		// The API names it a scan gate mode; the core parser's text differs.
		return scan.Gate{}, kerrors.New(kerrors.Validation, "unknown scan gate mode `%s`", d.Mode)
	}
	severity, err := scan.ParseSeverity(d.Severity)
	if err != nil {
		return scan.Gate{}, err //nolint:wrapcheck // a kerrors validation error
	}
	maxAge := scan.DefaultMaxAgeSecs
	if v, ok := d.MaxAgeSecs.Get(); ok {
		maxAge = uint32(v) //nolint:gosec // non-negative: the decoder enforces minimum 0
	}
	return scan.Gate{
		Mode:        mode,
		Severity:    severity,
		RequireScan: d.RequireScan.Or(false),
		MaxAgeSecs:  maxAge,
	}, nil
}

func policyOf(body *gen.PutPolicy, current scan.Gate) (policy.EnvironmentPolicy, error) {
	if body == nil {
		return policy.EnvironmentPolicy{}, kerrors.New(kerrors.Validation, "missing request body")
	}
	// Rust's u8 refused more than 255 while decoding the body (serde's
	// message); 6 to 255 reach Validate below and get its message. The
	// decoder already refused a negative count (minimum: 0).
	if body.RequiredApprovals > math.MaxUint8 {
		return policy.EnvironmentPolicy{}, kerrors.New(kerrors.Validation,
			"requiredApprovals: invalid value: integer `%d`, expected u8", body.RequiredApprovals)
	}
	deployRole, err := perm.ParseRole(body.DeployRole)
	if err != nil {
		return policy.EnvironmentPolicy{}, err //nolint:wrapcheck // a kerrors validation error
	}
	approveRole, err := perm.ParseRole(body.ApproveRole)
	if err != nil {
		return policy.EnvironmentPolicy{}, err //nolint:wrapcheck // a kerrors validation error
	}
	// The contract types approvalTtlSecs as int32 with minimum 0, so the
	// decoder refused negative values and Rust's u32 values above int32 cannot
	// be sent.
	ttl := policy.DefaultApprovalTTLSecs
	if v, ok := body.ApprovalTtlSecs.Get(); ok {
		ttl = uint32(v) //nolint:gosec // non-negative: the decoder enforces minimum 0
	}
	gate := current
	if scanDto, ok := body.Scan.Get(); ok {
		parsed, err := scanGateFromDto(scanDto)
		if err != nil {
			return policy.EnvironmentPolicy{}, err
		}
		gate = parsed
	}
	p := policy.EnvironmentPolicy{
		RequiredApprovals: uint8(body.RequiredApprovals), //nolint:gosec // 0..=255, checked above
		DeployRole:        deployRole,
		ApproveRole:       approveRole,
		ApprovalTTLSecs:   ttl,
		Scan:              gate,
	}
	if err := p.Validate(); err != nil {
		return policy.EnvironmentPolicy{}, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	return p, nil
}

func policyDto(rev opt.Val[store.PolicyRevision]) *gen.PolicyDto {
	p := policy.Open()
	var revision int64
	// Rust writes "updatedBy":null,"updatedAt":null without a revision.
	updatedBy, updatedAt := opt.None[string](), opt.None[int64]()
	if r, ok := rev.Get(); ok {
		p = r.Policy
		revision = int64(r.Revision) //nolint:gosec // a revision counter, far below 2^63
		updatedBy, updatedAt = opt.Some(r.CreatedBy), opt.Some(r.CreatedAt)
	}

	return &gen.PolicyDto{
		Revision:          revision,
		RequiredApprovals: int32(p.RequiredApprovals),
		DeployRole:        p.DeployRole.String(),
		ApproveRole:       p.ApproveRole.String(),
		ApprovalTtlSecs:   int32(p.ApprovalTTLSecs), //nolint:gosec // at most 30 days: stored policies are valid
		Scan: gen.ScanGateDto{
			Mode:        p.Scan.Mode.String(),
			Severity:    p.Scan.Severity.String(),
			RequireScan: gen.NewOptBool(p.Scan.RequireScan),
			MaxAgeSecs:  gen.NewOptInt32(int32(p.Scan.MaxAgeSecs)), //nolint:gosec // at most 90 days: stored gates are valid
		},
		UpdatedBy: optNilString(updatedBy),
		UpdatedAt: optNilInt64(updatedAt),
	}
}

// GetEnvironmentPolicy returns the environment's protection policy.
func (s *Server) GetEnvironmentPolicy(
	ctx context.Context, params gen.GetEnvironmentPolicyParams,
) (gen.GetEnvironmentPolicyRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	chain := e.chain()
	if _, err := acc.Require(perm.EnvRead, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	tenant, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // read only

	rev, found, err := tenant.EnvironmentPolicy(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return policyDto(revisionOf(rev, found)), nil
}

// revisionOf is a store lookup as Rust's Option<PolicyRevision>.
func revisionOf(rev store.PolicyRevision, found bool) opt.Val[store.PolicyRevision] {
	if !found {
		return opt.None[store.PolicyRevision]()
	}
	return opt.Some(rev)
}

// PutEnvironmentPolicy changes the environment's protection policy. Stricter
// policies need env-write; anything weaker needs an owner.
func (s *Server) PutEnvironmentPolicy(
	ctx context.Context, req *gen.PutPolicy, params gen.PutEnvironmentPolicyParams,
) (gen.PutEnvironmentPolicyRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := acc.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	e, err := s.findEnvironment(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	chain := e.chain()
	if _, err := acc.Require(perm.EnvWrite, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	_, actor := acc.Actor()

	tenant, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	rev, found, err := tenant.EnvironmentPolicy(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	current := revisionOf(rev, found)
	currentPolicy := policy.Open()
	if r, ok := current.Get(); ok {
		currentPolicy = r.Policy
	}

	next, err := policyOf(req, currentPolicy.Scan)
	if err != nil {
		return nil, err
	}
	if next == currentPolicy {
		return policyDto(current), nil
	}

	if next.Weakens(currentPolicy) {
		if _, err := acc.Require(perm.EnvProtect, chain); err != nil {
			return nil, err //nolint:wrapcheck // a kerrors already
		}
	}

	return recordPolicy(ctx, tenant, e, next, actor, params.Environment)
}

// recordPolicy stores next as the newest revision of e's policy, commits,
// and answers with that revision.
func recordPolicy(
	ctx context.Context, tenant *store.Tenant, e envScope, next policy.EnvironmentPolicy, actor, name string,
) (*gen.PolicyDto, error) {
	_, set, err := tenant.SetEnvironmentPolicy(ctx, e.project.project.ID, e.env.ID, next, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !set {
		return nil, scopeNotFound("environment", name)
	}
	newRev, newFound, err := tenant.EnvironmentPolicy(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := tenant.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return policyDto(revisionOf(newRev, newFound)), nil
}

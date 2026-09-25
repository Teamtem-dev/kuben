package api

// Environment protection policy routes (M4.1, routes/policy.rs): approvals,
// who deploys, who approves and how long a change waits, and the vulnerability
// gate (M4.6). Every change is a new revision; weakening protection is an
// owner's decision.

import (
	"context"
	"math"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func scanGateFromDto(d gen.ScanGateDto) (scan.Gate, error) {
	mode, err := scan.ParseGateMode(d.Mode)
	if err != nil {
		return scan.Gate{}, err //nolint:wrapcheck // a kerr validation error
	}
	severity, err := scan.ParseSeverity(d.Severity)
	if err != nil {
		return scan.Gate{}, err //nolint:wrapcheck // a kerr validation error
	}
	requireScan := d.RequireScan.Or(false)
	maxAge := scan.DefaultMaxAgeSecs
	if d.MaxAgeSecs.IsSet() {
		if d.MaxAgeSecs.Value < 0 {
			return scan.Gate{}, kerr.New(kerr.Validation, "maxAgeSecs must be non-negative")
		}
		maxAge = uint32(d.MaxAgeSecs.Value)
	}
	return scan.Gate{
		Mode:        mode,
		Severity:    severity,
		RequireScan: requireScan,
		MaxAgeSecs:  maxAge,
	}, nil
}

func policyOf(body *gen.PutPolicy, current scan.Gate) (policy.EnvironmentPolicy, error) {
	if body == nil {
		return policy.EnvironmentPolicy{}, kerr.New(kerr.Validation, "missing request body")
	}
	if body.RequiredApprovals < 0 || body.RequiredApprovals > math.MaxUint8 {
		return policy.EnvironmentPolicy{}, kerr.New(kerr.Validation, "requiredApprovals out of range")
	}
	deployRole, err := perm.ParseRole(body.DeployRole)
	if err != nil {
		return policy.EnvironmentPolicy{}, err //nolint:wrapcheck // a kerr validation error
	}
	approveRole, err := perm.ParseRole(body.ApproveRole)
	if err != nil {
		return policy.EnvironmentPolicy{}, err //nolint:wrapcheck // a kerr validation error
	}
	ttl := policy.DefaultApprovalTTLSecs
	if body.ApprovalTtlSecs.IsSet() {
		if body.ApprovalTtlSecs.Value < 0 {
			return policy.EnvironmentPolicy{}, kerr.New(kerr.Validation, "approvalTtlSecs must be non-negative")
		}
		ttl = uint32(body.ApprovalTtlSecs.Value)
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
		RequiredApprovals: uint8(body.RequiredApprovals),
		DeployRole:        deployRole,
		ApproveRole:       approveRole,
		ApprovalTTLSecs:   ttl,
		Scan:              gate,
	}
	if err := p.Validate(); err != nil {
		return policy.EnvironmentPolicy{}, kerr.New(kerr.Validation, "%s", err.Error())
	}
	return p, nil
}

func policyDto(rev opt.Val[store.PolicyRevision]) *gen.PolicyDto {
	p := policy.Open()
	var revision int64
	var updatedBy gen.OptNilString
	var updatedAt gen.OptNilInt64

	if r, ok := rev.Get(); ok {
		p = r.Policy
		revision = int64(r.Revision)
		updatedBy = gen.NewOptNilString(r.CreatedBy)
		updatedAt = gen.NewOptNilInt64(r.CreatedAt)
	}

	return &gen.PolicyDto{
		Revision:          revision,
		RequiredApprovals: int32(p.RequiredApprovals),
		DeployRole:        p.DeployRole.String(),
		ApproveRole:       p.ApproveRole.String(),
		ApprovalTtlSecs:   int32(p.ApprovalTTLSecs),
		Scan: gen.ScanGateDto{
			Mode:        p.Scan.Mode.String(),
			Severity:    p.Scan.Severity.String(),
			RequireScan: gen.NewOptBool(p.Scan.RequireScan),
			MaxAgeSecs:  gen.NewOptInt32(int32(p.Scan.MaxAgeSecs)),
		},
		UpdatedBy: updatedBy,
		UpdatedAt: updatedAt,
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
		return nil, err //nolint:wrapcheck // a kerr already
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
	if !found {
		return policyDto(opt.None[store.PolicyRevision]()), nil
	}
	return policyDto(opt.Some(rev)), nil
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
		return nil, err //nolint:wrapcheck // a kerr already
	}
	e, err := s.findEnvironment(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	chain := e.chain()
	if _, err := acc.Require(perm.EnvWrite, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
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
	current := policy.Open()
	if found {
		current = rev.Policy
	}

	next, err := policyOf(req, current.Scan)
	if err != nil {
		return nil, err
	}

	if next == current {
		var r opt.Val[store.PolicyRevision]
		if found {
			r = opt.Some(rev)
		}
		return policyDto(r), nil
	}

	if next.Weakens(current) {
		if _, err := acc.Require(perm.EnvProtect, chain); err != nil {
			return nil, err //nolint:wrapcheck // a kerr already
		}
	}

	_, set, err := tenant.SetEnvironmentPolicy(ctx, e.project.project.ID, e.env.ID, next, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !set {
		return nil, scopeNotFound("environment", params.Environment)
	}

	newRev, newFound, err := tenant.EnvironmentPolicy(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !newFound {
		return nil, scopeNotFound("environment", params.Environment)
	}
	if err := tenant.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return policyDto(opt.Some(newRev)), nil
}

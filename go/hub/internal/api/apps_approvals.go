package api

// Deployment approvals (M4.1, routes/apps/approvals.rs): who may start a
// deployment under an environment's policy, and the approve/reject decisions
// on a run waiting for them.
//
// Approving is a person's act: API tokens cannot decide, the requester can
// never decide on their own run, and the decision names the plan hash the
// approver was shown, so a changed or superseded run is not approved by
// mistake.

import (
	"context"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func refusalErr(e policy.ApprovalError) error {
	switch e {
	case policy.ErrSelfApproval:
		return kerr.ErrForbidden
	case policy.ErrNotAwaiting, policy.ErrExpired, policy.ErrAlreadyDecided, policy.ErrStalePlan:
		return kerr.New(kerr.Conflict, "%s", e)
	default:
		return kerr.New(kerr.Conflict, "%s", e)
	}
}

func eligible(acc access.Access, a store.RunApproval, mayApprove bool) bool {
	me := acc.Current.User.ID.String()
	if !acc.Current.Token.IsNone() {
		return false
	}
	if !mayApprove {
		return false
	}
	if a.Phase != run.AwaitingApproval {
		return false
	}
	if policy.Principal(a.RequestedBy) == me {
		return false
	}
	for _, d := range a.Decisions {
		if d.Approver == me {
			return false
		}
	}
	return true
}

func approvalDto(runID uuid.UUID, a store.RunApproval, act actors, canDecide bool) *gen.ApprovalDto {
	var planHash gen.OptNilString
	if len(a.PlanHash) > 0 {
		planHash = gen.NewOptNilString(hex.EncodeToString(a.PlanHash))
	}
	var expiresAt gen.OptNilInt64
	if a.ExpiresAt != nil {
		expiresAt = gen.NewOptNilInt64(*a.ExpiresAt)
	}
	decisions := make([]gen.DecisionDto, len(a.Decisions))
	for i, d := range a.Decisions {
		var comment gen.OptNilString
		if d.Comment != nil {
			comment = gen.NewOptNilString(*d.Comment)
		}
		decisions[i] = gen.DecisionDto{
			Approver:  act.name("user:" + d.Approver),
			Decision:  d.Decision.Stored(),
			Comment:   comment,
			DecidedAt: d.DecidedAt,
		}
	}
	return &gen.ApprovalDto{
		Approved:    int32(a.Approved()),
		CanDecide:   canDecide,
		Decisions:   decisions,
		ExpiresAt:   expiresAt,
		Phase:       a.Phase.String(),
		PlanHash:    planHash,
		RequestedBy: act.name(a.RequestedBy),
		Required:    int32(a.Required),
		Run:         runID,
	}
}

// GetDeploymentApproval returns the approval state of a deployment run.
func (s *Server) GetDeploymentApproval(ctx context.Context, params gen.GetDeploymentApprovalParams) (gen.GetDeploymentApprovalRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	chain := a.chain()
	if _, err := acc.Require(perm.AppRead, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}

	tenant, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // read only

	runID := ids.From[ids.DeploymentRun](params.Run)
	approval, found, err := tenant.RunApproval(ctx, a.app.Target, runID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, scopeNotFound("deployment", params.Run.String())
	}

	mayApprove := false
	if polRev, polFound, err := tenant.PolicyOfTarget(ctx, a.app.Target); err == nil && polFound {
		if _, err := acc.Require(perm.ReleaseApprove, chain); err == nil {
			mayApprove = polRev.Policy.MayApprove(authz.EffectiveRole(acc.Subject, chain))
		}
	}

	actors, err := s.loadActors(ctx)
	if err != nil {
		return nil, err
	}
	canDecide := eligible(acc, approval, mayApprove)
	return approvalDto(params.Run, approval, actors, canDecide), nil
}

func (s *Server) decideDeployment(
	ctx context.Context,
	project, environment, app string,
	runID uuid.UUID,
	req *gen.DecideRequest,
	decision policy.Decision,
) (*gen.ApprovalDto, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := acc.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	a, err := s.findApp(ctx, acc, project, environment, app)
	if err != nil {
		return nil, err
	}
	chain := a.chain()
	if _, err := acc.Require(perm.ReleaseApprove, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}

	var comment opt.Val[string]
	if c, ok := req.Comment.Get(); ok {
		c = strings.TrimSpace(c)
		if c != "" {
			if utf8.RuneCountInString(c) > policy.MaxCommentChars {
				return nil, kerr.New(kerr.Validation, "a comment has at most %d characters", policy.MaxCommentChars)
			}
			comment = opt.Some(c)
		}
	}

	planHashHex := strings.TrimSpace(req.PlanHash)
	planHash, err := hex.DecodeString(planHashHex)
	if err != nil || len(planHash) == 0 {
		return nil, kerr.New(kerr.Validation, "`planHash` is not a hex plan hash")
	}

	tenant, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // committed on success

	polRev, polFound, err := tenant.PolicyOfTarget(ctx, a.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !polFound {
		return nil, kerr.New(kerr.Conflict, "the environment requires no approvals")
	}
	if !polRev.Policy.MayApprove(authz.EffectiveRole(acc.Subject, chain)) {
		return nil, kerr.ErrForbidden
	}

	depRunID := ids.From[ids.DeploymentRun](runID)
	approver := acc.Current.User.ID.String()
	decided, err := tenant.DecideRun(ctx, a.app.Target, depRunID, approver, decision, planHash, comment)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch d := decided.(type) {
	case store.DecidedRecorded:
		s.deps.Logger.InfoContext(ctx, "deployment decision recorded",
			"run", runID, "approver", approver, "decision", decision.Stored(), "tally", d.Tally)
	case store.DecidedRefused:
		return nil, refusalErr(d.Err)
	case store.DecidedNotFound:
		return nil, scopeNotFound("deployment", runID.String())
	}

	approval, found, err := tenant.RunApproval(ctx, a.app.Target, depRunID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerr.New(kerr.Internal, "the decided run is missing")
	}
	if err := tenant.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}

	actors, err := s.loadActors(ctx)
	if err != nil {
		return nil, err
	}
	return approvalDto(runID, approval, actors, false), nil
}

// ApproveDeployment approves a deployment waiting for approval.
func (s *Server) ApproveDeployment(ctx context.Context, req *gen.DecideRequest, params gen.ApproveDeploymentParams) (gen.ApproveDeploymentRes, error) {
	return s.decideDeployment(ctx, params.Project, params.Environment, params.App, params.Run, req, policy.Approve)
}

// RejectDeployment rejects a deployment waiting for approval; it is cancelled.
func (s *Server) RejectDeployment(ctx context.Context, req *gen.DecideRequest, params gen.RejectDeploymentParams) (gen.RejectDeploymentRes, error) {
	return s.decideDeployment(ctx, params.Project, params.Environment, params.App, params.Run, req, policy.Reject)
}

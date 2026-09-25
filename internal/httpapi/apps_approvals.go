package httpapi

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
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// refusalErr is the HTTP answer to a refused decision: the requester is
// forbidden, every other refusal is a conflict carrying its message.
func refusalErr(e policy.ApprovalError) error {
	switch e {
	case policy.ErrSelfApproval:
		return kerrors.ErrForbidden
	case policy.ErrNotAwaiting, policy.ErrExpired, policy.ErrAlreadyDecided, policy.ErrStalePlan:
	}
	return kerrors.New(kerrors.Conflict, "%s", e)
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
	// Rust writes expiresAt, planHash and comment as null when absent.
	planHash := opt.None[string]()
	if h, ok := a.PlanHash.Get(); ok {
		planHash = opt.Some(policy.Hex(h))
	}
	decisions := make([]gen.DecisionDto, len(a.Decisions))
	for i, d := range a.Decisions {
		decisions[i] = gen.DecisionDto{
			Approver:  act.name("user:" + d.Approver),
			Decision:  d.Decision.Stored(),
			Comment:   optNilString(d.Comment),
			DecidedAt: d.DecidedAt,
		}
	}
	return &gen.ApprovalDto{
		Approved:    int32(a.Approved()),
		CanDecide:   canDecide,
		Decisions:   decisions,
		ExpiresAt:   optNilInt64(a.ExpiresAt),
		Phase:       a.Phase.String(),
		PlanHash:    optNilString(planHash),
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
		return nil, err //nolint:wrapcheck // a kerrors already
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

	polRev, polFound, err := tenant.PolicyOfTarget(ctx, a.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	mayApprove := false
	if polFound {
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
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	a, err := s.findApp(ctx, acc, project, environment, app)
	if err != nil {
		return nil, err
	}
	chain := a.chain()
	if _, err := acc.Require(perm.ReleaseApprove, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}

	comment, err := decisionComment(req.Comment)
	if err != nil {
		return nil, err
	}

	// An empty hash is a hash: it is refused as a changed plan, not here.
	planHash, ok := policy.Unhex(strings.TrimSpace(req.PlanHash))
	if !ok {
		return nil, kerrors.New(kerrors.Validation, "`planHash` is not a hex plan hash")
	}

	tenant, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // committed on success

	if err := mayDecide(ctx, tenant, a.app.Target, acc, chain); err != nil {
		return nil, err
	}

	depRunID := ids.From[ids.DeploymentRun](runID)
	approver := acc.Current.User.ID.String()
	decided, err := tenant.DecideRun(ctx, a.app.Target, depRunID, approver, decision, planHash, comment)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := s.decidedErr(ctx, decided, runID, approver, decision); err != nil {
		return nil, err
	}

	approval, found, err := tenant.RunApproval(ctx, a.app.Target, depRunID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.Internal, "the decided run is missing")
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

// mayDecide refuses a decision on target's runs when its environment has no
// policy, or when the caller's role may not approve there. The role is
// checked again at decision time: a demoted approver cannot use a page
// opened earlier.
func mayDecide(ctx context.Context, tenant *store.Tenant, target ids.TargetID, acc access.Access, chain authz.ScopeChain) error {
	polRev, polFound, err := tenant.PolicyOfTarget(ctx, target)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if !polFound {
		return kerrors.New(kerrors.Conflict, "the environment requires no approvals")
	}
	if !polRev.Policy.MayApprove(authz.EffectiveRole(acc.Subject, chain)) {
		return kerrors.ErrForbidden
	}
	return nil
}

// decisionComment is the trimmed comment of a decision, absent when empty.
func decisionComment(raw gen.OptNilString) (opt.Val[string], error) {
	c, ok := raw.Get()
	c = strings.TrimSpace(c)
	if !ok || c == "" {
		return opt.None[string](), nil
	}
	if utf8.RuneCountInString(c) > policy.MaxCommentChars {
		return opt.None[string](), kerrors.New(kerrors.Validation, "a comment has at most %d characters", policy.MaxCommentChars)
	}
	return opt.Some(c), nil
}

// decidedErr logs a recorded decision and turns a refused or missing run
// into its HTTP error.
func (s *Server) decidedErr(
	ctx context.Context, decided store.Decided, runID uuid.UUID, approver string, decision policy.Decision,
) error {
	switch d := decided.(type) {
	case store.DecidedRecorded:
		s.deps.Logger.InfoContext(ctx, "deployment decision recorded",
			"run", runID, "approver", approver, "decision", decision.Stored(), "tally", d.Tally)
	case store.DecidedRefused:
		return refusalErr(d.Err)
	case store.DecidedNotFound:
		return scopeNotFound("deployment", runID.String())
	}
	return nil
}

// ApproveDeployment approves a deployment waiting for approval.
func (s *Server) ApproveDeployment(ctx context.Context, req *gen.DecideRequest, params gen.ApproveDeploymentParams) (gen.ApproveDeploymentRes, error) {
	return s.decideDeployment(ctx, params.Project, params.Environment, params.App, params.Run, req, policy.Approve)
}

// RejectDeployment rejects a deployment waiting for approval; it is cancelled.
func (s *Server) RejectDeployment(ctx context.Context, req *gen.DecideRequest, params gen.RejectDeploymentParams) (gen.RejectDeploymentRes, error) {
	return s.decideDeployment(ctx, params.Project, params.Environment, params.App, params.Run, req, policy.Reject)
}

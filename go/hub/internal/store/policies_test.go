package store_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

const (
	aliceUser = "0192f3a1-0000-7000-8000-00000000000a"
	bobUser   = "0192f3a1-0000-7000-8000-00000000000b"
	carolUser = "0192f3a1-0000-7000-8000-00000000000c"
)

type policyFixture struct {
	org          ids.OrgID
	project      ids.ProjectID
	environment  ids.EnvironmentID
	application  ids.ApplicationID
	target       ids.TargetID
	release      ids.ReleaseID
	revision     ids.ConfigRevisionID
	lifecycleUID uuid.UUID
}

func newPolicyFixture(t *testing.T, s *store.Store, slug string, pol opt.Val[policy.EnvironmentPolicy]) policyFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	if p, ok := pol.Get(); ok {
		if _, ok, err := tn.SetEnvironmentPolicy(ctx, project, env, p, "user:admin"); err != nil || !ok {
			t.Fatalf("set policy: %v, %v", ok, err)
		}
	}
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, slug+"-shop"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	state, ok, err := tn.TargetState(ctx, tgt)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	rev := configRevision(t, tn, project, tgt, map[string]any{"replicas": 1})
	rel, _ := mustCreate(t)(tn.CreateRelease(ctx, project, portableRelease(t, application, "info")))
	commit(t, tn)
	return policyFixture{
		org:          o,
		project:      project,
		environment:  env,
		application:  application,
		target:       tgt,
		release:      rel,
		revision:     rev.ID,
		lifecycleUID: state.LifecycleUID,
	}
}

func startPolicyRun(t *testing.T, s *store.Store, f policyFixture, expected uint64, reason store.RunReason) (ids.DeploymentRunID, uint8) {
	t.Helper()
	req := store.StartDeployment{
		Project:            f.project,
		Target:             f.target,
		Release:            f.release,
		ConfigRevision:     f.revision,
		ExpectedGeneration: target.Generation(expected),
		LifecycleUID:       f.lifecycleUID,
		Reason:             reason,
		RequestedBy:        "user:" + aliceUser,
		InputHash:          []byte{byte(expected)},
	}
	tn := tenant(t, s, f.org)
	started := must[store.Started](t, "start")(tn.StartDeployment(t.Context(), req, deploymentAudit(), opt.None[store.IdempotencyKey]()))
	commit(t, tn)
	acc := acceptedRun(t, started)
	return acc.Run, acc.ApprovalsRequired
}

func runApprovalState(t *testing.T, s *store.Store, f policyFixture, runID ids.DeploymentRunID) store.RunApproval {
	t.Helper()
	tn := tenant(t, s, f.org)
	appr, ok, err := tn.RunApproval(t.Context(), f.target, runID)
	if err != nil || !ok {
		t.Fatalf("run approval: %v, %v", ok, err)
	}
	commit(t, tn)
	return appr
}

func voteRun(
	t *testing.T,
	s *store.Store,
	f policyFixture,
	runID ids.DeploymentRunID,
	who string,
	dec policy.Decision,
	hash []byte,
) store.Decided {
	t.Helper()
	tn := tenant(t, s, f.org)
	decided, err := tn.DecideRun(t.Context(), f.target, runID, who, dec, hash, opt.Some("looks good"))
	if err != nil {
		t.Fatalf("decide run: %v", err)
	}
	commit(t, tn)
	return decided
}

func isClaimable(t *testing.T, s *store.Store) bool {
	t.Helper()
	claimed, ok, err := s.ClaimOperation(t.Context(), "w", []string{store.RunKind}, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = claimed
	return ok
}

func TestPoliciesAreRevisionsSeenOnlyByTheirOrganization(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newPolicyFixture(t, s, "a", opt.None[policy.EnvironmentPolicy]())

	tn := tenant(t, s, f.org)
	_, found, err := tn.EnvironmentPolicy(ctx, f.environment)
	if err != nil || found {
		t.Fatalf("expected none, got: %v, %v", found, err)
	}
	prod := policy.Production()
	strict := prod
	strict.RequiredApprovals = 2
	for _, tc := range []struct {
		p   policy.EnvironmentPolicy
		rev uint64
	}{
		{prod, 1},
		{strict, 2},
	} {
		rev, ok, err := tn.SetEnvironmentPolicy(ctx, f.project, f.environment, tc.p, "user:admin")
		if err != nil || !ok || rev != tc.rev {
			t.Fatalf("set policy: %v, %v, %v", rev, ok, err)
		}
	}
	newest, ok, err := tn.EnvironmentPolicy(ctx, f.environment)
	if err != nil || !ok {
		t.Fatalf("read policy: %v, %v", ok, err)
	}
	if newest.Revision != 2 || newest.Policy != strict {
		t.Fatalf("expected rev 2 strict, got: %v", newest)
	}
	tgtPol, ok, err := tn.PolicyOfTarget(ctx, f.target)
	if err != nil || !ok || tgtPol.Revision != 2 {
		t.Fatalf("policy of target: %v, %v, %v", tgtPol.Revision, ok, err)
	}
	_, ok, err = tn.SetEnvironmentPolicy(ctx, f.project, ids.New[ids.Environment](), strict, "user:admin")
	if err != nil || ok {
		t.Fatalf("missing env should be false: %v, %v", ok, err)
	}
	commit(t, tn)

	otherOrg := org(t, s, "b", "B")
	otherTn := tenant(t, s, otherOrg)
	_, found, err = otherTn.EnvironmentPolicy(ctx, f.environment)
	if err != nil || found {
		t.Fatalf("RLS: another org should not read: %v, %v", found, err)
	}
	_, ok, err = otherTn.SetEnvironmentPolicy(ctx, f.project, f.environment, policy.Open(), "user:evil")
	if err != nil || ok {
		t.Fatalf("RLS: another org cannot set: %v, %v", ok, err)
	}
	commit(t, otherTn)
}

func TestAProtectedDeployWaitsForAnotherPerson(t *testing.T) {
	s := pgtest.Store(t)
	f := newPolicyFixture(t, s, "a", opt.Some(policy.Production()))
	runID, required := startPolicyRun(t, s, f, 0, store.ReasonDeploy)
	if required != 1 {
		t.Fatalf("expected 1 required approval, got %d", required)
	}
	waiting := runApprovalState(t, s, f, runID)
	if waiting.Phase != run.AwaitingApproval {
		t.Fatalf("expected awaitingApproval, got %v", waiting.Phase)
	}
	if waiting.ExpiresAt == nil || *waiting.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expected future expiry, got %v", waiting.ExpiresAt)
	}
	if len(waiting.PlanHash) != 32 {
		t.Fatalf("expected 32 byte plan hash, got %d", len(waiting.PlanHash))
	}
	if isClaimable(t, s) {
		t.Fatal("parked run should not be claimable")
	}

	d := voteRun(t, s, f, runID, aliceUser, policy.Approve, waiting.PlanHash)
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrSelfApproval {
		t.Fatalf("expected ErrSelfApproval, got %#v", d)
	}
	d = voteRun(t, s, f, runID, bobUser, policy.Approve, []byte("stale"))
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrStalePlan {
		t.Fatalf("expected ErrStalePlan, got %#v", d)
	}
	if len(runApprovalState(t, s, f, runID).Decisions) != 0 {
		t.Fatal("refusals should record nothing")
	}

	d = voteRun(t, s, f, runID, bobUser, policy.Approve, waiting.PlanHash)
	if rec, ok := d.(store.DecidedRecorded); !ok {
		t.Fatalf("expected DecidedRecorded, got %#v", d)
	} else if _, ok := rec.Tally.(policy.Approved); !ok {
		t.Fatalf("expected Approved tally, got %#v", rec.Tally)
	}

	approved := runApprovalState(t, s, f, runID)
	if approved.Phase != run.PendingDelivery {
		t.Fatalf("expected pendingDelivery, got %v", approved.Phase)
	}
	if approved.Approved() != 1 {
		t.Fatalf("expected 1 approval, got %d", approved.Approved())
	}
	if len(approved.Decisions) != 1 || approved.Decisions[0].Comment == nil || *approved.Decisions[0].Comment != "looks good" {
		t.Fatalf("expected comment 'looks good', got %#v", approved.Decisions)
	}
	if !isClaimable(t, s) {
		t.Fatal("approved run should be woken and claimable")
	}

	d = voteRun(t, s, f, runID, carolUser, policy.Approve, waiting.PlanHash)
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrNotAwaiting {
		t.Fatalf("expected ErrNotAwaiting, got %#v", d)
	}
	d = voteRun(t, s, f, ids.New[ids.DeploymentRun](), bobUser, policy.Approve, waiting.PlanHash)
	if _, ok := d.(store.DecidedNotFound); !ok {
		t.Fatalf("expected DecidedNotFound, got %#v", d)
	}
}

func TestOneRejectionCancelsAndTwoApprovalsNeedTwoPeople(t *testing.T) {
	s := pgtest.Store(t)
	two := policy.Production()
	two.RequiredApprovals = 2
	f := newPolicyFixture(t, s, "a", opt.Some(two))

	first, _ := startPolicyRun(t, s, f, 0, store.ReasonDeploy)
	hash := runApprovalState(t, s, f, first).PlanHash
	d := voteRun(t, s, f, first, bobUser, policy.Approve, hash)
	if rec, ok := d.(store.DecidedRecorded); !ok {
		t.Fatalf("expected DecidedRecorded, got %#v", d)
	} else if w, ok := rec.Tally.(policy.Waiting); !ok || w.Remaining != 1 {
		t.Fatalf("expected Waiting{Remaining: 1}, got %#v", rec.Tally)
	}
	d = voteRun(t, s, f, first, bobUser, policy.Approve, hash)
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrAlreadyDecided {
		t.Fatalf("expected ErrAlreadyDecided, got %#v", d)
	}
	if runApprovalState(t, s, f, first).Phase != run.AwaitingApproval {
		t.Fatal("still awaiting approval")
	}
	d = voteRun(t, s, f, first, carolUser, policy.Approve, hash)
	if rec, ok := d.(store.DecidedRecorded); !ok {
		t.Fatalf("expected DecidedRecorded, got %#v", d)
	} else if _, ok := rec.Tally.(policy.Approved); !ok {
		t.Fatalf("expected Approved, got %#v", rec.Tally)
	}

	second, _ := startPolicyRun(t, s, f, 1, store.ReasonRollback)
	secondHash := runApprovalState(t, s, f, second).PlanHash
	d = voteRun(t, s, f, second, carolUser, policy.Reject, secondHash)
	if rec, ok := d.(store.DecidedRecorded); !ok {
		t.Fatalf("expected DecidedRecorded, got %#v", d)
	} else if _, ok := rec.Tally.(policy.Rejected); !ok {
		t.Fatalf("expected Rejected, got %#v", rec.Tally)
	}
	rejected := runApprovalState(t, s, f, second)
	if rejected.Phase != run.Cancelled {
		t.Fatalf("expected cancelled, got %v", rejected.Phase)
	}
}

func TestRestartsAndOpenEnvironmentsNeedNoApproval(t *testing.T) {
	s := pgtest.Store(t)
	open := newPolicyFixture(t, s, "a", opt.Some(policy.Open()))
	runID, required := startPolicyRun(t, s, open, 0, store.ReasonDeploy)
	if required != 0 {
		t.Fatalf("expected 0 required approvals, got %d", required)
	}
	st := runApprovalState(t, s, open, runID)
	if st.Phase != run.Planned || st.PlanHash != nil || st.ExpiresAt != nil {
		t.Fatalf("expected planned with no plan hash/expiry, got %#v", st)
	}

	unset := newPolicyFixture(t, s, "b", opt.None[policy.EnvironmentPolicy]())
	_, reqUnset := startPolicyRun(t, s, unset, 0, store.ReasonDeploy)
	if reqUnset != 0 {
		t.Fatalf("expected 0 for unset, got %d", reqUnset)
	}

	protected := newPolicyFixture(t, s, "c", opt.Some(policy.Production()))
	restartRun, reqRestart := startPolicyRun(t, s, protected, 0, store.ReasonRestart)
	if reqRestart != 0 {
		t.Fatalf("restart needs no approvals, got %d", reqRestart)
	}
	if runApprovalState(t, s, protected, restartRun).Phase != run.Planned {
		t.Fatalf("restart should be Planned")
	}
}

func TestANewerRunSupersedesAWaitingOne(t *testing.T) {
	s := pgtest.Store(t)
	f := newPolicyFixture(t, s, "a", opt.Some(policy.Production()))
	older, _ := startPolicyRun(t, s, f, 0, store.ReasonDeploy)
	hash := runApprovalState(t, s, f, older).PlanHash

	newer, _ := startPolicyRun(t, s, f, 1, store.ReasonDeploy)
	if runApprovalState(t, s, f, older).Phase != run.Superseded {
		t.Fatal("older run should be superseded")
	}
	if !isClaimable(t, s) {
		t.Fatal("superseded run should be woken to settle")
	}
	d := voteRun(t, s, f, older, bobUser, policy.Approve, hash)
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrNotAwaiting {
		t.Fatalf("expected ErrNotAwaiting, got %#v", d)
	}
	newerHash := runApprovalState(t, s, f, newer).PlanHash
	if bytes.Equal(newerHash, hash) {
		t.Fatal("every run has its own plan hash")
	}
	d = voteRun(t, s, f, newer, bobUser, policy.Approve, hash)
	if ref, ok := d.(store.DecidedRefused); !ok || ref.Err != policy.ErrStalePlan {
		t.Fatalf("expected ErrStalePlan, got %#v", d)
	}
}

func TestApprovalInputsAndDecisionsNeverChange(t *testing.T) {
	s := pgtest.Store(t)
	f := newPolicyFixture(t, s, "a", opt.Some(policy.Production()))
	runID, _ := startPolicyRun(t, s, f, 0, store.ReasonDeploy)
	hash := runApprovalState(t, s, f, runID).PlanHash
	voteRun(t, s, f, runID, bobUser, policy.Approve, hash)

	for _, sqlStmt := range []string{
		"UPDATE deployment_runs SET approvals_required = 0 WHERE id = $1",
		"UPDATE deployment_runs SET approval_expires_at = approval_expires_at + 1 WHERE id = $1",
		"UPDATE run_approvals SET decision = 'rejected' WHERE run_id = $1",
		"DELETE FROM run_approvals WHERE run_id = $1",
	} {
		tn := tenant(t, s, f.org)
		if _, err := tn.TestExec(t.Context(), sqlStmt, runID); err == nil {
			t.Fatalf("append-only trigger should prevent: %s", sqlStmt)
		}
		rollback(t, tn)
	}

	tn := tenant(t, s, f.org)
	if _, err := tn.TestExec(t.Context(), "UPDATE environment_policies SET required_approvals = 0"); err == nil {
		t.Fatal("policy revisions are append-only")
	}
	rollback(t, tn)
}

func createUser(t *testing.T, s *store.Store, email string) ids.UserID {
	t.Helper()
	u, err := s.CreateUser(t.Context(), email, opt.None[string](), opt.None[string]())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

func TestScopedRolesAreOnePerNodeAndOnlyForMembers(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newPolicyFixture(t, s, "a", opt.None[policy.EnvironmentPolicy]())

	member := createUser(t, s, "dev@example.com")
	outsider := createUser(t, s, "out@example.com")
	if err := s.AddMembership(ctx, f.org, member); err != nil {
		t.Fatalf("member: %v", err)
	}
	if err := s.BindOrgRole(ctx, f.org, member, perm.Viewer); err != nil {
		t.Fatalf("org role: %v", err)
	}
	node := f.project.UUID()

	bound, err := s.BindScopedRole(ctx, f.org, outsider, model.ScopeProject, node, perm.Admin)
	if err != nil || bound {
		t.Fatalf("no membership, no role: %v, %v", bound, err)
	}
	for _, r := range []perm.Role{perm.Developer, perm.Admin} {
		bound, err = s.BindScopedRole(ctx, f.org, member, model.ScopeProject, node, r)
		if err != nil || !bound {
			t.Fatalf("bind scoped role: %v, %v", bound, err)
		}
	}
	scoped, err := s.ScopedMembers(ctx, f.org, model.ScopeProject, node)
	if err != nil || len(scoped) != 1 || scoped[0].Role != perm.Admin {
		t.Fatalf("scoped members updated in place: %#v, %v", scoped, err)
	}
	bindings, err := s.BindingsForUser(ctx, member)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("expected 2 bindings: %#v, %v", bindings, err)
	}
	if _, err := s.BindScopedRole(ctx, f.org, member, model.ScopeOrg, node, perm.Owner); err == nil {
		t.Fatal("org roles are not scoped")
	}
	unbound, err := s.UnbindScopedRole(ctx, f.org, member, model.ScopeProject, node)
	if err != nil || !unbound {
		t.Fatalf("unbind: %v, %v", unbound, err)
	}
	unbound, err = s.UnbindScopedRole(ctx, f.org, member, model.ScopeProject, node)
	if err != nil || unbound {
		t.Fatalf("unbind again should be false: %v, %v", unbound, err)
	}
}

func TestRemovingAMemberRevokesTheirTokensWithTheMembership(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newPolicyFixture(t, s, "a", opt.None[policy.EnvironmentPolicy]())

	member := createUser(t, s, "dev@example.com")
	if err := s.AddMembership(ctx, f.org, member); err != nil {
		t.Fatalf("member: %v", err)
	}
	if err := s.BindOrgRole(ctx, f.org, member, perm.Developer); err != nil {
		t.Fatalf("org role: %v", err)
	}
	if _, err := s.BindScopedRole(ctx, f.org, member, model.ScopeEnvironment, f.environment.UUID(), perm.Admin); err != nil {
		t.Fatalf("scoped: %v", err)
	}

	token, err := s.CreateToken(ctx, store.NewToken{
		ID:         ids.New[ids.Token](),
		OrgID:      f.org,
		Owner:      member,
		Name:       "ci",
		Prefix:     "kbn_pat_test",
		SecretHash: []byte("hash"),
		Scope: model.TokenScope{
			Role: perm.Developer,
		},
	})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := s.RemoveMember(ctx, f.org, member); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	bindings, err := s.BindingsForUser(ctx, member)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("bindings after removal should be empty: %#v, %v", bindings, err)
	}
	tok, found, err := s.FindToken(ctx, token.ID)
	if err != nil || !found {
		t.Fatalf("token: %v, %v", found, err)
	}
	if _, ok := tok.RevokedAt.Get(); !ok {
		t.Fatal("token should be revoked")
	}
}

package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from deployments.rs, plus the run reasons' tables.

type deployFixture struct {
	org              ids.OrgID
	project          ids.ProjectID
	application      ids.ApplicationID
	otherApplication ids.ApplicationID
	target           ids.TargetID
	lifecycleUID     uuid.UUID
}

func newDeployFixture(t *testing.T, s *store.Store) deployFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, "shop-production"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	other := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "api", "API"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	state, ok, err := tn.TargetState(ctx, tgt)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	commit(t, tn)
	return deployFixture{
		org: o, project: project, application: application, otherApplication: other, target: tgt,
		lifecycleUID: state.LifecycleUID,
	}
}

func digestOf(t *testing.T, n byte) artifact.Digest {
	t.Helper()
	return parseDigest(t, "sha256:"+strings.Repeat(fmt.Sprintf("%02x", n), 32))
}

func portableRelease(t *testing.T, application ids.ApplicationID, logLevel string) store.PortableRelease {
	t.Helper()
	return store.PortableRelease{
		Application:     application,
		Artifacts:       map[string]artifact.Digest{"web": digestOf(t, 1)},
		ProcessContract: map[string]any{"web": map[string]any{"port": 8080}},
		PortableConfig:  map[string]any{"LOG_LEVEL": logLevel},
		RendererSchema:  1,
		CreatedBy:       "user:alice",
	}
}

func deploymentAudit() store.NewAudit {
	return store.NewAudit{
		ActorKind: "user", ActorID: opt.Some("alice"), Action: "deployment.accepted", Outcome: "accepted",
	}
}

// deployInputs is a release and configuration revision for the fixture's
// target.
func deployInputs(t *testing.T, s *store.Store, f deployFixture) (ids.ReleaseID, ids.ConfigRevisionID) {
	t.Helper()
	tn := tenant(t, s, f.org)
	release, _ := mustCreate(t)(tn.CreateRelease(t.Context(), f.project, portableRelease(t, f.application, "info")))
	revision := configRevision(t, tn, f.project, f.target, map[string]any{"replicas": 2})
	commit(t, tn)
	return release, revision.ID
}

func startRequest(f deployFixture, release ids.ReleaseID, revision ids.ConfigRevisionID, expected uint64, reason store.RunReason) store.StartDeployment {
	return store.StartDeployment{
		Project: f.project, Target: f.target, Release: release, ConfigRevision: revision,
		ExpectedGeneration: target.Generation(expected), LifecycleUID: f.lifecycleUID, Reason: reason,
		RequestedBy: "user:alice", InputHash: fmt.Appendf(nil, "%s/%s/%d", release, revision, expected),
	}
}

// startRun starts req in its own committed transaction.
func startRun(t *testing.T, s *store.Store, o ids.OrgID, req store.StartDeployment, key opt.Val[store.IdempotencyKey]) store.Started {
	t.Helper()
	tn := tenant(t, s, o)
	started := must[store.Started](t, "start")(tn.StartDeployment(t.Context(), req, deploymentAudit(), key))
	commit(t, tn)
	return started
}

func acceptedRun(t *testing.T, started store.Started) store.StartedAccepted {
	t.Helper()
	a, ok := started.(store.StartedAccepted)
	if !ok {
		t.Fatalf("not accepted: %#v", started)
	}
	return a
}

func TestReleasesAreDeduplicatedAndImmutable(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newDeployFixture(t, s)
	tn := tenant(t, s, f.org)
	first, created := mustCreate(t)(tn.CreateRelease(ctx, f.project, portableRelease(t, f.application, "info")))
	if !created {
		t.Fatal("a first release is created")
	}
	if again, created := mustCreate(t)(tn.CreateRelease(ctx, f.project, portableRelease(t, f.application, "info"))); again != first || created {
		t.Fatalf("the same content is the same release: %v, %v", again, created)
	}
	if other, created := mustCreate(t)(tn.CreateRelease(ctx, f.project, portableRelease(t, f.application, "debug"))); !created || other == first {
		t.Fatalf("another release: %v, %v", other, created)
	}
	firstRevision := configRevision(t, tn, f.project, f.target, map[string]any{})
	secondRevision := configRevision(t, tn, f.project, f.target, map[string]any{})
	if firstRevision.Revision != 1 || secondRevision.Revision != 2 {
		t.Fatalf("revisions: %d, %d", firstRevision.Revision, secondRevision.Revision)
	}
	resources := []any{map[string]any{"kind": "Deployment"}}
	plan := must[ids.RenderPlanID](t, "plan")(tn.FreezeRenderPlan(ctx, "renderer/1", map[string]any{}, resources))
	if again := must[ids.RenderPlanID](t, "plan again")(tn.FreezeRenderPlan(ctx, "renderer/1", map[string]any{}, resources)); again != plan {
		t.Fatalf("the same plan: %v, %v", again, plan)
	}
	commit(t, tn)
	for _, table := range []string{"releases", "target_config_revisions", "render_plans"} {
		if _, err := s.TestExec(ctx, "UPDATE "+table+" SET created_at = 0"); err == nil {
			t.Fatalf("%s is append-only", table)
		}
	}
}

func TestDeploymentsRaiseTheGenerationAndSupersedeOlderRuns(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newDeployFixture(t, s)
	release, revision := deployInputs(t, s, f)
	none := opt.None[store.IdempotencyKey]()

	first := acceptedRun(t, startRun(t, s, f.org, startRequest(f, release, revision, 0, store.ReasonDeploy), none))
	if first.Generation != 1 {
		t.Fatalf("generation: %d", first.Generation)
	}
	stale := startRun(t, s, f.org, startRequest(f, release, revision, 0, store.ReasonDeploy), none)
	if stale != (store.StartedRejected{Reject: target.GenerationMoved{Current: 1, Expected: 0}}) {
		t.Fatalf("a stale expected generation: %#v", stale)
	}
	second := acceptedRun(t, startRun(t, s, f.org, startRequest(f, release, revision, 1, store.ReasonDeploy), none))
	if second.Generation != 2 {
		t.Fatalf("generation: %d", second.Generation)
	}

	tn := tenant(t, s, f.org)
	state, ok, err := tn.RunPhase(ctx, first.Run)
	if err != nil || !ok || state != (store.RunState{Phase: run.Superseded, Generation: 1}) {
		t.Fatalf("the newer run owns the target: %+v, %v, %v", state, ok, err)
	}
	rollback(t, tn)

	rolledBack := acceptedRun(t, startRun(t, s, f.org, startRequest(f, release, revision, 2, store.ReasonRollback), none))
	if rolledBack.Generation != 3 {
		t.Fatalf("generation: %d", rolledBack.Generation)
	}
	tn = tenant(t, s, f.org)
	ts, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok || ts.DesiredGeneration != 3 || ts.Policy != target.Pinned {
		t.Fatalf("a rollback pins the target: %+v, %v, %v", ts, ok, err)
	}
	rollback(t, tn)

	wrongLifecycle := startRequest(f, release, revision, 3, store.ReasonDeploy)
	wrongLifecycle.LifecycleUID = uuid.Must(uuid.NewV7())
	if got := startRun(t, s, f.org, wrongLifecycle, none); got != (store.StartedRejected{Reject: target.LifecycleMismatch{}}) {
		t.Fatalf("lifecycle: %#v", got)
	}

	tn = tenant(t, s, f.org)
	foreign, _ := mustCreate(t)(tn.CreateRelease(ctx, f.project, portableRelease(t, f.otherApplication, "info")))
	commit(t, tn)
	if got := startRun(t, s, f.org, startRequest(f, foreign, revision, 3, store.ReasonDeploy), none); got != (store.StartedNotFound{}) {
		t.Fatalf("a release of another application: %#v", got)
	}
}

func TestAReplayedKeyReturnsTheFirstReceipt(t *testing.T) {
	s := pgtest.Store(t)
	f := newDeployFixture(t, s)
	release, revision := deployInputs(t, s, f)
	key := opt.Some(store.IdempotencyKey{Actor: "user:alice", Key: "deploy-1", TTL: time.Hour})
	req := startRequest(f, release, revision, 0, store.ReasonDeploy)
	operation := acceptedRun(t, startRun(t, s, f.org, req, key)).Operation
	if got := startRun(t, s, f.org, req, key); got != (store.StartedReplayed{Operation: operation}) {
		t.Fatalf("the same request again, although generation 0 is stale by now: %#v", got)
	}
	other := startRequest(f, release, revision, 1, store.ReasonDeploy)
	if got := startRun(t, s, f.org, other, key); got != (store.StartedKeyReused{Operation: operation}) {
		t.Fatalf("another request: %#v", got)
	}
}

func advance(t *testing.T, s *store.Store, c store.Claim, r ids.DeploymentRunID, event run.Event) store.Advance {
	t.Helper()
	return must[store.Advance](t, "advance")(s.AdvanceRun(t.Context(), c, r, event))
}

func TestRunPhasesFollowTheStateMachineUnderTheFence(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newDeployFixture(t, s)
	release, revision := deployInputs(t, s, f)
	runID := acceptedRun(t, startRun(t, s, f.org, startRequest(f, release, revision, 0, store.ReasonDeploy),
		opt.None[store.IdempotencyKey]())).Run
	slow, ok := claim(t, s, "slow", store.RunKind)
	if !ok {
		t.Fatal("due")
	}
	if got := advance(t, s, slow, runID, run.EventReadyForDelivery); got != (store.AdvanceMoved{Phase: run.PendingDelivery}) {
		t.Fatalf("ready: %#v", got)
	}
	if _, illegal := advance(t, s, slow, runID, run.EventVerified).(store.AdvanceIllegal); !illegal {
		t.Fatal("verified before delivery")
	}

	if _, err := s.TestExec(ctx, "UPDATE operations SET lease_until = 0 WHERE id = $1", slow.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	fast, ok := claim(t, s, "fast", store.RunKind)
	if !ok {
		t.Fatal("taken over")
	}
	if got := advance(t, s, slow, runID, run.EventAcceptedByCluster); got != (store.AdvanceFenced{}) {
		t.Fatalf("slow: %#v", got)
	}
	if got := advance(t, s, fast, runID, run.EventAcceptedByCluster); got != (store.AdvanceMoved{Phase: run.AcceptedByCluster}) {
		t.Fatalf("fast: %#v", got)
	}
}

func TestRunReasonsMapToTheirChangeKinds(t *testing.T) {
	for _, c := range []struct {
		reason  store.RunReason
		kind    policy.ChangeKind
		newCode bool
	}{
		{store.ReasonDeploy, policy.Deploy, true},
		{store.ReasonRollback, policy.Rollback, true},
		{store.ReasonPromotion, policy.Promotion, true},
		{store.ReasonRestart, policy.Restart, false},
		{store.ReasonHandover, policy.Handover, false},
		{store.ReasonBuild, policy.Build, true},
		{store.ReasonEmergency, policy.Emergency, true},
		{store.ReasonRotation, policy.Rotation, false},
	} {
		if c.reason.ChangeKind() != c.kind || c.reason.CarriesNewCode() != c.newCode {
			t.Fatalf("%s: %s, %v", c.reason, c.reason.ChangeKind(), c.reason.CarriesNewCode())
		}
		if string(c.reason) != string(c.kind) {
			t.Fatalf("%s is stored as %s", c.reason, c.kind)
		}
	}
}

func TestAnEmergencyNeedsItsReason(t *testing.T) {
	s := pgtest.Store(t)
	f := newDeployFixture(t, s)
	release, revision := deployInputs(t, s, f)
	tn := tenant(t, s, f.org)
	_, err := tn.StartDeployment(t.Context(), startRequest(f, release, revision, 0, store.ReasonEmergency),
		deploymentAudit(), opt.None[store.IdempotencyKey]())
	var dbErr store.DatabaseError
	if !errors.As(err, &dbErr) || !strings.Contains(err.Error(), "an emergency rollback needs its reason") {
		t.Fatalf("an emergency through StartDeployment: %v", err)
	}
}

// The texts serde_json hashed: a byte of difference would make every stored
// release and plan look new.
func TestContentAddressesAreSerdeText(t *testing.T) {
	release := portableRelease(t, ids.New[ids.Application](), "info")
	release.ProcessContract = map[string]any{"web": map[string]any{"port": json.Number("8080"), "ratio": json.Number("1.0")}}
	content, err := store.ReleaseContent(release)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	want := `{"artifacts":{"web":"sha256:` + strings.Repeat("01", 32) + `"},"portable_config":{"LOG_LEVEL":"info"},` +
		`"process_contract":{"web":{"port":8080,"ratio":1.0}},"renderer_schema":1}`
	if content != want {
		t.Fatalf("release content:\n got %s\nwant %s", content, want)
	}
	plan, err := store.PlanContent("renderer/1", map[string]any{}, []any{map[string]any{"kind": "Deployment"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if want := `{"capability_snapshot":{},"renderer_version":"renderer/1","resources":[{"kind":"Deployment"}]}`; plan != want {
		t.Fatalf("plan content: %s", plan)
	}
}

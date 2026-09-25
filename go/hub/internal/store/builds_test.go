package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from repo/builds.rs.

const (
	headA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	buildDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var buildLimits = store.SlotLimits{Total: 4, PerOrg: 2}

type buildFixture struct {
	org     ids.OrgID
	project ids.ProjectID
	target  ids.TargetID
	binding store.SourceBinding
}

func bindingInput(t *testing.T) store.NewBinding {
	t.Helper()
	repo, err := source.ParseRepoName("acme/shop")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	branch, err := source.ParseBranchName("main")
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	return store.NewBinding{
		InstallationID:  7,
		Repository:      repo,
		Branch:          branch,
		Recipe:          source.BuildRecipe{Strategy: source.Auto},
		ImageRepository: "registry.local/acme/shop",
	}
}

func newBuildFixture(t *testing.T, s *store.Store, slug string, installation uint64) buildFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, slug+"-shop"))
	app := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, app, placement))
	configRevision(t, tn, project, tgt, map[string]any{"replicas": 1})
	if !must[bool](t, "link")(tn.LinkInstallation(ctx, installation, "acme")) {
		t.Fatal("link refused")
	}
	input := bindingInput(t)
	input.InstallationID = installation
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, project, tgt, input)); !isBound[store.BoundCreated](bound) {
		t.Fatalf("not created: %#v", bound)
	}
	binding, ok, err := tn.BindingOfTarget(ctx, tgt)
	if err != nil || !ok {
		t.Fatalf("binding: %v, %v", ok, err)
	}
	commit(t, tn)
	return buildFixture{org: o, project: project, target: tgt, binding: binding}
}

func isBound[B store.Bound](b store.Bound) bool {
	_, ok := b.(B)
	return ok
}

func commitSha(t *testing.T, text string) source.CommitSha {
	t.Helper()
	sha, err := source.ParseCommitSha(text)
	if err != nil {
		t.Fatalf("sha: %v", err)
	}
	return sha
}

// requestSync asks for a sync of f's binding in its own transaction.
func requestSync(t *testing.T, s *store.Store, f buildFixture) {
	t.Helper()
	tn := tenant(t, s, f.org)
	must[ids.OperationID](t, "sync")(tn.RequestSync(t.Context(), f.binding, "test", store.NewAudit{}))
	commit(t, tn)
}

// syncHead runs one source sync that reads head from the provider.
func syncHead(t *testing.T, s *store.Store, f buildFixture, head string) store.HeadObserved {
	t.Helper()
	ctx := t.Context()
	requestSync(t, s, f)
	c, ok := claim(t, s, "w", store.SourceSyncKind)
	if !ok {
		t.Fatal("no sync due")
	}
	tn := tenant(t, s, c.Org)
	binding, ok, err := tn.SyncBinding(ctx, c)
	if err != nil || !ok {
		t.Fatalf("sync binding: %v, %v", ok, err)
	}
	observed := must[store.HeadObserved](t, "observe")(tn.ObserveHead(ctx, c, binding, commitSha(t, head), 42))
	commit(t, tn)
	if _, err := s.FinishOperation(ctx, c, "succeeded", opt.None[string]()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return observed
}

func queuedEpoch(t *testing.T, observed store.HeadObserved) target.SourceEpoch {
	t.Helper()
	q, ok := observed.(store.HeadQueued)
	if !ok {
		t.Fatalf("not queued: %#v", observed)
	}
	return q.Epoch
}

func claimBuild(t *testing.T, s *store.Store) (store.Claim, store.BuildAttempt) {
	t.Helper()
	c, ok := claim(t, s, "w", store.BuildKind)
	if !ok {
		t.Fatal("no build due")
	}
	tn := tenant(t, s, c.Org)
	attempt, ok, err := tn.BuildOfOperation(t.Context(), c.ID)
	if err != nil || !ok {
		t.Fatalf("attempt: %v, %v", ok, err)
	}
	commit(t, tn)
	return c, attempt
}

func step(t *testing.T, s *store.Store, c store.Claim, attempt store.BuildAttempt, event build.Event) store.BuildAdvance {
	t.Helper()
	tn := tenant(t, s, c.Org)
	moved := must[store.BuildAdvance](t, "advance")(tn.AdvanceBuild(t.Context(), c, attempt.ID, event, store.BuildProgress{}))
	commit(t, tn)
	return moved
}

func runToVerifying(t *testing.T, s *store.Store, c store.Claim, attempt store.BuildAttempt) {
	t.Helper()
	for _, event := range []build.Event{build.EventStarted, build.EventBuilding, build.EventPublishing, build.EventPublished} {
		if moved := step(t, s, c, attempt, event); !isMoved(moved) {
			t.Fatalf("%s: %#v", event, moved)
		}
	}
}

func isMoved(a store.BuildAdvance) bool {
	_, ok := a.(store.BuildMoved)
	return ok
}

func claimSlot(t *testing.T, tn *store.Tenant, c store.Claim, attempt ids.BuildAttemptID, limits store.SlotLimits) bool {
	t.Helper()
	granted, live, err := tn.ClaimBuildSlot(t.Context(), c, attempt, limits)
	if err != nil || !live {
		t.Fatalf("slot: %v, %v", live, err)
	}
	return granted
}

func holdsSlot(t *testing.T, tn *store.Tenant, attempt ids.BuildAttemptID) bool {
	t.Helper()
	var held bool
	if err := tn.TestQueryRow(t.Context(), store.HasSlot, attempt).Scan(&held); err != nil {
		t.Fatalf("slot: %v", err)
	}
	return held
}

func storedBuild(t *testing.T, tn *store.Tenant, tgt ids.TargetID, attempt ids.BuildAttemptID) store.BuildAttempt {
	t.Helper()
	a, ok, err := tn.BuildOfTarget(t.Context(), tgt, attempt)
	if err != nil || !ok {
		t.Fatalf("attempt: %v, %v", ok, err)
	}
	return a
}

func complete(t *testing.T, tn *store.Tenant, c store.Claim, attempt store.BuildAttempt) store.Completed {
	t.Helper()
	digest, err := artifact.ParseDigest(buildDigest)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return must[store.Completed](t, "complete")(tn.CompleteBuild(t.Context(), c, attempt, digest))
}

func keptFor(t *testing.T, done store.Completed, decision string) {
	t.Helper()
	if kept, ok := done.(store.CompletedKept); !ok || kept.Decision != decision {
		t.Fatalf("not kept for %s: %#v", decision, done)
	}
}

func TestInstallationsNeverMoveBetweenOrganizations(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	other := org(t, s, "b", "B")
	tn := tenant(t, s, other)
	if must[bool](t, "link")(tn.LinkInstallation(ctx, 7, "evil")) {
		t.Fatal("held by another org")
	}
	if refused := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, bindingInput(t))); refused != (store.BoundInstallationMissing{}) {
		t.Fatalf("bound: %#v", refused)
	}
	rollback(t, tn)
	link, ok, err := s.GitInstallationOrg(ctx, 7)
	if err != nil || !ok || link != (store.InstallationLink{Org: f.org}) {
		t.Fatalf("link: %#v, %v, %v", link, ok, err)
	}
	if !must[bool](t, "suspend")(s.SetInstallationSuspended(ctx, 7, true)) {
		t.Fatal("not suspended")
	}
	link, ok, err = s.GitInstallationOrg(ctx, 7)
	if err != nil || !ok || link != (store.InstallationLink{Org: f.org, Suspended: true}) {
		t.Fatalf("link: %#v, %v, %v", link, ok, err)
	}
}

func TestTheSameHeadTwiceBuildsOnce(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	if epoch := queuedEpoch(t, syncHead(t, s, f, headA)); epoch != 1 {
		t.Fatalf("epoch %d", epoch)
	}
	if again := syncHead(t, s, f, headA); again != (store.HeadUnchanged{}) {
		t.Fatalf("a redelivery: %#v", again)
	}
	tn := tenant(t, s, f.org)
	if builds := must[[]store.BuildAttempt](t, "list")(tn.BuildsOfTarget(ctx, f.target, 10)); len(builds) != 1 {
		t.Fatalf("builds: %d", len(builds))
	}
	repo, err := source.ParseRepoName("Acme/Shop")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	pushes := must[[]store.SourceBinding](t, "push")(tn.BindingsForPush(ctx, 7, repo, f.binding.Branch))
	if len(pushes) != 1 {
		t.Fatalf("pushes: %d", len(pushes))
	}
	if head, _ := pushes[0].HeadSha.Get(); head.String() != headA {
		t.Fatalf("head %v", pushes[0].HeadSha)
	}
	if pushes[0].RepositoryID != opt.Some[uint64](42) {
		t.Fatalf("repository id %v", pushes[0].RepositoryID)
	}
}

func TestANewHeadAsksOlderBuildsToStop(t *testing.T) {
	s := pgtest.Store(t)
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	if epoch := queuedEpoch(t, syncHead(t, s, f, headB)); epoch != 2 {
		t.Fatalf("epoch %d", epoch)
	}
	tn := tenant(t, s, f.org)
	builds := must[[]store.BuildAttempt](t, "list")(tn.BuildsOfTarget(t.Context(), f.target, 10))
	if len(builds) != 2 {
		t.Fatalf("builds: %d", len(builds))
	}
	newer, older := builds[0], builds[1]
	if newer.Commit.String() != headB || newer.CancelRequested {
		t.Fatalf("newer: %s, %v", newer.Commit, newer.CancelRequested)
	}
	if !older.CancelRequested {
		t.Fatal("the older build is asked to stop")
	}
	rollback(t, tn)
	// A force-push back to A is a new head too.
	if epoch := queuedEpoch(t, syncHead(t, s, f, headA)); epoch != 3 {
		t.Fatalf("epoch %d", epoch)
	}
}

func TestAVerifiedBuildOfTheCurrentHeadDeploys(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	tn := tenant(t, s, f.org)
	if !claimSlot(t, tn, c, attempt.ID, buildLimits) {
		t.Fatal("no slot")
	}
	commit(t, tn)
	runToVerifying(t, s, c, attempt)
	tn = tenant(t, s, f.org)
	done := complete(t, tn, c, attempt)
	deployed, ok := done.(store.CompletedDeployed)
	if !ok {
		t.Fatalf("not deployed: %#v", done)
	}
	if deployed.Generation != 1 {
		t.Fatalf("generation %d", deployed.Generation)
	}
	stored := storedBuild(t, tn, f.target, attempt.ID)
	if stored.Phase != build.Succeeded || stored.Run != opt.Some(deployed.Run) ||
		stored.DeployDecision != opt.Some("deployed") || stored.FinishedAt.IsNone() {
		t.Fatalf("stored: %#v", stored)
	}
	var reason string
	if err := tn.TestQueryRow(ctx, "SELECT reason FROM deployment_runs WHERE id = $1", deployed.Run).Scan(&reason); err != nil {
		t.Fatalf("run: %v", err)
	}
	if reason != "build" {
		t.Fatalf("reason %s", reason)
	}
	if holdsSlot(t, tn, attempt.ID) {
		t.Fatal("the slot went with the terminal phase")
	}
}

func TestALateBuildOfAnOldHeadIsKeptButNotDeployed(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	runToVerifying(t, s, c, attempt)
	// B is pushed while A publishes; A's output is verified first anyway.
	requestSync(t, s, f)
	syncClaim, ok := claim(t, s, "w2", store.SourceSyncKind)
	if !ok {
		t.Fatal("no sync due")
	}
	tn := tenant(t, s, f.org)
	must[store.HeadObserved](t, "observe")(tn.ObserveHead(ctx, syncClaim, f.binding, commitSha(t, headB), 42))
	commit(t, tn)

	tn = tenant(t, s, f.org)
	keptFor(t, complete(t, tn, c, attempt), "StaleSource")
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	if state.DesiredGeneration != 0 {
		t.Fatalf("production did not move back: %d", state.DesiredGeneration)
	}
}

func TestAPinnedOrReconfiguredTargetKeepsTheRelease(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	runToVerifying(t, s, c, attempt)
	tn := tenant(t, s, f.org)
	changed := bindingInput(t)
	changed.Recipe = source.BuildRecipe{Strategy: source.Railpack}
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, changed)); !isBound[store.BoundChanged](bound) {
		t.Fatalf("not changed: %#v", bound)
	}
	keptFor(t, complete(t, tn, c, attempt), "BuildConfigChanged")
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	if state.Policy != target.Auto {
		t.Fatalf("policy %s", state.Policy)
	}
}

func TestSlotsAreLimitedPerOrgAndReleasedWhenFinal(t *testing.T) {
	s := pgtest.Store(t)
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	one := store.SlotLimits{Total: 1, PerOrg: 1}
	zero := store.SlotLimits{}
	tn := tenant(t, s, f.org)
	if claimSlot(t, tn, c, attempt.ID, zero) {
		t.Fatal("zero limits grant nothing")
	}
	if !claimSlot(t, tn, c, attempt.ID, one) {
		t.Fatal("the free slot")
	}
	if !claimSlot(t, tn, c, attempt.ID, one) {
		t.Fatal("idempotent for its holder")
	}
	commit(t, tn)

	g := newBuildFixture(t, s, "b", 8)
	syncHead(t, s, g, headA)
	otherClaim, other := claimBuild(t, s)
	tn = tenant(t, s, g.org)
	if claimSlot(t, tn, otherClaim, other.ID, one) {
		t.Fatal("the only slot is taken")
	}
	commit(t, tn)

	if moved := step(t, s, c, attempt, build.EventCancelRequested); moved != (store.BuildMoved{Phase: build.Cancelled}) {
		t.Fatalf("moved: %#v", moved)
	}
	tn = tenant(t, s, g.org)
	if !claimSlot(t, tn, otherClaim, other.ID, one) {
		t.Fatal("released by the terminal phase")
	}
}

func TestFencedWorkersAndFinalAttemptsChangeNothing(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	stale := c
	stale.Fence--
	if moved := step(t, s, stale, attempt, build.EventStarted); moved != (store.BuildFenced{}) {
		t.Fatalf("stale: %#v", moved)
	}
	failed := store.BuildProgress{
		Failure: opt.Some(store.BuildFailureReport{Code: "OutOfMemory", Detail: strings.Repeat("x", 5000)}),
	}
	tn := tenant(t, s, f.org)
	if moved := must[store.BuildAdvance](t, "fail")(tn.AdvanceBuild(ctx, c, attempt.ID, build.EventFailed, failed)); moved != (store.BuildMoved{Phase: build.Failed}) {
		t.Fatalf("moved: %#v", moved)
	}
	stored := storedBuild(t, tn, f.target, attempt.ID)
	if stored.Failure != opt.Some("OutOfMemory") {
		t.Fatalf("failure %v", stored.Failure)
	}
	if detail, _ := stored.FailureDetail.Get(); len(detail) != 2048 {
		t.Fatalf("detail of %d bytes", len(detail))
	}
	again := must[store.BuildAdvance](t, "illegal")(tn.AdvanceBuild(ctx, c, attempt.ID, build.EventStarted, store.BuildProgress{}))
	if illegal, ok := again.(store.BuildIllegal); !ok || !illegal.Err.Terminal {
		t.Fatalf("again: %#v", again)
	}
	if _, err := tn.TestExec(ctx, "UPDATE build_attempts SET phase = 'running' WHERE id = $1", attempt.ID); err == nil {
		t.Fatal("the database refuses to reopen a final attempt")
	}
}

func TestALostWorkerIsRetriedAsANewAttempt(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	for round := uint32(1); round <= store.MaxBuildAttempts; round++ {
		c, attempt := claimBuild(t, s)
		if attempt.AttemptNo != round {
			t.Fatalf("attempt %d in round %d", attempt.AttemptNo, round)
		}
		step(t, s, c, attempt, build.EventFailed)
		if _, err := s.FinishOperation(ctx, c, "failed", opt.Some("LostWorker")); err != nil {
			t.Fatalf("finish: %v", err)
		}
		tn := tenant(t, s, f.org)
		_, retried, err := tn.RetryBuild(ctx, attempt)
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if retried != (round < store.MaxBuildAttempts) {
			t.Fatalf("round %d: retried %v", round, retried)
		}
		commit(t, tn)
	}
}

func TestARunningBuildIsCancelledOnlyAfterItsJobIsGone(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	tn := tenant(t, s, f.org)
	if !claimSlot(t, tn, c, attempt.ID, buildLimits) {
		t.Fatal("no slot")
	}
	commit(t, tn)
	step(t, s, c, attempt, build.EventStarted)
	step(t, s, c, attempt, build.EventBuilding)
	tn = tenant(t, s, f.org)
	if _, ok, err := tn.RequestBuildCancel(ctx, f.target, attempt.ID); err != nil || !ok {
		t.Fatalf("cancel: %v, %v", ok, err)
	}
	commit(t, tn)
	for _, tc := range []struct {
		event build.Event
		phase build.Phase
	}{
		{build.EventCancelRequested, build.CancelRequested},
		{build.EventStopping, build.Cancelling},
	} {
		if moved := step(t, s, c, attempt, tc.event); moved != (store.BuildMoved{Phase: tc.phase}) {
			t.Fatalf("%s: %#v", tc.event, moved)
		}
	}
	tn = tenant(t, s, f.org)
	if !holdsSlot(t, tn, attempt.ID) {
		t.Fatal("the Job may still run while it is being deleted")
	}
	rollback(t, tn)
	if moved := step(t, s, c, attempt, build.EventStopped); moved != (store.BuildMoved{Phase: build.Cancelled}) {
		t.Fatalf("stopped: %#v", moved)
	}
	tn = tenant(t, s, f.org)
	if holdsSlot(t, tn, attempt.ID) {
		t.Fatal("the slot is released")
	}
	if stored := storedBuild(t, tn, f.target, attempt.ID); stored.FinishedAt.IsNone() || stored.Failure.IsSome() {
		t.Fatalf("stored: %#v", stored)
	}
	if _, ok, err := tn.RequestBuildCancel(ctx, f.target, attempt.ID); err != nil || ok {
		t.Fatalf("a finished build cannot be cancelled: %v, %v", ok, err)
	}
}

func TestCancellingWakesTheBuild(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	syncHead(t, s, f, headA)
	c, attempt := claimBuild(t, s)
	if _, err := s.RetryOperation(ctx, c, time.Hour, "Waiting"); err != nil {
		t.Fatalf("park: %v", err)
	}
	tn := tenant(t, s, f.org)
	operation, ok, err := tn.RequestBuildCancel(ctx, f.target, attempt.ID)
	if err != nil || !ok || operation != attempt.Operation {
		t.Fatalf("cancel: %v, %v, %v", operation, ok, err)
	}
	commit(t, tn)
	again, read := claimBuild(t, s)
	if again.ID != c.ID {
		t.Fatal("woken before its hour")
	}
	if !read.CancelRequested {
		t.Fatal("the request is read")
	}
}

// Beyond the Rust tests: the detail bound cuts on a character boundary, and
// the push reference and reject codes are the Rust ones.
func TestBuildHelpersMatchRust(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   int
	}{
		{"short", 5},
		{strings.Repeat("x", 2048), 2048},
		{strings.Repeat("x", 2047) + "é", 2047},
		{strings.Repeat("€", 1000), 2046},
	} {
		if got := len(store.Bounded(tc.detail)); got != tc.want {
			t.Errorf("bounded to %d bytes, want %d", got, tc.want)
		}
	}
	a := store.BuildAttempt{ImageRepository: "registry.local/acme/shop", Commit: commitSha(t, headA), AttemptNo: 2}
	if got := a.PushReference(); got != "registry.local/acme/shop:aaaaaaaaaaaa-2" {
		t.Errorf("push reference %s", got)
	}
	for reject, code := range map[target.Reject]string{
		target.LifecycleMismatch{}:  "LifecycleMismatch",
		target.Deleting{}:           "TargetDeleting",
		target.StaleSource{}:        "StaleSource",
		target.BuildConfigChanged{}: "BuildConfigChanged",
		target.GenerationMoved{}:    "GenerationMoved",
		target.NotAutomatic{}:       "NotAutomatic",
		target.Exhausted{}:          "GenerationExhausted",
	} {
		if got := store.RejectCode(reject); got != code {
			t.Errorf("%T: %s, want %s", reject, got, code)
		}
	}
}

package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// repo/controls.rs tests.

const controlsBy = "user:a"

type controlsFixture struct {
	org         ids.OrgID
	project     ids.ProjectID
	environment ids.EnvironmentID
	application ids.ApplicationID
	target      ids.TargetID
}

func newControlsFixture(t *testing.T, s *store.Store) controlsFixture {
	t.Helper()
	ctx := t.Context()
	f := controlsFixture{org: org(t, s, "a", "A")}
	tn := tenant(t, s, f.org)
	f.project = must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	f.environment = must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, f.project, "prod", "Prod", false))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, f.project, f.environment, cluster, "a-shop"))
	f.application = must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, f.project, "web", "Web"))
	f.target = must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, f.project, f.application, placement))
	commit(t, tn)
	return f
}

func (f controlsFixture) release(t *testing.T, tn *store.Tenant, digest string) ids.ReleaseID {
	t.Helper()
	d, err := artifact.ParseDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := tn.CreateRelease(t.Context(), f.project, store.PortableRelease{
		Application: f.application, Artifacts: map[string]artifact.Digest{"web": d},
		ProcessContract: map[string]any{}, PortableConfig: map[string]any{}, RendererSchema: 1, CreatedBy: controlsBy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f controlsFixture) request(release ids.ReleaseID, config ids.ConfigRevisionID, expected uint64, uid uuid.UUID) store.StartDeployment {
	return store.StartDeployment{
		Project: f.project, Target: f.target, Release: release, ConfigRevision: config,
		ExpectedGeneration: target.Generation(expected), LifecycleUID: uid, Reason: store.ReasonDeploy,
		RequestedBy: controlsBy, InputHash: fmt.Appendf(nil, "%s/%d", release, expected),
	}
}

// twoReleases makes old (ran successfully) and new (the newest run); the
// target is at generation 2.
func (f controlsFixture) twoReleases(t *testing.T, s *store.Store) (ids.ReleaseID, ids.ReleaseID, ids.ConfigRevisionID, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, f.org)
	config := configRevision(t, tn, f.project, f.target, map[string]any{}).ID
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v %v", ok, err)
	}
	uid := state.LifecycleUID
	old := f.release(t, tn, "sha256:"+strings.Repeat("a", 64))
	newer := f.release(t, tn, "sha256:"+strings.Repeat("b", 64))
	started, err := tn.StartDeployment(ctx, f.request(old, config, 0, uid), store.NewAudit{}, opt.None[store.IdempotencyKey]())
	accepted, ok := started.(store.StartedAccepted)
	if err != nil || !ok {
		t.Fatalf("not accepted: %#v %v", started, err)
	}
	if _, err := tn.TestExec(ctx, "UPDATE deployment_runs SET phase = 'succeeded' WHERE id = $1", accepted.Run); err != nil {
		t.Fatal(err)
	}
	if _, err := tn.StartDeployment(ctx, f.request(newer, config, 1, uid), store.NewAudit{}, opt.None[store.IdempotencyKey]()); err != nil {
		t.Fatal(err)
	}
	r, c, found, err := tn.RollbackPoint(ctx, f.target, opt.None[ids.ReleaseID]())
	if err != nil || !found || r != old || c != config {
		t.Fatalf("rollback point: %v %v %v %v", r, c, found, err)
	}
	if _, _, found, err := tn.RollbackPoint(ctx, f.target, opt.Some(newer)); err != nil || found {
		t.Fatalf("never succeeded: %v %v", found, err)
	}
	commit(t, tn)
	return old, newer, config, uid
}

func TestFreezesHoldReleasesAndEmergenciesPassEverything(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	fx := newControlsFixture(t, s)
	old, newer, config, uid := fx.twoReleases(t, s)
	tn := tenant(t, s, fx.org)
	now := clock.System{}.NowMs()
	window := func(startsAt int64) store.NewWindow {
		return store.NewWindow{
			Project: fx.project, Environment: fx.environment, Reason: "launch week", CreatedBy: controlsBy,
			StartsAt: startsAt, EndsAt: now + 3_600_000,
		}
	}
	later := must[uuid.UUID](t, "freeze")(tn.CreateFreeze(ctx, window(now+1_800_000)))
	if _, ok, err := tn.ActiveFreeze(ctx, fx.target, now); err != nil || ok {
		t.Fatalf("not started yet: %v %v", ok, err)
	}
	freeze := must[uuid.UUID](t, "freeze")(tn.CreateFreeze(ctx, window(now-1000)))
	if reason, ok, err := tn.ActiveFreeze(ctx, fx.target, now); err != nil || !ok || reason != "launch week" {
		t.Fatalf("active: %q %v %v", reason, ok, err)
	}
	none := opt.None[store.IdempotencyKey]()
	if started, err := tn.StartDeployment(ctx, fx.request(old, config, 2, uid), store.NewAudit{}, none); err != nil || started != (store.StartedFrozen{}) {
		t.Fatalf("frozen: %#v %v", started, err)
	}
	restart := fx.request(newer, config, 2, uid)
	restart.Reason = store.ReasonRestart
	if started, err := tn.StartDeployment(ctx, restart, store.NewAudit{}, none); err != nil {
		t.Fatal(err)
	} else if _, ok := started.(store.StartedAccepted); !ok {
		t.Fatalf("restarts pass a freeze: %#v", started)
	}
	unexplained := fx.request(old, config, 3, uid)
	unexplained.Reason = store.ReasonEmergency
	if _, err := tn.StartDeployment(ctx, unexplained, store.NewAudit{}, none); err == nil {
		t.Fatal("an emergency needs a reason")
	}
	if ok, err := tn.PauseTarget(ctx, fx.target, controlsBy, "incident"); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if ok, err := tn.PauseTarget(ctx, fx.target, controlsBy, "again"); err != nil || ok {
		t.Fatalf("paused once: %v %v", ok, err)
	}
	if held, err := tn.TargetPaused(ctx, fx.target); err != nil || !held {
		t.Fatalf("held: %v %v", held, err)
	}
	emergency, err := tn.StartEmergencyRollback(ctx, fx.request(old, config, 3, uid), "checkout is down", store.NewAudit{})
	accepted, ok := emergency.(store.StartedAccepted)
	if err != nil || !ok {
		t.Fatalf("emergency: %#v %v", emergency, err)
	}
	m, ok, err := tn.Materialization(ctx, accepted.Operation)
	if err != nil || !ok || !m.Emergency || !m.Paused || m.Phase != run.Planned {
		t.Fatalf("no approvals asked: %+v %v %v", m, ok, err)
	}

	if ok, err := tn.LiftFreeze(ctx, fx.environment, freeze, controlsBy); err != nil || !ok {
		t.Fatalf("lift: %v %v", ok, err)
	}
	if ok, err := tn.LiftFreeze(ctx, fx.environment, freeze, controlsBy); err != nil || ok {
		t.Fatalf("lifted once: %v %v", ok, err)
	}
	if freezes, err := tn.Freezes(ctx, fx.environment, false); err != nil || len(freezes) != 1 {
		t.Fatalf("the later one: %v %v", freezes, err)
	}
	if ok, err := tn.LiftFreeze(ctx, fx.environment, later, controlsBy); err != nil || !ok {
		t.Fatalf("lift later: %v %v", ok, err)
	}
	if ok, err := tn.ResumeTarget(ctx, fx.target); err != nil || !ok {
		t.Fatalf("resume: %v %v", ok, err)
	}
	if ok, err := tn.ResumeTarget(ctx, fx.target); err != nil || ok {
		t.Fatalf("resumed once: %v %v", ok, err)
	}
	commit(t, tn)
}

func TestOwnersAndSilences(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newControlsFixture(t, s)
	tn := tenant(t, s, f.org)
	project := opt.None[ids.ApplicationID]()
	if _, ok, err := tn.Owner(ctx, f.project, project); err != nil || ok {
		t.Fatalf("no owner yet: %v %v", ok, err)
	}
	set := func(app opt.Val[ids.ApplicationID], o opt.Val[store.NewOwner]) {
		t.Helper()
		if err := tn.SetOwner(ctx, f.project, app, o, controlsBy); err != nil {
			t.Fatal(err)
		}
	}
	set(project, opt.Some(store.NewOwner{Owner: "shop-team", Contact: opt.Some("#shop")}))
	set(opt.Some(f.application), opt.Some(store.NewOwner{Owner: "web-team", RunbookURL: opt.Some("https://wiki/web")}))
	set(project, opt.Some(store.NewOwner{Owner: "platform"}))
	if o, ok, err := tn.Owner(ctx, f.project, project); err != nil || !ok || o.Owner != "platform" {
		t.Fatalf("replaced: %+v %v %v", o, ok, err)
	}
	if o, ok, err := tn.Owner(ctx, f.project, opt.Some(f.application)); err != nil || !ok || o.RunbookURL != opt.Some("https://wiki/web") {
		t.Fatalf("the app's owner: %+v %v %v", o, ok, err)
	}
	set(project, opt.None[store.NewOwner]())
	if _, ok, err := tn.Owner(ctx, f.project, project); err != nil || ok {
		t.Fatalf("cleared: %v %v", ok, err)
	}

	now := clock.System{}.NowMs()
	silenced := func(target opt.Val[ids.TargetID], at int64) bool {
		t.Helper()
		ok, err := tn.Silenced(ctx, f.environment, target, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if silenced(opt.Some(f.target), now) {
		t.Fatal("not silenced yet")
	}
	silence := must[uuid.UUID](t, "silence")(tn.CreateSilence(ctx, store.NewWindow{
		Project: f.project, Environment: f.environment, Target: opt.Some(f.target), Reason: "maintenance",
		CreatedBy: controlsBy, StartsAt: now, EndsAt: now + 60_000,
	}))
	if !silenced(opt.Some(f.target), now) || silenced(opt.None[ids.TargetID](), now) || silenced(opt.Some(f.target), now+120_000) {
		t.Fatal("silenced: only that app, until it ends")
	}
	if silences, err := tn.Silences(ctx, f.environment, false); err != nil || len(silences) != 1 {
		t.Fatalf("silences: %v %v", silences, err)
	}
	if ok, err := tn.LiftSilence(ctx, f.environment, silence, controlsBy); err != nil || !ok {
		t.Fatalf("lift: %v %v", ok, err)
	}
	if silenced(opt.Some(f.target), now) {
		t.Fatal("lifted")
	}
	commit(t, tn)
}

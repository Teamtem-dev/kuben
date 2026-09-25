package store_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from materialize.rs.

type materializeFixture struct {
	org          ids.OrgID
	project      ids.ProjectID
	target       ids.TargetID
	lifecycleUID uuid.UUID
	release      ids.ReleaseID
	revision     ids.ConfigRevisionID
}

func newMaterializeFixture(t *testing.T, s *store.Store, slug string) materializeFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster,
		"kb-shop-production-"+slug))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	release, _ := mustCreate(t)(tn.CreateRelease(ctx, project, store.PortableRelease{
		Application:     application,
		Artifacts:       map[string]artifact.Digest{"web": parseDigest(t, catalogDigest)},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source:          opt.Some[any](map[string]any{"image_repository": "ghcr.io/acme/web"}),
		CreatedBy:       "user:alice",
	}))
	config := map[string]any{"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}}}
	revision := configRevision(t, tn, project, tgt, config)
	state, ok, err := tn.TargetState(ctx, tgt)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	commit(t, tn)
	return materializeFixture{
		org: o, project: project, target: tgt, lifecycleUID: state.LifecycleUID, release: release, revision: revision.ID,
	}
}

// startOperation accepts a run of reason expecting expected: its operation.
func startOperation(t *testing.T, s *store.Store, f materializeFixture, expected uint64, reason store.RunReason) ids.OperationID {
	t.Helper()
	tn := tenant(t, s, f.org)
	started := must[store.Started](t, "start")(tn.StartDeployment(t.Context(), store.StartDeployment{
		Project: f.project, Target: f.target, Release: f.release, ConfigRevision: f.revision,
		ExpectedGeneration: target.Generation(expected), LifecycleUID: f.lifecycleUID, Reason: reason,
		RequestedBy: "user:alice", InputHash: fmt.Appendf(nil, "%s-%d", reason, expected),
	}, store.NewAudit{ActorKind: "user", Action: "startDeployment", Outcome: "accepted"},
		opt.None[store.IdempotencyKey]()))
	accepted := acceptedRun(t, started)
	commit(t, tn)
	return accepted.Operation
}

// deployAndClaim accepts a deployment expecting expected and claims its
// operation.
func deployAndClaim(t *testing.T, s *store.Store, f materializeFixture, expected uint64) store.Claim {
	t.Helper()
	startOperation(t, s, f, expected, store.ReasonDeploy)
	c, ok := claim(t, s, "materializer", store.RunKind)
	if !ok {
		t.Fatal("due")
	}
	return c
}

func readMaterialization(t *testing.T, s *store.Store, o ids.OrgID, operation ids.OperationID) (store.Materialization, bool) {
	t.Helper()
	tn := tenant(t, s, o)
	m, ok, err := tn.Materialization(t.Context(), operation)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rollback(t, tn)
	return m, ok
}

func mustMaterialization(t *testing.T, s *store.Store, o ids.OrgID, operation ids.OperationID) store.Materialization {
	t.Helper()
	m, ok := readMaterialization(t, s, o, operation)
	if !ok {
		t.Fatal("the run")
	}
	return m
}

func TestARunReadsEverythingItIsRenderedFrom(t *testing.T) {
	s := pgtest.Store(t)
	f := newMaterializeFixture(t, s, "a")
	other := newMaterializeFixture(t, s, "b")
	c := deployAndClaim(t, s, f, 0)

	m := mustMaterialization(t, s, f.org, c.ID)
	if m.Operation != c.ID || m.Generation != 1 || m.DesiredGeneration != 1 || m.Phase != run.Planned {
		t.Fatalf("run: %+v", m)
	}
	if m.ProjectSlug != "shop" || m.EnvironmentSlug != "production" || m.ApplicationSlug != "web" ||
		m.Namespace != "kb-shop-production-a" {
		t.Fatalf("names: %+v", m)
	}
	if !m.Protected || m.Deleting {
		t.Fatalf("flags: %+v", m)
	}
	if m.Target != f.target || m.Release != f.release || m.LifecycleUID != f.lifecycleUID {
		t.Fatalf("ids: %+v", m)
	}
	if d := m.Artifacts["web"]; d.String() != catalogDigest {
		t.Fatalf("artifacts: %+v", m.Artifacts)
	}
	if repo, _ := m.ImageRepository.Get(); repo != "ghcr.io/acme/web" {
		t.Fatalf("image repository: %+v", m.ImageRepository)
	}
	if m.ConfigRevision != f.revision || m.ConfigRevisionNumber != 1 {
		t.Fatalf("revision: %+v", m)
	}
	if port := path(m.Config, "runtime", "processes", "web", "port"); port != json.Number("8080") {
		t.Fatalf("config: %#v", m.Config)
	}

	if _, ok := readMaterialization(t, s, other.org, c.ID); ok {
		t.Fatal("another organization's run")
	}
	if _, ok := readMaterialization(t, s, f.org, ids.New[ids.Operation]()); ok {
		t.Fatal("an unknown operation")
	}
}

func TestARestartStampsItsRunAndLaterRunsKeepTheStamp(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newMaterializeFixture(t, s, "a")
	first := startOperation(t, s, f, 0, store.ReasonDeploy)
	if m := mustMaterialization(t, s, f.org, first); m.RestartedAt.IsSome() {
		t.Fatalf("a deploy is not stamped: %+v", m.RestartedAt)
	}
	restart := startOperation(t, s, f, 1, store.ReasonRestart)
	stamp, ok := mustMaterialization(t, s, f.org, restart).RestartedAt.Get()
	if !ok {
		t.Fatal("a restart run is stamped")
	}
	next := startOperation(t, s, f, 2, store.ReasonDeploy)
	if got, _ := mustMaterialization(t, s, f.org, next).RestartedAt.Get(); got != stamp {
		t.Fatalf("a later deploy keeps the stamp, so its pods are not restarted again: %d, %d", got, stamp)
	}
	tn := tenant(t, s, f.org)
	if _, err := tn.TestExec(ctx, "UPDATE deployment_runs SET restarted_at = 0 WHERE operation_id = $1", next); err == nil {
		t.Fatal("the stamp is an input of the run")
	}
}

func appRecord(t *testing.T, s *store.Store, o ids.OrgID, environment ids.EnvironmentID) store.AppRecord {
	t.Helper()
	tn := tenant(t, s, o)
	a, ok, err := tn.App(t.Context(), environment, "web")
	if err != nil || !ok {
		t.Fatalf("app: %v, %v", ok, err)
	}
	rollback(t, tn)
	return a
}

func TestAnAgentDeliveredAppShowsWhatItsAgentObserved(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newMaterializeFixture(t, s, "a")
	c := deployAndClaim(t, s, f, 0)
	m := mustMaterialization(t, s, f.org, c.ID)
	if before := appRecord(t, s, f.org, m.Environment); before.Delivery != store.DeliveryController || before.Runtime.IsSome() {
		t.Fatalf("before: %+v", before)
	}

	caps := map[string]any{"sizes": []any{}, "clusterIssuer": "letsencrypt"}
	resources := []any{
		map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "web-web"}},
		map[string]any{
			"kind":     "HTTPRoute",
			"metadata": map[string]any{"name": "web"},
			"spec":     map[string]any{"hostnames": []any{"shop.example.com", "web-production.apps.example.com"}},
		},
	}
	tn := tenant(t, s, f.org)
	if _, ok, err := tn.FreezeRunPlan(ctx, c, m.Run, "kuben-renderer/1", caps, resources); err != nil || !ok {
		t.Fatalf("freeze: %v, %v", ok, err)
	}
	// A target moves to its agent, never back (migration 0012).
	if _, err := tn.TestExec(ctx, "UPDATE application_targets SET delivery = 'agent' WHERE id = $1", f.target); err != nil {
		t.Fatalf("to the agent: %v", err)
	}
	if !must[bool](t, "record")(tn.RecordRuntimeObservation(ctx, m.Cluster, f.target, 1, "applying",
		opt.None[string](), opt.None[string]())) {
		t.Fatal("record")
	}
	commit(t, tn)

	applying := appRecord(t, s, f.org, m.Environment)
	if applying.Delivery != store.DeliveryAgent {
		t.Fatalf("delivery: %s", applying.Delivery)
	}
	runtime, ok := applying.Runtime.Get()
	if !ok || runtime.Generation != 1 || runtime.Phase != "applying" || runtime.Ready() {
		t.Fatalf("observed: %+v", applying.Runtime)
	}
	if url, _ := runtime.URL.Get(); url != "https://shop.example.com" {
		t.Fatalf("the first hostname of the observed plan's route: %+v", runtime.URL)
	}

	tn = tenant(t, s, f.org)
	if !must[bool](t, "record")(tn.RecordRuntimeObservation(ctx, m.Cluster, f.target, 1, "failed",
		opt.Some("ProgressDeadlineExceeded"), opt.Some("web-web did not roll out"))) {
		t.Fatal("record")
	}
	commit(t, tn)
	failed, ok := appRecord(t, s, f.org, m.Environment).Runtime.Get()
	if reason, _ := failed.Reason.Get(); !ok || failed.Phase != "failed" || reason != "ProgressDeadlineExceeded" {
		t.Fatalf("failed: %+v", failed)
	}
}

func TestARunPlanIsFrozenOnceUnderTheFence(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newMaterializeFixture(t, s, "a")
	c := deployAndClaim(t, s, f, 0)
	m := mustMaterialization(t, s, f.org, c.ID)
	if m.RenderPlan.IsSome() {
		t.Fatalf("plan: %+v", m.RenderPlan)
	}
	caps := map[string]any{"sizes": []any{}}
	first := []any{map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "web-web"}}}

	tn := tenant(t, s, f.org)
	plan, ok, err := tn.FreezeRunPlan(ctx, c, m.Run, "kuben-renderer/1", caps, first)
	if err != nil || !ok {
		t.Fatalf("freeze: %v, %v", ok, err)
	}
	commit(t, tn)
	if got, _ := mustMaterialization(t, s, f.org, c.ID).RenderPlan.Get(); got != plan {
		t.Fatalf("frozen plan: %v, %v", got, plan)
	}

	tn = tenant(t, s, f.org)
	again, ok, err := tn.FreezeRunPlan(ctx, c, m.Run, "kuben-renderer/2", caps, []any{})
	if err != nil || !ok || again != plan {
		t.Fatalf("a frozen plan is never replaced: %v, %v, %v", again, ok, err)
	}
	frozen, ok, err := tn.RunRenderPlan(ctx, m.Run)
	if err != nil || !ok || frozen.ID != plan || frozen.RendererVersion != "kuben-renderer/1" {
		t.Fatalf("frozen: %+v, %v, %v", frozen, ok, err)
	}
	if diff := cmp.Diff(any(first), frozen.Resources); diff != "" {
		t.Fatalf("resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(any(caps), frozen.CapabilitySnapshot); diff != "" {
		t.Fatalf("capabilities (-want +got):\n%s", diff)
	}
	rollback(t, tn)

	next := deployAndClaim(t, s, f, 1)
	m2 := mustMaterialization(t, s, f.org, next.ID)
	stale := next
	stale.Fence--
	tn = tenant(t, s, f.org)
	if _, ok, err := tn.FreezeRunPlan(ctx, stale, m2.Run, "kuben-renderer/1", caps, first); err != nil || ok {
		t.Fatalf("a fenced-off worker freezes nothing: %v, %v", ok, err)
	}
	rollback(t, tn)
	if got := mustMaterialization(t, s, f.org, next.ID).RenderPlan; got.IsSome() {
		t.Fatalf("plan of run 2: %+v", got)
	}
}

func TestRecordsMoveForwardUnderTheFence(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newMaterializeFixture(t, s, "a")
	older := deployAndClaim(t, s, f, 0)
	newer := deployAndClaim(t, s, f, 1)
	mOlder := mustMaterialization(t, s, f.org, older.ID)
	mNewer := mustMaterialization(t, s, f.org, newer.ID)

	if !must[bool](t, "record")(s.RecordMaterialization(ctx, newer, mNewer, "uid-1", 4)) {
		t.Fatal("record")
	}
	if must[bool](t, "record")(s.RecordMaterialization(ctx, older, mOlder, "uid-1", 5)) {
		t.Fatal("a lower generation never replaces a newer record")
	}

	if _, err := s.TestExec(ctx, "UPDATE operations SET lease_until = 0 WHERE id = $1", newer.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	taken, ok := claim(t, s, "other", store.RunKind)
	if !ok || taken.ID != newer.ID {
		t.Fatalf("taken over: %+v, %v", taken, ok)
	}
	if must[bool](t, "record")(s.RecordMaterialization(ctx, newer, mNewer, "uid-1", 6)) {
		t.Fatal("a fenced-off worker records nothing")
	}

	tn := tenant(t, s, f.org)
	recorded, ok, err := tn.Materialized(ctx, f.target)
	if err != nil || !ok || recorded.Generation != 2 || recorded.Operation != newer.ID || recorded.ResourceGeneration != 4 {
		t.Fatalf("recorded: %+v, %v, %v", recorded, ok, err)
	}
	rollback(t, tn)
	if _, err := s.TestExec(ctx, "UPDATE target_materializations SET generation = 1"); err == nil {
		t.Fatal("the recorded generation never decreases")
	}
}

func TestDriftIsRecordedAgainstTheWrittenGeneration(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newMaterializeFixture(t, s, "a")
	c := deployAndClaim(t, s, f, 0)
	m := mustMaterialization(t, s, f.org, c.ID)
	if !must[bool](t, "record")(s.RecordMaterialization(ctx, c, m, "uid-1", 3)) {
		t.Fatal("record")
	}

	written, ok, err := s.MaterializedResource(ctx, "uid-1")
	if err != nil || !ok || written.Org != f.org || written.Target != f.target {
		t.Fatalf("found by the object's UID: %+v, %v, %v", written, ok, err)
	}
	if _, ok, err := s.MaterializedResource(ctx, "uid-2"); err != nil || ok {
		t.Fatalf("uid-2: %v, %v", ok, err)
	}

	drift := map[string]any{"managers": []any{"kubectl-edit"}}
	if !must[bool](t, "drift")(s.RecordDrift(ctx, written, opt.Some(store.Replacement{UID: "uid-2", Generation: 5}), drift)) {
		t.Fatal("drift")
	}
	stale := written
	stale.Generation = 2
	if must[bool](t, "drift")(s.RecordDrift(ctx, stale, opt.None[store.Replacement](), drift)) {
		t.Fatal("drift of another generation")
	}
	if _, ok, err := s.MaterializedResource(ctx, "uid-1"); err != nil || ok {
		t.Fatalf("the object that replaced the drifted one has a new UID: %v, %v", ok, err)
	}
	now, ok, err := s.MaterializedResource(ctx, "uid-2")
	if err != nil || !ok || now.DriftCount != 1 || now.ResourceGeneration != 5 || now.DriftDetectedAt.IsNone() {
		t.Fatalf("found by the replacement's UID: %+v, %v, %v", now, ok, err)
	}
	got, _ := now.Drift.Get()
	if diff := cmp.Diff(any(drift), got); diff != "" {
		t.Fatalf("drift (-want +got):\n%s", diff)
	}
}

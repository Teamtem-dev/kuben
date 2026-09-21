package store_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from catalog.rs.

const catalogDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type shop struct {
	org         ids.OrgID
	project     ids.ProjectID
	environment ids.EnvironmentID
	application ids.ApplicationID
	target      ids.TargetID
}

func newShop(t *testing.T, s *store.Store) shop {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProjectDescribed(ctx, "shop", "Shop", opt.Some("The online shop")))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironmentTyped(ctx, project, "prod", "Prod",
		store.Production, opt.Some[any](map[string]any{"cpu": "4"})))
	cluster := must[ids.ClusterID](t, "cluster")(tn.EnsureCluster(ctx, "primary"))
	if again := must[ids.ClusterID](t, "again")(tn.EnsureCluster(ctx, "primary")); again != cluster {
		t.Fatalf("ensure_cluster made a second cluster: %v, %v", again, cluster)
	}
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster, "kb-shop-prod"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	commit(t, tn)
	return shop{org: o, project: project, environment: environment, application: application, target: tgt}
}

func parseDigest(t *testing.T, text string) artifact.Digest {
	t.Helper()
	d, err := artifact.ParseDigest(text)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d
}

// deployShop records a revision and a deployed release of `nginx:1.27`,
// resolved to catalogDigest.
func deployShop(t *testing.T, s *store.Store, sh shop) {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, sh.org)
	release, _ := mustCreate(t)(tn.CreateRelease(ctx, sh.project, store.PortableRelease{
		Application:     sh.application,
		Artifacts:       map[string]artifact.Digest{"web": parseDigest(t, catalogDigest)},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source: opt.Some[any](map[string]any{
			"image_repository": "docker.io/library/nginx", "image": "nginx:1.27",
		}),
		CreatedBy: "user:alice",
	}))
	config := map[string]any{"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 80}}}}
	revision := configRevision(t, tn, sh.project, sh.target, config)
	state, ok, err := tn.TargetState(ctx, sh.target)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	started := must[store.Started](t, "start")(tn.StartDeployment(ctx, store.StartDeployment{
		Project: sh.project, Target: sh.target, Release: release, ConfigRevision: revision.ID,
		ExpectedGeneration: 0, LifecycleUID: state.LifecycleUID, Reason: store.ReasonDeploy,
		RequestedBy: "user:alice", InputHash: []byte("deploy"),
	}, store.NewAudit{ActorKind: "user", Action: "createApp", Outcome: "accepted"},
		opt.Some(store.IdempotencyKey{Actor: "user:alice", Key: "k", TTL: time.Hour})))
	if _, ok := started.(store.StartedAccepted); !ok {
		t.Fatalf("not accepted: %#v", started)
	}
	commit(t, tn)
}

// mustCreate reads CreateRelease's (id, created, error).
func mustCreate(t *testing.T) func(ids.ReleaseID, bool, error) (ids.ReleaseID, bool) {
	t.Helper()
	return func(id ids.ReleaseID, created bool, err error) (ids.ReleaseID, bool) {
		t.Helper()
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		return id, created
	}
}

func configRevision(t *testing.T, tn *store.Tenant, project ids.ProjectID, tgt ids.TargetID, config any) store.ConfigRevisionRecord {
	t.Helper()
	r, ok, err := tn.CreateConfigRevision(t.Context(), project, tgt, config, "user:alice")
	if err != nil || !ok {
		t.Fatalf("revision: %v, %v", ok, err)
	}
	return r
}

// path reads a nested member of decoded JSON.
func path(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func TestListsWhatTheAPIShows(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	sh := newShop(t, s)
	tn := tenant(t, s, sh.org)

	projects := must[[]store.Project](t, "projects")(tn.Projects(ctx))
	if len(projects) != 1 {
		t.Fatalf("projects: %+v", projects)
	}
	if d, _ := projects[0].Description.Get(); d != "The online shop" || projects[0].Environments != 1 {
		t.Fatalf("project: %+v", projects[0])
	}
	if p, ok, err := tn.Project(ctx, "shop"); err != nil || !ok || p.ID != sh.project {
		t.Fatalf("project shop: %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := tn.Project(ctx, "nope"); err != nil || ok {
		t.Fatalf("project nope: %v, %v", ok, err)
	}

	envs := must[[]store.EnvironmentRecord](t, "environments")(tn.Environments(ctx, sh.project))
	if len(envs) != 1 {
		t.Fatalf("environments: %+v", envs)
	}
	e := envs[0]
	if e.EnvType != "production" || !e.Protected || e.ID != sh.environment {
		t.Fatalf("environment: %+v", e)
	}
	if ns, _ := e.Namespace.Get(); ns != "kb-shop-prod" {
		t.Fatalf("namespace: %+v", e.Namespace)
	}
	quota, _ := e.Quota.Get()
	if diff := cmp.Diff(any(map[string]any{"cpu": "4"}), quota); diff != "" {
		t.Fatalf("quota (-want +got):\n%s", diff)
	}
	if one, ok, err := tn.Environment(ctx, sh.project, "prod"); err != nil || !ok || one.ID != sh.environment {
		t.Fatalf("environment prod: %+v, %v, %v", one, ok, err)
	}

	app, ok, err := tn.App(ctx, sh.environment, "web")
	if err != nil || !ok {
		t.Fatalf("app: %v, %v", ok, err)
	}
	if app.Config.IsSome() || app.Image.IsSome() {
		t.Fatalf("nothing deployed yet: %+v", app)
	}
	rollback(t, tn)

	deployShop(t, s, sh)
	tn = tenant(t, s, sh.org)
	apps := must[[]store.AppRecord](t, "apps")(tn.Apps(ctx, sh.environment))
	if len(apps) != 1 {
		t.Fatalf("apps: %+v", apps)
	}
	a := apps[0]
	if a.Target != sh.target || a.Namespace != "kb-shop-prod" || a.DesiredGeneration != target.Generation(1) {
		t.Fatalf("app: %+v", a)
	}
	if image, _ := a.Image.Get(); image != "nginx:1.27" {
		t.Fatalf("the image as given: %+v", a.Image)
	}
	config, _ := a.Config.Get()
	if port := path(config, "runtime", "processes", "web", "port"); port != float64(80) {
		t.Fatalf("config: %#v", config)
	}

	// M2.16: every phase a run enters is kept, in order.
	runs := must[[]store.RunRecord](t, "runs")(tn.Runs(ctx, sh.target, 1))
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	r := runs[0].Run
	first := must[map[ids.DeploymentRunID][]store.PhaseEntry](t, "phases")(tn.RunPhases(ctx, sh.target, []ids.DeploymentRunID{r}))
	if len(first[r]) == 0 {
		t.Fatal("the phase the run started in")
	}
	for _, phase := range []string{"applying", "verifying", "succeeded"} {
		if _, err := tn.TestExec(ctx, "UPDATE deployment_runs SET phase = $2, updated_at = updated_at + 1 WHERE id = $1",
			r, phase); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	timeline := must[map[ids.DeploymentRunID][]store.PhaseEntry](t, "phases")(tn.RunPhases(ctx, sh.target, []ids.DeploymentRunID{r}))[r]
	var phases []string
	for i, p := range timeline {
		phases = append(phases, p.Phase)
		if i > 0 && timeline[i-1].At > p.At {
			t.Fatalf("out of order: %+v", timeline)
		}
	}
	if len(phases) < 3 {
		t.Fatalf("phases: %v", phases)
	}
	if diff := cmp.Diff([]string{"applying", "verifying", "succeeded"}, phases[len(phases)-3:]); diff != "" {
		t.Fatalf("phases (-want +got):\n%s", diff)
	}
	if _, err := tn.TestExec(ctx, "UPDATE deployment_run_phases SET phase = 'failed' WHERE run_id = $1", r); err == nil {
		t.Fatal("the history is never rewritten")
	}
}

func TestDeletedRowsAreHiddenAndFreeTheirSlug(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	sh := newShop(t, s)
	tn := tenant(t, s, sh.org)
	for _, sql := range []string{
		"UPDATE application_targets SET deleting = TRUE, deleted_at = 1 WHERE id = $1",
		"UPDATE applications SET deleted_at = 1 WHERE id = (SELECT application_id FROM application_targets WHERE id = $1)",
	} {
		if _, err := tn.TestExec(ctx, sql, sh.target); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if apps := must[[]store.AppRecord](t, "apps")(tn.Apps(ctx, sh.environment)); len(apps) != 0 {
		t.Fatalf("apps: %+v", apps)
	}
	var placement uuid.UUID
	if err := tn.TestQueryRow(ctx, "SELECT placement_id FROM application_targets WHERE id = $1", sh.target).
		Scan(&placement); err != nil {
		t.Fatalf("placement: %v", err)
	}
	again := must[ids.ApplicationID](t, "the slug is free again")(tn.CreateApplication(ctx, sh.project, "web", "Web"))
	must[ids.TargetID](t, "a new target on the same placement")(tn.CreateTarget(ctx, sh.project, again, store.PlacementID(placement)))
	if apps := must[[]store.AppRecord](t, "apps")(tn.Apps(ctx, sh.environment)); len(apps) != 1 {
		t.Fatalf("apps: %+v", apps)
	}
	commit(t, tn)

	tn = tenant(t, s, sh.org)
	if _, err := tn.TestExec(ctx, "UPDATE application_targets SET deleted_at = NULL WHERE id = $1", sh.target); err == nil {
		t.Fatal("a deleted row stays deleted")
	}
}

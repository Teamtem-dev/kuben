package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
)

func jsonValue(t *testing.T, text string) any {
	t.Helper()
	v, err := wire.DecodeAny([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// sqlApp is tests/http.rs sql_app: app `api`, shown as `API`, on a
// placement of prod, in SQL only, never deployed.
func (f fixture) sqlApp() (ids.ProjectID, ids.ApplicationID, ids.TargetID) {
	t := f.t
	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	project, _, err := tn.Project(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := tn.Environment(ctx, project.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := tn.EnsureCluster(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	placement, err := tn.CreatePlacement(ctx, project.ID, env.ID, cluster, "kb-shop-prod")
	if err != nil {
		t.Fatal(err)
	}
	application, err := tn.CreateApplication(ctx, project.ID, "api", "API")
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := tn.CreateTarget(ctx, project.ID, application, placement)
	if err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return project.ID, application, tgt
}

// seedApp is the app half of tests/http.rs seed_sql: sqlApp, deployed once
// as `nginx:1.27` on `api.example.com`.
func (f fixture) seedApp() {
	t := f.t
	ctx := t.Context()
	projectID, application, tgt := f.sqlApp()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	config := jsonValue(t, `{"runtime":{"processes":{"web":{"port":80}}},"domains":[{"host":"api.example.com","tls":"auto"}]}`)
	revision, _, err := tn.CreateConfigRevision(ctx, projectID, tgt, config, "user:seed")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := artifact.ParseDigest(nginx127)
	if err != nil {
		t.Fatal(err)
	}
	release, _, err := tn.CreateRelease(ctx, projectID, store.PortableRelease{
		Application: application, Artifacts: map[string]artifact.Digest{"web": digest},
		ProcessContract: map[string]any{}, PortableConfig: map[string]any{}, RendererSchema: 1,
		Source:    opt.Some(jsonValue(t, `{"image_repository":"docker.io/library/nginx","image":"nginx:1.27"}`)),
		CreatedBy: "user:seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := tn.TargetState(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.StartDeployment(ctx, store.StartDeployment{
		Project: projectID, Target: tgt, Release: release, ConfigRevision: revision.ID,
		ExpectedGeneration: target.Generation(0), LifecycleUID: state.LifecycleUID, Reason: store.ReasonDeploy,
		RequestedBy: "user:seed", InputHash: []byte("seed"),
	}, store.NewAudit{ActorKind: "user", Action: "seed", Outcome: "accepted"}, opt.None[store.IdempotencyKey]()); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedAppProjections is tests/http.rs seed_projections for prod and api.
func (f fixture) seedAppProjections() {
	org := opt.Some(f.org.String())
	uid := func() opt.Val[string] { return opt.Some(uuid.Must(uuid.NewV7()).String()) }
	f.projections.UpsertEnvironment(projection.EnvironmentView{
		Name: "shop-prod", UID: uid(), Project: "shop", Org: org, EnvType: "production", Namespace: "kb-shop-prod",
		Phase: opt.Some("Ready"), Ready: true,
	})
	f.projections.UpsertApp(projection.AppView{
		Key: "kb-shop-prod/api", Namespace: "kb-shop-prod", Name: "api", UID: uid(), Org: org,
		Project: opt.Some("shop"), Environment: opt.Some("shop-prod"), Image: opt.Some("nginx:1.27"),
		URL: opt.Some("https://api.example.com"), Ready: true, Reason: opt.Some("Available"),
		Processes: []projection.ProcessView{{
			Name: "web", Command: []string{}, Port: opt.Some[uint16](80), Size: "small", MinReplicas: 1,
			MaxReplicas: 1, Protocol: "http",
		}},
		Env: []projection.EnvVarRef{}, Domains: []string{"api.example.com"}, Volumes: []projection.VolumeView{},
	})
	f.projections.UpsertPod(projection.PodView{
		Key: "kb-shop-prod/api-web-1", Namespace: "kb-shop-prod", Name: "api-web-1", Org: org,
		App: opt.Some("api"), Process: opt.Some("web"), Phase: projection.PodRunning, Ready: true,
		Node: opt.Some("node-1"),
	})
}

// The app half of tests/http.rs environments_and_apps_read_from_sql.
func TestAppsReadFromSQL(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	f.seedAppProjections()
	alice := f.signIn("alice@example.com", seedPassword)

	status, apps := alice.list("/api/v1/projects/shop/environments/prod/apps")
	if status != 200 || len(apps) != 1 || apps[0]["name"] != "api" || apps[0]["environment"] != "prod" {
		t.Fatalf("list: %d %v", status, apps)
	}
	status, detail, _ := alice.do("GET", "/api/v1/projects/shop/environments/prod/apps/api", nil)
	app, _ := detail["app"].(map[string]any)
	pods, _ := detail["pods"].([]any)
	if status != 200 || app["url"] != "https://api.example.com" || len(pods) != 1 {
		t.Fatalf("detail: %d %v", status, detail)
	}
	pod, _ := pods[0].(map[string]any)
	if pod["name"] != "api-web-1" || pod["phase"] != "running" {
		t.Fatalf("pod: %v", pod)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/prod/apps/nope", nil); status != http.StatusNotFound {
		t.Fatalf("unknown app: %d", status)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/prod/apps/api/logs", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("logs without cluster: %d", status)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/prod/apps/api/events", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("events without cluster: %d", status)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/prod/apps/api/domains", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("domains without cluster: %d", status)
	}

	// Invalid input is rejected; a tag is resolved to a digest, and SQL
	// needs no cluster.
	create := func(body map[string]any) (int, map[string]any) {
		status, out, _ := alice.do("POST", "/api/v1/projects/shop/environments/prod/apps", body)
		return status, out
	}
	if status, _ := create(map[string]any{"name": "web", "image": "nginx latest"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid image: %d", status)
	}
	status, created := create(map[string]any{"name": "web", "image": "nginx:1.27", "port": 80})
	if status != http.StatusCreated || created["image"] != "nginx:1.27" || created["ready"] != false {
		t.Fatalf("create: %d %v", status, created)
	}
	if status, _ := create(map[string]any{"name": "www", "image": "nginx:1.27", "port": 80, "domains": []string{"api.example.com"}}); status != http.StatusConflict {
		t.Fatalf("a domain another app uses: %d", status)
	}
	if status, _ := create(map[string]any{"name": "cache", "image": "redis:7"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("a tag its registry does not know: %d", status)
	}
}

// The app half of tests/http.rs viewers_can_read_but_not_write.
func TestViewersCanReadAppsButNotWrite(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	bob := f.signIn("bob@example.com", seedPassword)
	if status := bob.status("POST", "/api/v1/projects/shop/environments/prod/apps", map[string]any{"name": "web", "image": "nginx:1.27"}); status != http.StatusForbidden {
		t.Fatalf("create: %d", status)
	}
	if status := bob.status("POST", "/api/v1/projects/shop/environments/prod/apps/api/restart", nil); status != http.StatusForbidden {
		t.Fatalf("restart: %d", status)
	}
}

// sameJSON compares a and b as the JSON they write.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

func record(t *testing.T) store.AppRecord {
	t.Helper()
	spec := sampleSpec(t)
	config, err := api.ConfigOf(&spec)
	if err != nil {
		t.Fatal(err)
	}
	return store.AppRecord{
		Target: ids.New[ids.Target](), Application: ids.New[ids.Application](), Slug: "web", Name: "web",
		Namespace: "kb-shop-prod", LifecycleUID: uuid.Must(uuid.NewV7()), DesiredGeneration: target.Generation(1),
		Config: opt.Some(config), Image: opt.Some("nginx:1.27"), Delivery: store.DeliveryController,
	}
}

// apps/mod.rs the_configuration_is_the_spec_without_its_image.
func TestTheConfigurationIsTheSpecWithoutItsImage(t *testing.T) {
	r := record(t)
	config, _ := r.Config.Get()
	if object, _ := config.(map[string]any); object["source"] != nil {
		t.Fatalf("source in the configuration: %v", config)
	}
	back, ok := api.DesiredSpec(r)
	spec := sampleSpec(t)
	if !ok || !sameJSON(t, back, spec) {
		t.Fatalf("the round trip is lossless: %+v", back)
	}
	dto := api.AppDtoOf("shop", "prod", r, opt.None[projection.AppView]())
	image, _ := dto.Image.Get()
	port, _ := dto.Processes[0].Port.Get()
	if image != "nginx:1.27" || port != 80 || dto.Ready {
		t.Fatalf("%+v", dto)
	}
}

// apps/mod.rs an_agent_delivered_app_shows_its_agents_report.
func TestAnAgentDeliveredAppShowsItsAgentsReport(t *testing.T) {
	runtime := store.RuntimeStatus{Generation: 1, Phase: "applying", URL: opt.Some("https://shop.example.com")}
	applying := record(t)
	applying.Delivery, applying.Runtime = store.DeliveryAgent, opt.Some(runtime)
	dto := api.AppDtoOf("shop", "prod", applying, opt.None[projection.AppView]())
	if reason, _ := dto.Reason.Get(); dto.Ready || reason != "Applying" {
		t.Fatalf("applying: %+v", dto)
	}
	if url, _ := dto.URL.Get(); url != "https://shop.example.com" {
		t.Fatal(url)
	}

	ready := applying
	runtime.Phase = "ready"
	ready.Runtime = opt.Some(runtime)
	dto = api.AppDtoOf("shop", "prod", ready, opt.None[projection.AppView]())
	if !dto.Ready || !dto.Reason.IsNull() {
		t.Fatalf("ready: %+v", dto)
	}
	deleting := ready
	deleting.Deleting = true
	if api.AppDtoOf("shop", "prod", deleting, opt.None[projection.AppView]()).Ready {
		t.Fatal("deleting")
	}

	failed := applying
	runtime.Phase, runtime.Reason = "failed", opt.Some("ProgressDeadlineExceeded")
	runtime.Message = opt.Some("web-web did not roll out")
	failed.Runtime = opt.Some(runtime)
	dto = api.AppDtoOf("shop", "prod", failed, opt.None[projection.AppView]())
	reason, _ := dto.Reason.Get()
	message, _ := dto.Message.Get()
	if dto.Ready || reason != "ProgressDeadlineExceeded" || message != "web-web did not roll out" {
		t.Fatalf("failed: %+v", dto)
	}

	unreported := record(t)
	unreported.Delivery = store.DeliveryAgent
	dto = api.AppDtoOf("shop", "prod", unreported, opt.None[projection.AppView]())
	if dto.Ready || !dto.URL.IsNull() {
		t.Fatalf("the agent has not reported yet: %+v", dto)
	}
}

// admission.rs environment_quotas_become_limits.
func TestEnvironmentQuotasBecomeLimits(t *testing.T) {
	if !api.EnvLimits(opt.None[any]()).IsUnlimited() {
		t.Fatal("no quota")
	}
	l := api.EnvLimits(opt.Some(jsonValue(t, `{"cpu":"4","memory":"8Gi","pods":20}`)))
	if l.CPUMillis != opt.Some[uint64](4000) || l.MemoryBytes != opt.Some[uint64](8<<30) || l.Pods != opt.Some[uint64](20) {
		t.Fatalf("%+v", l)
	}
	if !api.EnvLimits(opt.Some[any]("broken")).IsUnlimited() {
		t.Fatal("broken")
	}
}

// admission.rs stored_configurations_are_measured_without_their_image.
func TestStoredConfigurationsAreMeasuredWithoutTheirImage(t *testing.T) {
	config := jsonValue(t, `{"runtime":{"processes":{"web":{"port":80,"replicas":{"min":2,"max":2}}}}}`)
	spec, ok := api.SpecOf(opt.Some(config), opt.Some(api.AnyImage))
	if !ok {
		t.Fatal("spec")
	}
	d, err := api.Peak(&spec, render.DefaultPlatform())
	if err != nil {
		t.Fatal(err)
	}
	if d.Peak.Pods != 3 {
		t.Fatalf("two replicas and a surge pod: %+v", d.Peak)
	}
}

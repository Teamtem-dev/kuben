package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// routes/apps/promote.rs promotion_keeps_target_domains_and_scaling_and_reports_changes.
func TestPromotionKeepsTargetDomainsAndScalingAndReportsChanges(t *testing.T) {
	source := sampleSpec(t)
	source.Source = v1alpha1.SourceFromImage("nginx:1.28")
	source.Env = []v1alpha1.EnvVar{
		api.ToCRDEnv(gen.EnvVarDto{
			Name:   "DATABASE_URL",
			Secret: gen.NewOptNilSecretRef(gen.SecretRef{Name: "db", Key: "url"}),
		}),
	}

	targetSpec := sampleSpec(t)
	targetSpec.Domains = api.ToDomains([]string{"shop.acme.com"})
	if web, ok := targetSpec.Runtime.Processes["web"]; ok {
		web.Replicas = v1alpha1.Replicas{Min: 3, Max: 6}
		web.Size = "large"
		targetSpec.Runtime.Processes["web"] = web
	}

	promoted := api.PromoteSpec(&source, &targetSpec)
	if promoted.Source.Image == nil || *promoted.Source.Image != "nginx:1.28" {
		t.Fatalf("promoted image: %v", promoted.Source.Image)
	}
	if len(promoted.Domains) == 0 || promoted.Domains[0].Host != "shop.acme.com" {
		t.Fatalf("promoted domains: %v", promoted.Domains)
	}
	if promoted.Runtime.Processes["web"].Replicas != (v1alpha1.Replicas{Min: 3, Max: 6}) {
		t.Fatalf("promoted replicas: %v", promoted.Runtime.Processes["web"].Replicas)
	}
	if promoted.Runtime.Processes["web"].Size != "large" {
		t.Fatalf("promoted size: %v", promoted.Runtime.Processes["web"].Size)
	}

	changes := api.SpecChanges(&targetSpec, &promoted)
	if !slices.Contains(changes, "image: nginx:1.27 → nginx:1.28") {
		t.Fatalf("changes missing image: %v", changes)
	}
	if !slices.Contains(changes, "env: add DATABASE_URL") {
		t.Fatalf("changes missing env: %v", changes)
	}

	createChanges := api.SpecChanges(nil, &promoted)
	if len(createChanges) == 0 || !strings.HasPrefix(createChanges[0], "create the app") {
		t.Fatalf("create changes: %v", createChanges)
	}
	sameChanges := api.SpecChanges(&promoted, &promoted)
	if len(sameChanges) != 0 {
		t.Fatalf("same changes not empty: %v", sameChanges)
	}

	none := map[string]map[string]struct{}{}
	missing := api.MissingSecrets(&promoted, none)
	if len(missing) != 1 {
		t.Fatalf("missing secrets with none: %v", missing)
	}

	have := map[string]map[string]struct{}{
		"db": {"url": struct{}{}},
	}
	missing = api.MissingSecrets(&promoted, have)
	if len(missing) != 0 {
		t.Fatalf("missing secrets with have: %v", missing)
	}
}

const promotePath = "/api/v1/projects/shop/environments/prod/apps/api/promote"

// withTenant runs fn in a transaction of the fixture's organization and
// commits it.
func (f fixture) withTenant(fn func(ctx context.Context, tn *store.Tenant, project store.Project)) {
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
	fn(ctx, tn, project)
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// stagingGeneration is the desired generation of app `api` in staging.
func (f fixture) stagingGeneration() target.Generation {
	t := f.t
	var generation target.Generation
	f.withTenant(func(ctx context.Context, tn *store.Tenant, project store.Project) {
		env, found, err := tn.Environment(ctx, project.ID, "staging")
		if err != nil || !found {
			t.Fatalf("staging: %v, %v", found, err)
		}
		app, found, err := tn.App(ctx, env.ID, "api")
		if err != nil || !found {
			t.Fatalf("staging app: %v, %v", found, err)
		}
		generation = app.DesiredGeneration
	})
	return generation
}

func createStaging(t *testing.T, c *client) {
	t.Helper()
	if status := c.status("POST", "/api/v1/projects/shop/environments", map[string]any{"name": "staging"}); status != http.StatusCreated {
		t.Fatalf("create staging: %d", status)
	}
}

// promoteResult is the fields of a PromoteResult other than the app.
type promoteResult struct {
	DryRun   bool     `json:"dry_run"`
	Created  bool     `json:"created"`
	Changes  []string `json:"changes"`
	Warnings []string `json:"warnings"`
}

func resultOf(t *testing.T, body map[string]any) promoteResult {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var r promoteResult
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// HTTP promotion: a dry run, the promotion, a promotion that changes
// nothing, and the refusals.
func TestPromoteApp(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)
	createStaging(t, alice)

	// A dry run reports what would change and deploys nothing.
	status, body, _ := alice.do("POST", promotePath, map[string]any{"to_environment": "staging", "dry_run": true})
	if status != http.StatusOK {
		t.Fatalf("dry run promote status %d: %v", status, body)
	}
	want := promoteResult{
		DryRun: true, Created: true, Changes: []string{"create the app with image nginx:1.27"}, Warnings: []string{},
	}
	if diff := cmp.Diff(want, resultOf(t, body)); diff != "" {
		t.Fatalf("dry run (-want +got):\n%s", diff)
	}
	if app, exists := body["app"]; !exists || app != nil {
		t.Fatalf("expected app: null in a dry run: %v", body)
	}

	// The promotion creates the app in staging.
	status, body, _ = alice.do("POST", promotePath, map[string]any{"to_environment": "staging", "dry_run": false})
	if status != http.StatusOK {
		t.Fatalf("live promote status %d: %v", status, body)
	}
	want.DryRun = false
	if diff := cmp.Diff(want, resultOf(t, body)); diff != "" {
		t.Fatalf("promotion (-want +got):\n%s", diff)
	}
	appObj, ok := body["app"].(map[string]any)
	if !ok {
		t.Fatalf("expected app object in result: %v", body)
	}
	if env, _ := appObj["environment"].(string); env != "staging" {
		t.Fatalf("expected environment=staging: %v", appObj)
	}

	// Promoting again changes nothing: no app and no deployment.
	generation := f.stagingGeneration()
	status, body, _ = alice.do("POST", promotePath, map[string]any{"to_environment": "staging"})
	if status != http.StatusOK {
		t.Fatalf("second promote status %d: %v", status, body)
	}
	unchanged := promoteResult{Changes: []string{}, Warnings: []string{}}
	if diff := cmp.Diff(unchanged, resultOf(t, body)); diff != "" {
		t.Fatalf("second promotion (-want +got):\n%s", diff)
	}
	if app, exists := body["app"]; !exists || app != nil {
		t.Fatalf("expected app: null without changes: %v", body)
	}
	if after := f.stagingGeneration(); after != generation {
		t.Fatalf("a promotion without changes deployed: generation %d → %d", generation, after)
	}

	// The same environment is refused.
	status, body, _ = alice.do("POST", promotePath, map[string]any{"to_environment": "prod"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for same environment, got %d: %v", status, body)
	}

	// A target whose project is being deleted is refused.
	f.withTenant(func(ctx context.Context, tn *store.Tenant, project store.Project) {
		if _, err := tn.MarkProjectDeleting(ctx, project.ID); err != nil {
			t.Fatal(err)
		}
	})
	status, body, _ = alice.do("POST", promotePath, map[string]any{"to_environment": "staging"})
	if status != http.StatusConflict || body["detail"] != "conflict: environment `staging` is being deleted" {
		t.Fatalf("expected 409 for a deleting project, got %d: %v", status, body)
	}
}

// An app with a configuration but no release yet cannot be promoted.
func TestPromoteAppWithoutReleaseIsRefused(t *testing.T) {
	f := newFixture(t)
	projectID, _, tgt := f.sqlApp()
	f.withTenant(func(ctx context.Context, tn *store.Tenant, _ store.Project) {
		// A source in the revision itself: without a release there is no
		// image to add, and a spec needs one.
		config := jsonValue(t, `{"source":{"image":"nginx:1.27"},"runtime":{"processes":{"web":{"port":80}}}}`)
		if _, _, err := tn.CreateConfigRevision(ctx, projectID, tgt, config, "user:seed"); err != nil {
			t.Fatal(err)
		}
	})
	alice := f.signIn("alice@example.com", seedPassword)
	createStaging(t, alice)
	status, body, _ := alice.do("POST", promotePath, map[string]any{"to_environment": "staging"})
	if status != http.StatusConflict || body["detail"] != "conflict: app `api` has no release yet" {
		t.Fatalf("expected 409 without a release, got %d: %v", status, body)
	}
}

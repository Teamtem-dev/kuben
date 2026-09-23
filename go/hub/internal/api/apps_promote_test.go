package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// routes/apps/promote.rs promotion_keeps_target_domains_and_scaling_and_reports_changes.
func TestPromotionKeepsTargetDomainsAndScalingAndReportsChanges(t *testing.T) {
	source := sampleSpec(t)
	source.Source = v1alpha1.SourceFromImage("nginx:1.28")
	secretURL := "db/url"
	source.Env = []v1alpha1.EnvVar{
		api.ToCRDEnv(gen.EnvVarDto{
			Name:   "DATABASE_URL",
			Secret: gen.NewOptNilSecretRef(gen.SecretRef{Name: "db", Key: "url"}),
		}),
	}
	_ = secretURL

	target := sampleSpec(t)
	target.Domains = api.ToDomains([]string{"shop.acme.com"})
	if web, ok := target.Runtime.Processes["web"]; ok {
		web.Replicas = v1alpha1.Replicas{Min: 3, Max: 6}
		web.Size = "large"
		target.Runtime.Processes["web"] = web
	}

	promoted := api.PromoteSpec(&source, &target)
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

	changes := api.SpecChanges(&target, &promoted)
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

// HTTP promotion: dry-run and live promote from prod to staging.
func TestPromoteApp(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)

	// 1. Dry run
	status, body, _ := alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/promote", map[string]any{
		"to_environment": "staging",
		"dry_run":        true,
	})
	if status != http.StatusOK {
		t.Fatalf("dry run promote status %d: %v", status, body)
	}
	if dryRun, _ := body["dry_run"].(bool); !dryRun {
		t.Fatalf("expected dry_run=true: %v", body)
	}
	if created, _ := body["created"].(bool); !created {
		t.Fatalf("expected created=true in staging: %v", body)
	}
	if appVal, exists := body["app"]; exists && appVal != nil {
		t.Fatalf("expected app=null in dry run: %v", body)
	}

	// 2. Real promote
	status, body, _ = alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/promote", map[string]any{
		"to_environment": "staging",
		"dry_run":        false,
	})
	if status != http.StatusOK {
		t.Fatalf("live promote status %d: %v", status, body)
	}
	if dryRun, _ := body["dry_run"].(bool); dryRun {
		t.Fatalf("expected dry_run=false: %v", body)
	}
	if created, _ := body["created"].(bool); !created {
		t.Fatalf("expected created=true: %v", body)
	}
	appObj, ok := body["app"].(map[string]any)
	if !ok || appObj == nil {
		t.Fatalf("expected app object in result: %v", body)
	}
	if env, _ := appObj["environment"].(string); env != "staging" {
		t.Fatalf("expected environment=staging: %v", appObj)
	}

	// 3. Same environment rejected
	status, body, _ = alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/promote", map[string]any{
		"to_environment": "prod",
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for same environment, got %d: %v", status, body)
	}
}

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// Ported from tests/http.rs: m5_public_status_pages_show_only_public_facts.
func TestPublicStatusPagesShowOnlyPublicFacts(t *testing.T) {
	f := newFixture(t)
	_, _, tgt := f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	anon := f.browser()

	const publicURL = "/api/v1/public/status/shop-status"
	if got := anon.status("GET", publicURL, nil); got != http.StatusNotFound {
		t.Fatalf("no page yet: status %d", got)
	}

	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if _, _, err := tn.OpenIncident(ctx, store.NewIncident{
		Target:    opt.Some(tgt),
		Kind:      "deployment.failed",
		Severity:  "critical",
		DedupeKey: "k",
		Title:     "The deployment of shop/prod/api failed",
		Detail:    opt.Some("secret internal detail"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	const pageURL = "/api/v1/projects/shop/status-page"
	body := map[string]any{"slug": "shop-status", "title": "Shop", "environments": []string{"prod"}}
	for _, c := range []struct {
		what   string
		client *client
		body   map[string]any
		want   int
	}{
		{"viewers cannot publish", bob, body, http.StatusForbidden},
		{"bad slug", alice, map[string]any{"slug": "Shop Status", "title": "Shop", "environments": []string{"prod"}}, http.StatusUnprocessableEntity},
		{"unknown environment", alice, map[string]any{"slug": "shop-status", "title": "Shop", "environments": []string{"nope"}}, http.StatusUnprocessableEntity},
		{"no environments", alice, map[string]any{"slug": "shop-status", "title": "Shop", "environments": []string{}}, http.StatusUnprocessableEntity},
		{"blank title", alice, map[string]any{"slug": "shop-status", "title": "  ", "environments": []string{"prod"}}, http.StatusUnprocessableEntity},
	} {
		if got := c.client.status("PUT", pageURL, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", c.what, got, c.want)
		}
	}

	status, saved, _ := alice.do("PUT", pageURL, body)
	if status != http.StatusOK || saved["path"] != "/status/shop-status" {
		t.Fatalf("publish: %d %v", status, saved)
	}
	status, settings, _ := alice.do("GET", pageURL, nil)
	if status != http.StatusOK {
		t.Fatalf("settings: %d %v", status, settings)
	}
	// The route and the store each read their clock: the stamps may differ.
	delete(saved, "updatedAt")
	delete(settings, "updatedAt")
	if diff := cmp.Diff(saved, settings); diff != "" {
		t.Errorf("settings differ from the saved page (-saved +read):\n%s", diff)
	}

	status, shown, header := anon.do("GET", publicURL, nil)
	if status != http.StatusOK {
		t.Fatalf("public page: %d %v", status, shown)
	}
	if got := header.Get("Cache-Control"); got != "public, max-age=15" {
		t.Errorf("Cache-Control %q", got)
	}
	if shown["title"] != "Shop" || shown["status"] != "degraded" {
		t.Errorf("title and status: %v %v", shown["title"], shown["status"])
	}
	wantComponents := []any{map[string]any{"name": "API", "status": "degraded"}}
	if diff := cmp.Diff(wantComponents, shown["components"]); diff != "" {
		t.Errorf("components (-want +got):\n%s", diff)
	}
	incidents, _ := shown["incidents"].([]any)
	if len(incidents) == 0 {
		t.Fatalf("no incidents in %v", shown)
	}
	first, _ := incidents[0].(map[string]any)
	if first["severity"] != "critical" || first["component"] != "API" {
		t.Errorf("first incident %v", first)
	}

	raw, err := json.Marshal(shown)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{
		"secret internal detail",
		"shop/prod/api",
		"kb-shop-prod",
		tgt.UUID().String(),
		"deployment.failed",
	} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%s leaked: %s", leak, raw)
		}
	}

	for _, c := range []struct {
		what   string
		client *client
		method string
		url    string
		want   int
	}{
		{"take the page down", alice, "DELETE", pageURL, http.StatusNoContent},
		{"taken down at once", anon, "GET", publicURL, http.StatusNotFound},
		{"already down", alice, "DELETE", pageURL, http.StatusNotFound},
		{"no settings left", alice, "GET", pageURL, http.StatusNotFound},
	} {
		if got := c.client.status(c.method, c.url, nil); got != c.want {
			t.Errorf("%s: status %d, want %d", c.what, got, c.want)
		}
	}
}

func TestStatusPageDto(t *testing.T) {
	page := store.StatusPage{
		Project:      ids.New[ids.Project](),
		Org:          ids.New[ids.Org](),
		Slug:         "shop-status",
		Title:        "Shop Service Status",
		Enabled:      true,
		Environments: []ids.EnvironmentID{ids.New[ids.Environment]()},
		UpdatedBy:    "user:alice",
		UpdatedAt:    1_700_000_000_000,
	}
	want := &gen.StatusPageDto{
		Slug:         "shop-status",
		Title:        "Shop Service Status",
		Enabled:      true,
		Environments: []string{"prod"},
		Path:         "/status/shop-status",
		UpdatedBy:    "user:alice",
		UpdatedAt:    httpapi.Timestamp(1_700_000_000_000),
	}
	if diff := cmp.Diff(want, httpapi.StatusPageDtoOf(page, []string{"prod"})); diff != "" {
		t.Errorf("dto (-want +got):\n%s", diff)
	}
}

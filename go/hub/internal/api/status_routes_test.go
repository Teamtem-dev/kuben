package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// Ported from tests/http.rs: m5_public_status_pages_show_only_public_facts.
func TestPublicStatusPagesShowOnlyPublicFacts(t *testing.T) {
	f := newFixture(t)
	_, _, tgt := f.sqlApp()

	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)

	const publicURL = "/api/v1/public/status/shop-status"
	anon := f.browser()

	// 1. Initial state: status page doesn't exist -> 404
	status, _, _ := anon.do("GET", publicURL, nil)
	assert.Equal(t, 404, status)

	// 2. Open an incident on target with secret internal detail
	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	require.NoError(t, err)
	incident := store.NewIncident{
		Target:    opt.Some(tgt),
		Kind:      "deployment.failed",
		Severity:  "critical",
		DedupeKey: "k",
		Title:     "The deployment of shop/prod/api failed",
		Detail:    opt.Some("secret internal detail"),
	}
	_, _, err = tn.OpenIncident(ctx, incident)
	require.NoError(t, err)
	require.NoError(t, tn.Commit(ctx))

	const pageURL = "/api/v1/projects/shop/status-page"
	body := map[string]any{
		"slug":         "shop-status",
		"title":        "Shop",
		"environments": []string{"prod"},
	}

	// 3. Bob (viewer) cannot configure status page -> 403
	status, _, _ = bob.do("PUT", pageURL, body)
	assert.Equal(t, 403, status)

	// 4. Invalid slug -> 422
	badSlug := map[string]any{"slug": "Shop Status", "title": "Shop", "environments": []string{"prod"}}
	status, _, _ = alice.do("PUT", pageURL, badSlug)
	assert.Equal(t, 422, status)

	// 5. Unknown environment -> 422
	badEnv := map[string]any{"slug": "shop-status", "title": "Shop", "environments": []string{"nope"}}
	status, _, _ = alice.do("PUT", pageURL, badEnv)
	assert.Equal(t, 422, status)

	// 6. Empty environments -> 422
	emptyEnvs := map[string]any{"slug": "shop-status", "title": "Shop", "environments": []string{}}
	status, _, _ = alice.do("PUT", pageURL, emptyEnvs)
	assert.Equal(t, 422, status)

	// 7. Empty title -> 422
	emptyTitle := map[string]any{"slug": "shop-status", "title": "  ", "environments": []string{"prod"}}
	status, _, _ = alice.do("PUT", pageURL, emptyTitle)
	assert.Equal(t, 422, status)

	// 8. Alice (owner) configures status page -> 200 OK
	status, saved, _ := alice.do("PUT", pageURL, body)
	assert.Equal(t, 200, status)
	assert.Equal(t, "/status/shop-status", saved["path"])
	assert.Equal(t, "shop-status", saved["slug"])
	assert.Equal(t, "Shop", saved["title"])
	assert.Equal(t, true, saved["enabled"])

	// 9. Alice reads configuration -> 200 OK
	status, got, _ := alice.do("GET", pageURL, nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, saved["path"], got["path"])
	assert.Equal(t, saved["slug"], got["slug"])

	// 10. Anon reads public status page -> 200 OK
	status, shown, header := anon.do("GET", publicURL, nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, "public, max-age=15", header.Get("Cache-Control"))
	assert.Equal(t, "Shop", shown["title"])
	assert.Equal(t, "degraded", shown["status"])

	components, ok := shown["components"].([]any)
	require.True(t, ok)
	require.Len(t, components, 1)
	comp, ok := components[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api", comp["name"])
	assert.Equal(t, "degraded", comp["status"])

	incidents, ok := shown["incidents"].([]any)
	require.True(t, ok)
	require.Len(t, incidents, 1)
	inc, ok := incidents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "critical", inc["severity"])
	assert.Equal(t, "api", inc["component"])

	rawBytes, err := json.Marshal(shown)
	require.NoError(t, err)
	text := string(rawBytes)
	for _, leak := range []string{
		"secret internal detail",
		"shop/prod/api",
		"kb-shop-prod",
		tgt.UUID().String(),
		"deployment.failed",
	} {
		assert.False(t, strings.Contains(text, leak), "leaked %s in %s", leak, text)
	}

	// 11. Delete status page -> 204
	status, _, _ = alice.do("DELETE", pageURL, nil)
	assert.Equal(t, 204, status)

	// 12. Public status is gone at once (cache invalidated) -> 404
	status, _, _ = anon.do("GET", publicURL, nil)
	assert.Equal(t, 404, status)

	// 13. Second DELETE returns 404
	status, _, _ = alice.do("DELETE", pageURL, nil)
	assert.Equal(t, 404, status)

	// 14. GET settings returns 404
	status, _, _ = alice.do("GET", pageURL, nil)
	assert.Equal(t, 404, status)
}

func TestStatusPageDto(t *testing.T) {
	projID := ids.New[ids.Project]()
	orgID := ids.New[ids.Org]()
	envID := ids.New[ids.Environment]()

	page := store.StatusPage{
		Project:      projID,
		Org:          orgID,
		Slug:         "shop-status",
		Title:        "Shop Service Status",
		Enabled:      true,
		Environments: []ids.EnvironmentID{envID},
		UpdatedBy:    "user:alice",
		UpdatedAt:    1_700_000_000_000,
	}

	dto := api.StatusPageDtoOf(page, []string{"prod"})
	assert.Equal(t, "shop-status", dto.Slug)
	assert.Equal(t, "Shop Service Status", dto.Title)
	assert.True(t, dto.Enabled)
	assert.Equal(t, []string{"prod"}, dto.Environments)
	assert.Equal(t, "/status/shop-status", dto.Path)
	assert.Equal(t, "user:alice", dto.UpdatedBy)
	assert.Equal(t, api.Timestamp(1_700_000_000_000), dto.UpdatedAt)
}

package api_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

func TestEveryTemplateBuildsValidObjects(t *testing.T) {
	for _, tpl := range api.TemplatesCatalogue {
		r, err := api.RenderTemplate(tpl, "svc")
		require.NoError(t, err, "%s: render error", tpl.ID)

		err = api.ValidateSpec(&r.Spec)
		require.NoError(t, err, "%s: spec validation error", tpl.ID)

		app := &v1alpha1.App{Spec: r.Spec}
		app.Name = "svc"
		app.Namespace = "kb-shop-prod"

		caps := render.CapabilitiesOf(render.DefaultPlatform())
		plan, err := render.Render(app, caps)
		require.NoError(t, err, "%s: plan rendering error", tpl.ID)

		var deployments, services, pvcs int
		for _, item := range plan.Inventory {
			switch item.Kind {
			case "Deployment":
				deployments++
			case "Service":
				services++
			case "PersistentVolumeClaim":
				pvcs++
			}
		}
		assert.Equal(t, 1, deployments, "%s: expected 1 deployment", tpl.ID)
		assert.Equal(t, 1, services, "%s: expected 1 service", tpl.ID)
		assert.Equal(t, len(tpl.Volumes), pvcs, "%s: expected %d persistent volume claims", tpl.ID, len(tpl.Volumes))

		specBytes, err := json.Marshal(r.Spec)
		require.NoError(t, err)
		for _, value := range r.Secret {
			if len(value) == api.SecretLen {
				assert.NotContains(t, string(specBytes), value, "%s: secret value leaked into spec", tpl.ID)
				assert.NotContains(t, plan.ResourcesJSON(), value, "%s: secret value leaked into plan", tpl.ID)
			}
		}
	}
}

func TestCredentialsAreRandomAndConnectionUrlsComplete(t *testing.T) {
	var pg api.TemplateDef
	found := false
	for _, tpl := range api.TemplatesCatalogue {
		if tpl.ID == "postgres" {
			pg = tpl
			found = true
			break
		}
	}
	require.True(t, found, "postgres template found")

	a, err := api.RenderTemplate(pg, "db")
	require.NoError(t, err)
	b, err := api.RenderTemplate(pg, "db")
	require.NoError(t, err)

	assert.Equal(t, api.SecretLen, len(a.Secret["password"]))
	assert.NotEqual(t, a.Secret["password"], b.Secret["password"], "passwords must be distinct random strings")
	assert.Equal(t, fmt.Sprintf("postgres://app:%s@db:5432/app", a.Secret["password"]), a.Secret["url"])
	assert.Equal(t, "db-credentials", api.CredentialsSecret("db"))

	ids := make(map[string]bool)
	for _, tpl := range api.TemplatesCatalogue {
		assert.False(t, ids[tpl.ID], "template id %s duplicated", tpl.ID)
		ids[tpl.ID] = true
	}
	assert.Equal(t, len(api.TemplatesCatalogue), len(ids), "template ids are unique")
}

func TestScenario8TemplateCatalogue(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()

	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)

	// GET /api/v1/templates as bob (viewer)
	status, list := bob.list("/api/v1/templates")
	assert.Equal(t, 200, status)
	assert.Len(t, list, 8)

	var pg map[string]any
	for _, item := range list {
		if item["id"] == "postgres" {
			pg = item
			break
		}
	}
	require.NotNil(t, pg, "postgres found in templates")
	assert.Equal(t, "tcp", pg["protocol"])
	keys, ok := pg["connection_keys"].([]any)
	require.True(t, ok)
	assert.Contains(t, keys, "url")

	const deployURL = "/api/v1/projects/shop/environments/prod/templates/postgres"
	deployBody := map[string]any{"name": "db"}

	// Viewer cannot deploy template -> 403 Forbidden
	assert.Equal(t, 403, bob.status("POST", deployURL, deployBody), "viewers cannot deploy templates")

	// Owner deploys template without cluster -> 503 Service Unavailable ("no cluster in tests")
	assert.Equal(t, 503, alice.status("POST", deployURL, deployBody), "no cluster in tests")

	// Non-existent template -> 404 Not Found
	const badTemplateURL = "/api/v1/projects/shop/environments/prod/templates/nope"
	assert.Equal(t, 404, alice.status("POST", badTemplateURL, deployBody), "template does not exist")

	// Invalid app name -> 422 Unprocessable Entity
	badNameBody := map[string]any{"name": "-invalid-name-"}
	assert.Equal(t, 422, alice.status("POST", deployURL, badNameBody), "invalid dns label")
}

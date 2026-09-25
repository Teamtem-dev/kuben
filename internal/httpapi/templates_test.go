package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// template is the catalogue entry id; the test fails without it.
func template(t *testing.T, id string) httpapi.TemplateDef {
	t.Helper()
	for _, tpl := range httpapi.TemplatesCatalogue() {
		if tpl.ID == id {
			return tpl
		}
	}
	t.Fatalf("no template %q", id)
	return httpapi.TemplateDef{}
}

// Ported from routes/templates.rs: every_template_builds_valid_objects.
func TestEveryTemplateBuildsValidObjects(t *testing.T) {
	for _, tpl := range httpapi.TemplatesCatalogue() {
		t.Run(tpl.ID, func(t *testing.T) {
			r, err := httpapi.RenderTemplate(tpl, "svc")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if err := httpapi.ValidateSpec(&r.Spec); err != nil {
				t.Fatalf("validate the spec: %v", err)
			}

			app := &v1alpha1.App{Spec: r.Spec}
			app.Name = "svc"
			app.Namespace = "kb-shop-prod"
			plan, err := render.Render(app, render.CapabilitiesOf(render.DefaultPlatform()))
			if err != nil {
				t.Fatalf("render the plan: %v", err)
			}
			kinds := map[string]int{}
			for _, item := range plan.Inventory {
				kinds[item.Kind]++
			}
			want := map[string]int{"Deployment": 1, "Service": 1, "PersistentVolumeClaim": len(tpl.Volumes)}
			got := map[string]int{
				"Deployment":            kinds["Deployment"],
				"Service":               kinds["Service"],
				"PersistentVolumeClaim": kinds["PersistentVolumeClaim"],
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("objects (-want +got):\n%s", diff)
			}

			spec, err := json.Marshal(r.Spec)
			if err != nil {
				t.Fatal(err)
			}
			resources := plan.ResourcesJSON()
			for _, value := range r.Secret {
				if len(value) != httpapi.SecretLen {
					continue
				}
				if strings.Contains(string(spec), value) {
					t.Errorf("a secret value leaked into the spec")
				}
				if strings.Contains(resources, value) {
					t.Errorf("a secret value leaked into the plan")
				}
			}
		})
	}
}

// Ported from routes/templates.rs: credentials_are_random_and_connection_urls_complete.
func TestCredentialsAreRandomAndConnectionUrlsComplete(t *testing.T) {
	pg := template(t, "postgres")
	a, err := httpapi.RenderTemplate(pg, "db")
	if err != nil {
		t.Fatal(err)
	}
	b, err := httpapi.RenderTemplate(pg, "db")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(a.Secret["password"]); got != httpapi.SecretLen {
		t.Errorf("password length %d, want %d", got, httpapi.SecretLen)
	}
	if a.Secret["password"] == b.Secret["password"] {
		t.Errorf("two renders share a password")
	}
	if want := fmt.Sprintf("postgres://app:%s@db:5432/app", a.Secret["password"]); a.Secret["url"] != want {
		t.Errorf("url %q, want %q", a.Secret["url"], want)
	}
	if got := httpapi.CredentialsSecret("db"); got != "db-credentials" {
		t.Errorf("credentials secret %q", got)
	}

	seen := map[string]bool{}
	for _, tpl := range httpapi.TemplatesCatalogue() {
		if seen[tpl.ID] {
			t.Errorf("template id %q is not unique", tpl.ID)
		}
		seen[tpl.ID] = true
	}
}

// The optional fields become the CRD's pointers, and a rendered spec shares
// nothing with the catalogue.
func TestRenderedSpecsOwnTheirValues(t *testing.T) {
	gitea, err := httpapi.RenderTemplate(template(t, "gitea"), "git")
	if err != nil {
		t.Fatal(err)
	}
	if hc := gitea.Spec.Runtime.HealthCheck; hc == nil || hc.Path != "/api/healthz" {
		t.Errorf("gitea health check %+v", hc)
	}
	if fs := gitea.Spec.Runtime.FSGroup; fs == nil || *fs != 1000 {
		t.Errorf("gitea fsGroup %v", fs)
	}

	whoamiDef := template(t, "whoami")
	whoami, err := httpapi.RenderTemplate(whoamiDef, "echo")
	if err != nil {
		t.Fatal(err)
	}
	if whoami.Spec.Runtime.HealthCheck != nil || whoami.Spec.Runtime.FSGroup != nil {
		t.Errorf("whoami has no health check and no fsGroup: %+v", whoami.Spec.Runtime)
	}
	web := whoami.Spec.Runtime.Processes["web"]
	web.Command[0] = "changed"
	if whoamiDef.Command[0] != "/whoami" {
		t.Errorf("the rendered command aliases the template: %v", whoamiDef.Command)
	}
	if got := template(t, "whoami").Command[0]; got != "/whoami" {
		t.Errorf("the catalogue changed: %q", got)
	}
}

// templateJSON is one entry of GET /api/v1/templates, as Rust's TemplateDto
// serializes it.
func templateJSON(
	id, name, description, category, image string, port int, protocol string, volumes, keys []any,
) map[string]any {
	return map[string]any{
		"id":              id,
		"name":            name,
		"description":     description,
		"category":        category,
		"image":           image,
		"port":            json.Number(fmt.Sprint(port)),
		"protocol":        protocol,
		"volumes":         volumes,
		"connection_keys": keys,
	}
}

// The whole catalogue as served, derived from routes/templates.rs TEMPLATES.
func TestTemplateCatalogueBody(t *testing.T) {
	f := newFixture(t)
	bob := f.signIn("bob@example.com", seedPassword)

	req, err := http.NewRequest("GET", bob.base+"/api/v1/templates", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(httpx.ClientHeader, "console")
	resp := bob.send(req)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	got, err := jsonx.DecodeAny(raw)
	if err != nil {
		t.Fatal(err)
	}

	want := []any{
		templateJSON("postgres", "PostgreSQL 17",
			"Relational database. Other apps connect with the `url` key of the credentials secret.",
			"database", "postgres:17-alpine", 5432, "tcp",
			[]any{"/var/lib/postgresql/data (5Gi)"},
			[]any{"database", "host", "password", "port", "url", "username"}),
		templateJSON("redis", "Redis 7",
			"In-memory cache and queue with append-only persistence and a password.",
			"database", "redis:7-alpine", 6379, "tcp",
			[]any{"/data (1Gi)"},
			[]any{"host", "password", "port", "url"}),
		templateJSON("mariadb", "MariaDB 11",
			"MySQL-compatible database. Connect with the `url` key of the credentials secret.",
			"database", "mariadb:11", 3306, "tcp",
			[]any{"/var/lib/mysql (5Gi)"},
			[]any{"database", "host", "password", "port", "root-password", "url", "username"}),
		templateJSON("n8n", "n8n",
			"Workflow automation. Credentials are encrypted with a generated key.",
			"automation", "n8nio/n8n:stable", 5678, "http",
			[]any{"/home/node/.n8n (1Gi)"},
			[]any{"encryption-key"}),
		templateJSON("uptime-kuma", "Uptime Kuma",
			"Self-hosted uptime monitoring with status pages.",
			"monitoring", "louislam/uptime-kuma:1", 3001, "http",
			[]any{"/app/data (1Gi)"},
			[]any{}),
		templateJSON("vaultwarden", "Vaultwarden",
			"Bitwarden-compatible password manager. Sign-ups are closed; invite users from /admin with the generated admin token.",
			"security", "vaultwarden/server:latest", 80, "http",
			[]any{"/data (1Gi)"},
			[]any{"admin-token"}),
		templateJSON("gitea", "Gitea",
			"Lightweight Git hosting (rootless image). Finish the installer on first visit.",
			"development", "gitea/gitea:1-rootless", 3000, "http",
			[]any{"/var/lib/gitea (5Gi)", "/etc/gitea (100Mi)"},
			[]any{}),
		templateJSON("whoami", "whoami",
			"Tiny HTTP echo service to test domains, TLS and routing.",
			"sample", "traefik/whoami:v1.10", 8080, "http",
			[]any{},
			[]any{}),
	}
	if diff := cmp.Diff(any(want), got); diff != "" {
		t.Errorf("GET /api/v1/templates (-want +got):\n%s", diff)
	}
}

// Ported from tests/http.rs: scenario8_template_catalogue.
func TestScenario8TemplateCatalogue(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)

	status, list := bob.list("/api/v1/templates")
	if status != http.StatusOK {
		t.Fatalf("list templates: %d", status)
	}
	if len(list) != 8 {
		t.Fatalf("%d templates, want 8", len(list))
	}
	var pg map[string]any
	for _, item := range list {
		if item["id"] == "postgres" {
			pg = item
		}
	}
	if pg == nil {
		t.Fatal("no postgres template")
	}
	if pg["protocol"] != "tcp" {
		t.Errorf("postgres protocol %v", pg["protocol"])
	}
	keys, _ := pg["connection_keys"].([]any)
	if !slices.Contains(keys, any("url")) {
		t.Errorf("postgres connection keys %v lack url", keys)
	}

	const deployURL = "/api/v1/projects/shop/environments/prod/templates/postgres"
	name := map[string]any{"name": "db"}
	for _, c := range []struct {
		what   string
		client *client
		url    string
		body   map[string]any
		want   int
	}{
		{"viewers cannot deploy templates", bob, deployURL, name, http.StatusForbidden},
		{"no cluster in tests", alice, deployURL, name, http.StatusServiceUnavailable},
		{"unknown template", alice, "/api/v1/projects/shop/environments/prod/templates/nope", name, http.StatusNotFound},
		{"invalid app name", alice, deployURL, map[string]any{"name": "-invalid-name-"}, http.StatusUnprocessableEntity},
	} {
		if got := c.client.status("POST", c.url, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", c.what, got, c.want)
		}
	}
}

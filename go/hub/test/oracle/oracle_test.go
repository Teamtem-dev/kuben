package oracle_test

import (
	"os"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/test/oracle"
)

// The walking skeleton (G2): first-run setup, sign-in, the session, the
// project list, sign-out, and the error paths around them. More scenarios
// join with every slice.
var skeleton = []oracle.Step{
	{Name: "setup status", Method: "GET", Path: "/api/v1/setup"},
	{Name: "me before setup", Method: "GET", Path: "/api/v1/me"},
	{Name: "setup without a name", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": " ", "email": "admin@example.com", "password": "correct horse battery staple",
	}},
	{Name: "setup", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": "ACME", "email": "Admin@Example.com", "password": "correct horse battery staple",
	}},
	{Name: "setup again", Method: "POST", Path: "/api/v1/setup", Body: map[string]any{
		"org_name": "ACME", "email": "b@example.com", "password": "correct horse battery staple",
	}},
	{Name: "me after setup", Method: "GET", Path: "/api/v1/me"},
	{Name: "projects", Method: "GET", Path: "/api/v1/projects"},
	{Name: "unknown project", Method: "GET", Path: "/api/v1/projects/nope"},
	{Name: "unknown API route", Method: "GET", Path: "/api/v1/nothing-here"},
	{Name: "logout", Method: "POST", Path: "/api/v1/auth/logout"},
	{Name: "me after logout", Method: "GET", Path: "/api/v1/me"},
	{Name: "wrong password", Method: "POST", Path: "/api/v1/auth/login", Body: map[string]any{
		"email": "admin@example.com", "password": "wrong",
	}},
	{Name: "login", Method: "POST", Path: "/api/v1/auth/login", Body: map[string]any{
		"email": "ADMIN@example.com ", "password": "correct horse battery staple",
	}},
	{Name: "me after login", Method: "GET", Path: "/api/v1/me"},
	{
		Name: "mutation without the client header", Method: "POST", Path: "/api/v1/auth/logout",
		Headers: map[string]string{"X-Kuben-Client": ""},
	},
	{Name: "console page", Method: "GET", Path: "/projects/shop"},
	{Name: "livez", Method: "GET", Path: "/livez"},
}

// apps is the app collection of the S1 scenario; pinned is an image by
// digest, so neither server asks a registry.
const (
	apps   = "/api/v1/projects/shop/environments/prod/apps"
	pinned = "docker.io/library/nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// Slice S1: templates, projects, environments, policies, status pages,
// apps and their deployments, members, tokens.
var sliceS1 = []oracle.Step{
	{Name: "templates list", Method: "GET", Path: "/api/v1/templates"},
	{Name: "create project", Method: "POST", Path: "/api/v1/projects", Body: map[string]any{
		"name": "shop", "display_name": "Shop",
	}},
	{Name: "get project", Method: "GET", Path: "/api/v1/projects/shop"},
	{Name: "create environment", Method: "POST", Path: "/api/v1/projects/shop/environments", Body: map[string]any{
		"name": "prod", "env_type": "production",
	}},
	{Name: "get environment", Method: "GET", Path: "/api/v1/projects/shop/environments/prod"},
	{Name: "get policy", Method: "GET", Path: "/api/v1/projects/shop/environments/prod/policy"},
	{Name: "put policy", Method: "PUT", Path: "/api/v1/projects/shop/environments/prod/policy", Body: map[string]any{
		"requiredApprovals": 1, "deployRole": "developer", "approveRole": "admin",
	}},
	{Name: "status page not found", Method: "GET", Path: "/api/v1/projects/shop/status-page"},
	{Name: "put status page", Method: "PUT", Path: "/api/v1/projects/shop/status-page", Body: map[string]any{
		"slug": "shop-status", "title": "Shop", "environments": []string{"prod"},
	}},
	{Name: "get status page", Method: "GET", Path: "/api/v1/projects/shop/status-page"},
	{Name: "public status", Method: "GET", Path: "/api/v1/public/status/shop-status"},
	{Name: "delete status page", Method: "DELETE", Path: "/api/v1/projects/shop/status-page"},
	{Name: "public status after delete", Method: "GET", Path: "/api/v1/public/status/shop-status"},
	{Name: "create app", Method: "POST", Path: apps, Body: map[string]any{
		"name": "web", "image": pinned, "port": 8080, "replicas": 2, "size": "small",
		"env": []map[string]any{{"name": "MODE", "value": "prod"}}, "health_check_path": "/healthz",
	}},
	{Name: "create app again", Method: "POST", Path: apps, Body: map[string]any{"name": "web", "image": pinned, "port": 8080}},
	{Name: "create app without an image", Method: "POST", Path: apps, Body: map[string]any{"name": "worker"}},
	{Name: "create app with a bad name", Method: "POST", Path: apps, Body: map[string]any{"name": "Web!", "image": pinned}},
	{Name: "list apps", Method: "GET", Path: apps},
	{Name: "get app", Method: "GET", Path: apps + "/web"},
	{Name: "unknown app", Method: "GET", Path: apps + "/nope"},
	{Name: "update app", Method: "PATCH", Path: apps + "/web", Body: map[string]any{"replicas": 3, "expected_generation": 1}},
	{Name: "update app stale", Method: "PATCH", Path: apps + "/web", Body: map[string]any{"replicas": 4, "expected_generation": 1}},
	{Name: "get app after update", Method: "GET", Path: apps + "/web"},
	{Name: "list deployments", Method: "GET", Path: apps + "/web/deployments"},
	{Name: "list releases", Method: "GET", Path: apps + "/web/releases"},
	{Name: "approval of an unknown run", Method: "GET", Path: apps + "/web/deployments/01a0d847-e59d-7aac-8b51-e3fa8881c73d/approval"},
	{Name: "logs without a cluster", Method: "GET", Path: apps + "/web/logs"},
	{Name: "delete app", Method: "DELETE", Path: apps + "/web"},
	{Name: "get app after delete", Method: "GET", Path: apps + "/web"},
	{Name: "list members", Method: "GET", Path: "/api/v1/members"},
	{Name: "invite member", Method: "POST", Path: "/api/v1/members", Body: map[string]any{"email": "Dev@Example.com", "role": "developer"}},
	{Name: "invite member again", Method: "POST", Path: "/api/v1/members", Body: map[string]any{"email": "dev@example.com", "role": "developer"}},
	{Name: "create token", Method: "POST", Path: "/api/v1/tokens", Body: map[string]any{
		"name": "test-token", "role": "admin",
	}},
	{Name: "list tokens", Method: "GET", Path: "/api/v1/tokens"},
}

// TestSkeletonMatchesRust needs both servers running on empty databases:
// KUBEN_ORACLE_RUST and KUBEN_ORACLE_GO are their base URLs.
func TestSkeletonMatchesRust(t *testing.T) {
	rustURL, goURL := os.Getenv("KUBEN_ORACLE_RUST"), os.Getenv("KUBEN_ORACLE_GO")
	if rustURL == "" || goURL == "" {
		t.Skip("set KUBEN_ORACLE_RUST and KUBEN_ORACLE_GO to compare the servers")
	}
	rust, err := oracle.NewClient(rustURL)
	if err != nil {
		t.Fatal(err)
	}
	goServer, err := oracle.NewClient(goURL)
	if err != nil {
		t.Fatal(err)
	}
	allSteps := append(skeleton, sliceS1...)
	for _, step := range allSteps {
		want, err := rust.Do(step)
		if err != nil {
			t.Fatal(err)
		}
		got, err := goServer.Do(step)
		if err != nil {
			t.Fatal(err)
		}
		if d := oracle.Diff(want, got); d != "" {
			t.Errorf("%s: %s", step.Name, d)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := `id 0192f3a1-0000-7000-8000-000000000001 at 2026-09-21T10:00:00.123Z token kbn_pat_0192f3a100007000800000000000000a_abc-DEF_1`
	want := `id <uuid> at <time> token <token>`
	if got := oracle.Normalize(in); got != want {
		t.Fatalf("got %s", got)
	}
}

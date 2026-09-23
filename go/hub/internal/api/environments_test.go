package api_test

import (
	"net/http"
	"testing"
)

// The environment half of tests/http.rs environments_and_apps_read_from_sql
// and viewers_can_read_but_not_write; the app half follows with the app
// routes.
func TestEnvironmentsReadAndWriteSQL(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)

	status, envs := alice.list("/api/v1/projects/shop/environments")
	if status != 200 || len(envs) != 1 || envs[0]["name"] != "prod" || envs[0]["resource_name"] != "shop-prod" ||
		envs[0]["namespace"] != "kb-shop-prod" {
		t.Fatalf("list: %d %v", status, envs)
	}
	if status, env, _ := alice.do("GET", "/api/v1/projects/shop/environments/prod", nil); status != 200 || env["project"] != "shop" {
		t.Fatalf("get: %d %v", status, env)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/nope", nil); status != http.StatusNotFound {
		t.Fatalf("unknown: %d", status)
	}

	if status := alice.status("POST", "/api/v1/projects/shop/environments", map[string]any{"name": "Staging"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid name: %d", status)
	}
	quota := map[string]any{"cpu": "lots"}
	if status := alice.status("POST", "/api/v1/projects/shop/environments", map[string]any{"name": "qa", "quota": quota}); status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid quota: %d", status)
	}
	status, created, _ := alice.do("POST", "/api/v1/projects/shop/environments",
		map[string]any{"name": "staging", "quota": map[string]any{"cpu": "4", "memory": "8Gi", "pods": 20}})
	if status != http.StatusCreated || created["namespace"] != "kb-shop-staging" || created["phase"] != "Pending" ||
		created["env_type"] != "standard" || created["ready"] != false {
		t.Fatalf("create: %d %v", status, created)
	}
	if status, problem, _ := alice.do("POST", "/api/v1/projects/shop/environments", map[string]any{"name": "staging"}); status != http.StatusConflict || problem["detail"] != "environment `staging` already exists" {
		t.Fatalf("a duplicate: %d %v", status, problem)
	}

	bob := f.signIn("bob@example.com", seedPassword)
	if status := bob.status("GET", "/api/v1/projects/shop/environments", nil); status != 200 {
		t.Fatalf("a viewer reads: %d", status)
	}
	if status, problem, _ := bob.do("POST", "/api/v1/projects/shop/environments", map[string]any{"name": "qa"}); status != http.StatusForbidden || problem["code"] != "forbidden" {
		t.Fatalf("a viewer writes: %d %v", status, problem)
	}
	if status := bob.status("DELETE", "/api/v1/projects/shop/environments/staging", nil); status != http.StatusForbidden {
		t.Fatalf("a viewer deletes: %d", status)
	}

	if status := alice.status("DELETE", "/api/v1/projects/shop/environments/staging", nil); status != http.StatusAccepted {
		t.Fatalf("delete: %d", status)
	}
	if status, problem, _ := alice.do("DELETE", "/api/v1/projects/shop/environments/staging", nil); status != http.StatusConflict || problem["detail"] != "environment `staging` is being deleted" {
		t.Fatalf("twice: %d %v", status, problem)
	}
	if status, env, _ := alice.do("GET", "/api/v1/projects/shop/environments/staging", nil); status != 200 || env["phase"] != "Terminating" || env["deleting"] != true {
		t.Fatalf("deleting: %d %v", status, env)
	}
}

package api_test

import (
	"net/http"
	"testing"
)

// tests/http.rs projects_are_tenant_scoped.
func TestProjectsAreTenantScoped(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)

	status, list := alice.list("/api/v1/projects")
	if status != 200 || len(list) != 1 || list[0]["name"] != "shop" {
		t.Fatalf("other tenants' projects are invisible: %d %v", status, list)
	}
	if status, project, _ := alice.do("GET", "/api/v1/projects/shop", nil); status != 200 || project["display_name"] != "Shop" {
		t.Fatalf("shop: %d %v", status, project)
	}
	if status := alice.status("GET", "/api/v1/projects/secret-project", nil); status != http.StatusNotFound {
		t.Fatalf("404, not 403: existence must not leak: %d", status)
	}

	// SQL holds projects: creating one needs no cluster.
	if status := alice.status("POST", "/api/v1/projects", map[string]any{"name": "Bad Name", "display_name": "x"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid name: %d", status)
	}
	status, created, _ := alice.do("POST", "/api/v1/projects", map[string]any{"name": "blog", "display_name": "Blog"})
	if status != http.StatusCreated || created["name"] != "blog" || created["ready"] != false {
		t.Fatalf("create: %d %v", status, created)
	}
	if status, problem, _ := alice.do("POST", "/api/v1/projects", map[string]any{"name": "blog", "display_name": "Again"}); status != http.StatusConflict || problem["detail"] != "project `blog` already exists" {
		t.Fatalf("a duplicate: %d %v", status, problem)
	}

	// A project with environments cannot be deleted; an empty one can.
	if status, problem, _ := alice.do("DELETE", "/api/v1/projects/shop", nil); status != http.StatusConflict || problem["code"] != "conflict" {
		t.Fatalf("with environments: %d %v", status, problem)
	}
	if status := alice.status("DELETE", "/api/v1/projects/blog", nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status, problem, _ := alice.do("DELETE", "/api/v1/projects/blog", nil); status != http.StatusConflict || problem["detail"] != "project `blog` is being deleted" {
		t.Fatalf("twice: %d %v", status, problem)
	}

	// A viewer reads but does not write.
	bob := f.signIn("bob@example.com", seedPassword)
	if status := bob.status("POST", "/api/v1/projects", map[string]any{"name": "qa", "display_name": "QA"}); status != http.StatusForbidden {
		t.Fatalf("a viewer creates: %d", status)
	}
}

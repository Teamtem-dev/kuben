package api_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
)

// A project is ready when its projection is, in its own organization only
// (routes/projects.rs ProjectDto::of and the org filter of list and scope).
func TestProjectReadinessComesFromItsOwnProjection(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	ready := func() (bool, bool) {
		t.Helper()
		status, one, _ := alice.do("GET", "/api/v1/projects/shop", nil)
		listed, all := alice.list("/api/v1/projects")
		if status != 200 || listed != 200 || len(all) != 1 {
			t.Fatalf("projects: %d %d %v", status, listed, all)
		}
		return one["ready"] == true, all[0]["ready"] == true
	}
	if one, listed := ready(); one || listed {
		t.Fatal("no projection yet: not ready")
	}

	// The same name in another organization's projection says nothing.
	f.projections.UpsertProject(projection.ProjectView{Name: "shop", Org: opt.Some("someone-else"), Ready: true})
	if one, listed := ready(); one || listed {
		t.Fatal("another organization's project")
	}
	f.projections.UpsertProject(projection.ProjectView{Name: "shop", Org: opt.Some(f.org.String()), Ready: true})
	if one, listed := ready(); !one || !listed {
		t.Fatal("its own ready projection")
	}
}

// routes/health.rs details: the projections' sequence and pod count.
func TestHealthDetailsCountThePods(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	f.projections.UpsertPod(projection.PodView{Key: "kb-shop-prod/api-1", Namespace: "kb-shop-prod", Name: "api-1"})
	status, body, _ := alice.do("GET", "/api/v1/healthz/details", nil)
	if status != 200 || body["pods"] != float64(1) || body["seq"] != float64(f.projections.Seq()) || body["cluster"] != false {
		t.Fatalf("details: %d %v", status, body)
	}
}

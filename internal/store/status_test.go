package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from repo/status.rs: status_pages_are_found_by_slug_only_when_enabled.
func TestStatusPagesAreFoundBySlugOnlyWhenEnabled(t *testing.T) {
	const stamped int64 = 1_700_000_000_000
	s := pgtest.Store(t).WithClock(clock.Fixed(stamped))
	ctx := t.Context()

	orgA := org(t, s, "a", "A")
	orgB := org(t, s, "b", "B")

	tnA := tenant(t, s, orgA)
	projectA := must[ids.ProjectID](t, "project")(tnA.CreateProject(ctx, "shop", "Shop"))
	envA := must[ids.EnvironmentID](t, "env")(tnA.CreateEnvironment(ctx, projectA, "prod", "Prod", true))

	page := store.StatusPage{
		Project:      projectA,
		Org:          orgA,
		Slug:         "shop-status",
		Title:        "Shop",
		Enabled:      true,
		Environments: []ids.EnvironmentID{envA},
		UpdatedBy:    "u",
		UpdatedAt:    42, // ignored: the store's clock stamps the page
	}
	if err := tnA.SetStatusPage(ctx, page); err != nil {
		t.Fatalf("set status page: %v", err)
	}

	target := uuid.Must(uuid.NewV7())
	incident := store.NewIncident{
		Project:     opt.Some(projectA),
		Environment: opt.Some(envA),
		Kind:        "deployment.failed",
		Severity:    "critical",
		DedupeKey:   "k",
		Title:       "internal words",
	}
	if _, _, err := tnA.OpenIncident(ctx, incident); err != nil {
		t.Fatalf("open incident: %v", err)
	}

	incidents, err := tnA.PublicIncidents(ctx, []uuid.UUID{target}, 0)
	if err != nil {
		t.Fatalf("public incidents: %v", err)
	}
	if len(incidents) != 0 {
		t.Fatalf("only the page's apps: %v", incidents)
	}

	commit(t, tnA)

	found, ok, err := s.PublicStatusPage(ctx, "shop-status")
	if err != nil {
		t.Fatalf("read public status page: %v", err)
	}
	if !ok {
		t.Fatalf("expected page to be found")
	}
	if found.Org != orgA || len(found.Environments) != 1 || found.Environments[0] != envA {
		t.Fatalf("unexpected found page: %+v", found)
	}
	if found.UpdatedAt != stamped {
		t.Fatalf("updated at %d, want the store clock's %d", found.UpdatedAt, stamped)
	}

	tnB := tenant(t, s, orgB)
	projectB := must[ids.ProjectID](t, "project")(tnB.CreateProject(ctx, "web", "Web"))
	envB := must[ids.EnvironmentID](t, "env")(tnB.CreateEnvironment(ctx, projectB, "prod", "Prod", true))
	taken := store.StatusPage{
		Project:      projectB,
		Org:          orgB,
		Slug:         page.Slug,
		Title:        page.Title,
		Enabled:      page.Enabled,
		Environments: []ids.EnvironmentID{envB},
		UpdatedBy:    page.UpdatedBy,
		UpdatedAt:    0,
	}
	err = tnB.SetStatusPage(ctx, taken)
	if err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("slugs are global: %v", err)
	}
	if err := tnB.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}

	page.Enabled = false
	tnA = tenant(t, s, orgA)
	if err := tnA.SetStatusPage(ctx, page); err != nil {
		t.Fatalf("set status page disabled: %v", err)
	}
	commit(t, tnA)

	_, ok, err = s.PublicStatusPage(ctx, "shop-status")
	if err != nil {
		t.Fatalf("read public status page: %v", err)
	}
	if ok {
		t.Fatalf("expected disabled status page not to be found")
	}

	tnA = tenant(t, s, orgA)
	deleted, err := tnA.DeleteStatusPage(ctx, projectA)
	if err != nil {
		t.Fatalf("delete status page: %v", err)
	}
	if !deleted {
		t.Fatalf("expected status page to be deleted")
	}
	_, ok, err = tnA.StatusPage(ctx, projectA)
	if err != nil {
		t.Fatalf("read status page: %v", err)
	}
	if ok {
		t.Fatalf("expected deleted status page to be gone")
	}
	commit(t, tnA)
}

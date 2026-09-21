package store_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from catalog.rs, the projects and environments half of
// lists_what_the_api_shows (apps and runs come with their repositories).

type shop struct {
	org         ids.OrgID
	project     ids.ProjectID
	environment ids.EnvironmentID
}

func newShop(t *testing.T, s *store.Store) shop {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProjectDescribed(ctx, "shop", "Shop", opt.Some("The online shop")))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironmentTyped(ctx, project, "prod", "Prod",
		store.Production, opt.Some[any](map[string]any{"cpu": "4"})))
	cluster := must[ids.ClusterID](t, "cluster")(tn.EnsureCluster(ctx, "primary"))
	if again := must[ids.ClusterID](t, "again")(tn.EnsureCluster(ctx, "primary")); again != cluster {
		t.Fatalf("ensure_cluster made a second cluster: %v, %v", again, cluster)
	}
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster, "kb-shop-prod"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	if err := tn.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return shop{org: o, project: project, environment: environment}
}

func TestListsTheProjectsAndEnvironmentsTheAPIShows(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	sh := newShop(t, s)
	tn := tenant(t, s, sh.org)

	projects := must[[]store.Project](t, "projects")(tn.Projects(ctx))
	if len(projects) != 1 {
		t.Fatalf("projects: %+v", projects)
	}
	if d, _ := projects[0].Description.Get(); d != "The online shop" || projects[0].Environments != 1 {
		t.Fatalf("project: %+v", projects[0])
	}
	if p, ok, err := tn.Project(ctx, "shop"); err != nil || !ok || p.ID != sh.project {
		t.Fatalf("project shop: %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := tn.Project(ctx, "nope"); err != nil || ok {
		t.Fatalf("project nope: %v, %v", ok, err)
	}

	envs := must[[]store.EnvironmentRecord](t, "environments")(tn.Environments(ctx, sh.project))
	if len(envs) != 1 {
		t.Fatalf("environments: %+v", envs)
	}
	e := envs[0]
	if e.EnvType != "production" || !e.Protected || e.ID != sh.environment {
		t.Fatalf("environment: %+v", e)
	}
	if ns, _ := e.Namespace.Get(); ns != "kb-shop-prod" {
		t.Fatalf("namespace: %+v", e.Namespace)
	}
	quota, _ := e.Quota.Get()
	if diff := cmp.Diff(any(map[string]any{"cpu": "4"}), quota); diff != "" {
		t.Fatalf("quota (-want +got):\n%s", diff)
	}
	if one, ok, err := tn.Environment(ctx, sh.project, "prod"); err != nil || !ok || one.ID != sh.environment {
		t.Fatalf("environment prod: %+v, %v, %v", one, ok, err)
	}
}

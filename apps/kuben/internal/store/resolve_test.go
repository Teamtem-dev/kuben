package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from resolve.rs.

// resolveShop is project `shop`, environment `production` and application
// `web` on it.
func resolveShop(t *testing.T, s *store.Store, o ids.OrgID) store.SQLScope {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster,
		"shop-production-"+o.String()))
	app := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, app, placement))
	commit(t, tn)
	return store.SQLScope{
		Project: opt.Some(project), Environment: opt.Some(environment), Target: opt.Some(tgt), Application: opt.Some(app),
	}
}

func named(slug string) store.Named { return store.Named{Slug: slug} }

func TestResolvesBySlugInsideTheOrganization(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "a", "A")
	b := org(t, s, "b", "B")
	expected := resolveShop(t, s, a)

	tn := tenant(t, s, a)
	got := must[store.SQLScope](t, "resolve")(tn.ResolveScope(ctx, named("shop"),
		opt.Some(named("production")), opt.Some(named("web"))))
	if got != expected {
		t.Fatalf("resolve: %+v, want %+v", got, expected)
	}
	got = must[store.SQLScope](t, "resolve")(tn.ResolveScope(ctx, named("shop"),
		opt.Some(named("staging")), opt.Some(named("web"))))
	if got != (store.SQLScope{Project: expected.Project}) {
		t.Fatalf("levels below a missing one are not looked up: %+v", got)
	}
	rollback(t, tn)

	tn = tenant(t, s, b)
	got = must[store.SQLScope](t, "resolve")(tn.ResolveScope(ctx, named("shop"), opt.None[store.Named](), opt.None[store.Named]()))
	if got != (store.SQLScope{}) {
		t.Fatalf("another organization's project is found: %+v", got)
	}
}

func TestTheKubernetesUIDWinsOverTheSlug(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	imported := resolveShop(t, s, o)
	tn := tenant(t, s, o)
	other := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "other", "Other"))
	commit(t, tn)

	projectUID, environmentUID, appUID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	project, _ := imported.Project.Get()
	environment, _ := imported.Environment.Get()
	tgt, _ := imported.Target.Get()
	for _, u := range []struct {
		sql string
		uid uuid.UUID
		id  any
	}{
		{"UPDATE projects SET legacy_uid = $1 WHERE id = $2", projectUID, project},
		{"UPDATE environments SET legacy_uid = $1 WHERE id = $2", environmentUID, environment},
		{"UPDATE application_targets SET legacy_uid = $1 WHERE id = $2", appUID, tgt},
	} {
		if _, err := s.TestExec(ctx, u.sql, u.uid, u.id); err != nil {
			t.Fatalf("record the Kubernetes UID: %v", err)
		}
	}

	tn = tenant(t, s, o)
	// The slugs name another project, and an environment and app that do
	// not exist; the UIDs name the imported ones.
	resolved := must[store.SQLScope](t, "resolve")(tn.ResolveScope(ctx,
		store.Named{Slug: "other", LegacyUID: opt.Some(projectUID)},
		opt.Some(store.Named{Slug: "renamed", LegacyUID: opt.Some(environmentUID)}),
		opt.Some(store.Named{Slug: "renamed", LegacyUID: opt.Some(appUID)})))
	if resolved != imported {
		t.Fatalf("resolved: %+v, want %+v", resolved, imported)
	}
	if p, _ := resolved.Project.Get(); p == other {
		t.Fatal("the slug won")
	}
}

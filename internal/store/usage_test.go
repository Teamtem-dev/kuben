package store_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// usage.rs live_targets_and_environments_are_counted_per_organization.
func TestLiveTargetsAndEnvironmentsAreCountedPerOrganization(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	other := org(t, s, "b", "B")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "prod", "Prod", false))
	must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "staging", "Staging", false))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster, "a-shop"))
	var targets [2]ids.TargetID
	for i, slug := range []string{"web", "api"} {
		application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, slug, slug))
		targets[i] = must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	}
	var newest any
	for _, text := range []string{`{"env":[]}`, `{"env":[{"name":"A","value":"1"}]}`} {
		config, err := wire.DecodeAny([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := tn.CreateConfigRevision(ctx, project, targets[0], config, "user:a"); err != nil || !found {
			t.Fatalf("revision: %v %v", found, err)
		}
		newest = config
	}
	if marked, err := tn.MarkTargetDeleting(ctx, targets[1]); err != nil || !marked {
		t.Fatalf("delete: %v %v", marked, err)
	}
	live, err := tn.LiveConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.LiveConfig{{Target: targets[0], Environment: environment, Config: opt.Some(newest)}}
	if diff := cmp.Diff(want, live, cmp.AllowUnexported(opt.Val[any]{}, ids.TargetID{}, ids.EnvironmentID{})); diff != "" {
		t.Fatalf("the newest configuration of live targets only: %s", diff)
	}
	if n, err := tn.LiveEnvironmentCount(ctx); err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
	commit(t, tn)

	tn = tenant(t, s, other)
	if live, err := tn.LiveConfigs(ctx); err != nil || len(live) != 0 {
		t.Fatalf("another organization: %v %v", live, err)
	}
	if n, err := tn.LiveEnvironmentCount(ctx); err != nil || n != 0 {
		t.Fatalf("another organization's count: %d %v", n, err)
	}
}

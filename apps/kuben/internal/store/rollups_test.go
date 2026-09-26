package store_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from repo/rollups.rs: hours_merge_and_are_found_by_name.
func TestHoursMergeAndAreFoundByName(t *testing.T) {
	const hour int64 = 3_600_000
	s := pgtest.Store(t)
	ctx := t.Context()
	tn := tenant(t, s, org(t, s, "a", "A"))
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "env")(tn.CreateEnvironment(ctx, project, "dev", "Dev", false))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "primary"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, "kb-shop-dev"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	target := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))

	found, ok, err := tn.TargetByName(ctx, "kb-shop-dev", "web")
	if err != nil || !ok || found != target {
		t.Fatalf("find web = %v, %v, %v; want %v", found, ok, err, target)
	}
	if _, ok, err := tn.TargetByName(ctx, "kb-shop-dev", "api"); err != nil || ok {
		t.Fatalf("find api = %v, %v; want nothing", ok, err)
	}
	for _, r := range []store.UsageRollup{
		{Hour: hour, CPUAvg: 100, CPUMax: 200, MemoryAvg: 1_000, MemoryMax: 2_000, Samples: 10},
		{Hour: hour, CPUAvg: 50, CPUMax: 300, MemoryAvg: 500, MemoryMax: 1_500, Samples: 5},
		{Hour: 2 * hour, CPUAvg: 1, CPUMax: 1, MemoryAvg: 1, MemoryMax: 1, Samples: 1},
	} {
		if err := tn.KeepUsage(ctx, target, r); err != nil {
			t.Fatalf("keep %+v: %v", r, err)
		}
	}
	hours := must[[]store.UsageHour](t, "read")(tn.UsageSince(ctx, target, 0))
	if len(hours) != 2 {
		t.Fatalf("hours = %+v, want 2", hours)
	}
	want := store.UsageHour{Hour: hour, CPUAvg: 100, CPUMax: 300, MemoryAvg: 1_000, MemoryMax: 2_000, Samples: 10}
	if diff := cmp.Diff(want, hours[0]); diff != "" {
		t.Errorf("the fuller sample's averages, the higher peaks (-want +got):\n%s", diff)
	}
	if got := must[[]store.UsageHour](t, "read")(tn.UsageSince(ctx, target, 2*hour)); len(got) != 1 {
		t.Errorf("since the second hour = %+v, want 1", got)
	}
}

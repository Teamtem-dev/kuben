package store_test

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from capabilities.rs.

func TestCapabilitiesAreRecordedPerOrganizationAndNeverBackwards(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "cap-a", "A")
	b := org(t, s, "cap-b", "B")
	tn := tenant(t, s, a)
	must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "primary"))
	commit(t, tn)

	first := map[string]any{"certManager": true}
	// Only A has a `primary` cluster.
	if n := must[int](t, "record")(s.RecordCapabilitiesEverywhere(ctx, "primary", first, 10)); n != 1 {
		t.Fatalf("recorded: %d", n)
	}
	tn = tenant(t, s, a)
	if must[bool](t, "older")(tn.RecordClusterCapabilities(ctx, "primary", map[string]any{"certManager": false}, 5)) {
		t.Fatal("an older observation never replaces a newer one")
	}
	if must[bool](t, "unknown cluster")(tn.RecordClusterCapabilities(ctx, "edge", first, 20)) {
		t.Fatal("an unknown cluster")
	}
	got, ok, err := tn.ClusterCapabilities(ctx, "primary")
	if err != nil || !ok {
		t.Fatalf("read: %v, %v", ok, err)
	}
	if diff := cmp.Diff(store.CapabilityRecord{Facts: map[string]any{"certManager": true}, ObservedAt: 10}, got); diff != "" {
		t.Fatalf("capabilities (-want +got):\n%s", diff)
	}
	newer := map[string]any{"certManager": true, "metricsApi": true}
	if !must[bool](t, "newer")(tn.RecordClusterCapabilities(ctx, "primary", newer, 30)) {
		t.Fatal("newer")
	}
	got, _, err = tn.ClusterCapabilities(ctx, "primary")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if diff := cmp.Diff(any(newer), got.Facts); diff != "" {
		t.Fatalf("facts (-want +got):\n%s", diff)
	}
	commit(t, tn)

	// Row-level security: B sees nothing of A's cluster.
	tn = tenant(t, s, b)
	if _, ok, err := tn.ClusterCapabilities(ctx, "primary"); err != nil || ok {
		t.Fatalf("B reads A's cluster: %v, %v", ok, err)
	}
	commit(t, tn)
	orgs := must[[]ids.OrgID](t, "orgs")(s.OrgIDs(ctx))
	if !slices.Contains(orgs, a) || !slices.Contains(orgs, b) {
		t.Fatalf("orgs: %v", orgs)
	}
	if named, ok, err := s.InstallationOrg(ctx, "cap-b"); err != nil || !ok || named != b {
		t.Fatalf("named: %v, %v, %v", named, ok, err)
	}
	if first, ok, err := s.InstallationOrg(ctx, "missing"); err != nil || !ok || first != a {
		t.Fatalf("without the named one, the first organization made: %v, %v, %v", first, ok, err)
	}
}

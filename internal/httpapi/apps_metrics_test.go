package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/usage"
)

// tests/http.rs m5_metrics_are_never_invented, over HTTP: the fixture runs
// no collector, so the live window says so instead of showing zeros; the
// week comes from the hourly rollups; any other window is refused. The live
// window with a collector is TestLiveMetricsAreNeverInvented.
func TestM5MetricsAreNeverInvented(t *testing.T) {
	f := newFixture(t)
	_, _, target := f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	const path = "/api/v1/projects/shop/environments/prod/apps/api/metrics"

	status, empty, _ := alice.do("GET", path, nil)
	if status != http.StatusOK {
		t.Fatalf("1h: %d %v", status, empty)
	}
	if empty["available"] != false || !cmp.Equal(empty["points"], []any{}) || empty["window"] != "1h" {
		t.Fatalf("1h: %v", empty)
	}
	if empty["reason"] != "usage is not collected on this server" {
		t.Errorf("1h reason: %v", empty["reason"])
	}

	week := path + "?window=7d"
	if _, none, _ := alice.do("GET", week, nil); none["available"] != false || none["reason"] != "no hourly usage recorded yet" {
		t.Fatalf("7d without rollups: %v", none)
	}
	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	hour := usage.HourOf(time.Now().UnixMilli()) - usage.HourMs
	r := store.UsageRollup{Hour: hour, CPUAvg: 100, CPUMax: 400, MemoryAvg: 1_000, MemoryMax: 2_000, Samples: 120}
	if err := tn.KeepUsage(ctx, target, r); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, rolled, _ := alice.do("GET", week, nil)
	want := map[string]any{
		"window": "7d", "available": true, "reason": nil,
		"points": []any{map[string]any{
			"at": api.Timestamp(hour), "cpuMillis": 100.0, "memoryBytes": 1_000.0,
			"cpuMax": 400.0, "memoryMax": 2_000.0, "pods": nil,
		}},
	}
	if diff := cmp.Diff(want, rolled); diff != "" {
		t.Errorf("7d (-want +got):\n%s", diff)
	}
	if status, body, _ := alice.do("GET", path+"?window=2d", nil); status != http.StatusUnprocessableEntity {
		t.Errorf("2d: %d %v", status, body)
	}
}

// The live half of m5_metrics_are_never_invented: a collector without
// samples reports why, never zeros; a sample shows as it was measured.
func TestLiveMetricsAreNeverInvented(t *testing.T) {
	var b usage.Buffer
	const now int64 = 1_757_894_400_000
	points, reason := api.LiveMetrics(opt.Some(&b), "kb-shop-prod", "api", now)
	if why, ok := reason.Get(); len(points) != 0 || !ok || !strings.Contains(why, "no samples") {
		t.Fatalf("empty: %v, %v", points, reason)
	}
	b.Record(usage.SeriesKey{Namespace: "kb-shop-prod", App: "api", Org: "o"},
		usage.Sample{At: now, CPUMillis: 250, MemoryBytes: 64 << 20, Pods: 2})
	points, reason = api.LiveMetrics(opt.Some(&b), "kb-shop-prod", "api", now)
	if reason.IsSome() || len(points) != 1 {
		t.Fatalf("live: %v, %v", points, reason)
	}
	data, err := json.Marshal(&points[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"at": "2025-09-15T00:00:00Z", "cpuMillis": 250.0, "memoryBytes": float64(64 << 20),
		"cpuMax": nil, "memoryMax": nil, "pods": 2.0,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("live point (-want +got):\n%s", diff)
	}
	if _, reason := api.LiveMetrics(opt.None[*usage.Buffer](), "kb-shop-prod", "api", now); reason.IsNone() {
		t.Error("no collector, no reason")
	}
}

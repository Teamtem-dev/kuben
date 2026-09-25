package usage_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/usage"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

func key(app string) usage.SeriesKey {
	return usage.SeriesKey{Namespace: "kb-shop-prod", App: app, Org: "o"}
}

func sample(at int64, cpu uint64) usage.Sample {
	return usage.Sample{At: at, CPUMillis: cpu, MemoryBytes: cpu * 1_000, Pods: 1}
}

// Ported from usage.rs: series_are_bounded.
func TestSeriesAreBounded(t *testing.T) {
	var b usage.Buffer
	for i := range int64(200) {
		b.Record(key("web"), sample(i, 1))
	}
	w := b.Window("kb-shop-prod", "web", 0)
	if len(w) != usage.SamplesPerSeries {
		t.Fatalf("len = %d, want %d", len(w), usage.SamplesPerSeries)
	}
	if w[0].At != 80 {
		t.Errorf("the oldest go first: first at = %d, want 80", w[0].At)
	}
	if got := b.Window("kb-shop-prod", "other", 0); len(got) != 0 {
		t.Errorf("nothing is not zero: %v", got)
	}
	b.Trim(usage.HourMs + 150)
	if got := len(b.Window("kb-shop-prod", "web", 0)); got != 50 {
		t.Errorf("after trim len = %d, want 50", got)
	}
	b.Trim(10 * usage.HourMs)
	if b.Len() != 0 {
		t.Errorf("len = %d, want empty", b.Len())
	}
}

// Ported from usage.rs: the_least_recent_series_makes_room.
func TestTheLeastRecentSeriesMakesRoom(t *testing.T) {
	var b usage.Buffer
	for i := range usage.MaxSeries {
		b.Record(key(fmt.Sprintf("app-%d", i)), sample(int64(i)+1, 1))
	}
	b.Record(key("newcomer"), sample(1_000_000, 1))
	if b.Len() != usage.MaxSeries {
		t.Fatalf("len = %d, want %d", b.Len(), usage.MaxSeries)
	}
	if got := b.Window("kb-shop-prod", "app-0", 0); len(got) != 0 {
		t.Errorf("app-0 kept: %v", got)
	}
	if got := b.Window("kb-shop-prod", "newcomer", 0); len(got) != 1 {
		t.Errorf("newcomer = %v, want one sample", got)
	}
}

// Ported from usage.rs: rollups_average_and_keep_peaks.
func TestRollupsAverageAndKeepPeaks(t *testing.T) {
	r, ok := usage.RollupOf(0, []usage.Sample{sample(10, 100), sample(20, 300), sample(30, 200)})
	want := usage.Rollup{Hour: 0, CPUAvg: 200, CPUMax: 300, MemoryAvg: 200_000, MemoryMax: 300_000, Samples: 3}
	if !ok {
		t.Fatal("no rollup")
	}
	if diff := cmp.Diff(want, r); diff != "" {
		t.Errorf("rollup (-want +got):\n%s", diff)
	}
	if _, ok := usage.RollupOf(0, nil); ok {
		t.Error("a rollup of nothing")
	}
	if got := usage.HourOf(usage.HourMs + 5); got != usage.HourMs {
		t.Errorf("HourOf = %d, want %d", got, usage.HourMs)
	}
	if got := usage.HourOf(-5); got != -usage.HourMs {
		t.Errorf("HourOf(-5) = %d, want %d (Euclidean)", got, -usage.HourMs)
	}
	var b usage.Buffer
	b.Record(key("web"), sample(usage.HourMs-1, 1))
	b.Record(key("web"), sample(usage.HourMs+1, 1))
	first := b.Between(0, usage.HourMs)
	if len(first) != 1 || len(first[0].Samples) != 1 {
		t.Errorf("first hour = %v, want one series with one sample", first)
	}
}

func podMetrics(name, namespace string, labels map[string]any, containers ...any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1",
		"kind":       "PodMetrics",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"containers": containers,
	}}
}

func pod(name, app, cpu, memory string) unstructured.Unstructured {
	return podMetrics(name, "kb-shop-prod",
		map[string]any{v1alpha1.LabelApp: app, v1alpha1.LabelOrg: "o", v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue},
		map[string]any{"name": "c", "usage": map[string]any{"cpu": cpu, "memory": memory}})
}

// Ported from usage.rs: pod_metrics_are_summed_per_app.
func TestPodMetricsAreSummedPerApp(t *testing.T) {
	items := []unstructured.Unstructured{
		pod("a", "web", "250000000n", "64Mi"),
		pod("b", "web", "150m", "36Mi"),
		pod("c", "worker", "2500u", "1Gi"),
		podMetrics("x", "kube-system", map[string]any{}),
	}
	got := usage.Summarize(items, 7)
	want := []usage.Measured{
		{Key: key("web"), Sample: usage.Sample{At: 7, CPUMillis: 400, MemoryBytes: 100 << 20, Pods: 2}},
		{Key: key("worker"), Sample: usage.Sample{At: 7, CPUMillis: 2, MemoryBytes: 1 << 30, Pods: 1}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Summarize (-want +got):\n%s", diff)
	}
}

// steppedClock answers the times it was given, one per call, then the last.
type steppedClock struct {
	mu    sync.Mutex
	times []int64
}

func (c *steppedClock) NowMs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return now
}

// Beyond the Rust tests: one pass of the collector reads the Metrics API
// through the dynamic client, fills the buffer and rolls up the hour that
// ended; a cluster without the API makes usage unavailable, not failed.
func TestRunScrapesAndRollsUp(t *testing.T) {
	scheme := runtime.NewScheme()
	lists := map[schema.GroupVersionResource]string{usage.PodMetrics(): "PodMetricsList"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists)
	web := pod("a", "web", "100m", "1Mi")
	if _, err := client.Resource(usage.PodMetrics()).Namespace("kb-shop-prod").Create(t.Context(), &web, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var (
		b    usage.Buffer
		kept []usage.Rollup
	)
	h := health.New(clock.Fixed(0))
	// Start in the first hour, scrape in it, then find the second hour begun.
	c := &steppedClock{times: []int64{usage.HourMs - 10, usage.HourMs - 5, usage.HourMs + 5}}
	err := usage.Run(ctx, usage.Deps{
		Cluster: client, Buffer: &b, Health: h, Clock: c,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keep: func(_ context.Context, k usage.SeriesKey, r usage.Rollup) error {
			if k != key("web") {
				t.Errorf("key = %v", k)
			}
			kept = append(kept, r)
			cancel()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []usage.Rollup{{Hour: 0, CPUAvg: 100, CPUMax: 100, MemoryAvg: 1 << 20, MemoryMax: 1 << 20, Samples: 1}}
	if diff := cmp.Diff(want, kept); diff != "" {
		t.Errorf("rollups (-want +got):\n%s", diff)
	}
	if _, ok := b.Unavailable().Get(); ok {
		t.Error("usage unavailable after a good scrape")
	}

	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := usage.Run(ctx, usage.Deps{
		Cluster: client, Buffer: &b, Health: h, Clock: clock.Fixed(usage.HourMs + 6),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keep:   func(context.Context, usage.SeriesKey, usage.Rollup) error { return nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if why, ok := b.Unavailable().Get(); !ok || why == "" {
		t.Errorf("unavailable = %q, %v; want a reason", why, ok)
	}
}

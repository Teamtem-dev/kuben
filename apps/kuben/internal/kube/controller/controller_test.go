package controller_test

import (
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// The tests of controller::mod, duration and crd_apply.

func TestBackoffGrowsAndCaps(t *testing.T) {
	for failures, want := range map[uint32]time.Duration{
		0: 2 * time.Second, 1: 2 * time.Second, 3: 8 * time.Second, 30: 5 * time.Minute,
		8: 256 * time.Second, 9: 5 * time.Minute, 1 << 31: 5 * time.Minute,
	} {
		if got := controller.Backoff(failures); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", failures, got, want)
		}
	}
}

func TestTheRateLimiterWaitsTheBackoff(t *testing.T) {
	limiter := controller.RateLimiter()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}}
	other := reconcile.Request{NamespacedName: types.NamespacedName{Name: "b"}}
	for n := uint32(1); n <= 12; n++ {
		if got := limiter.When(req); got != controller.Backoff(n) {
			t.Fatalf("failure %d waits %s, want %s", n, got, controller.Backoff(n))
		}
	}
	if got := limiter.When(other); got != 2*time.Second {
		t.Fatalf("another object backs off on its own: %s", got)
	}
	limiter.Forget(req)
	if got := limiter.When(req); got != 2*time.Second {
		t.Fatalf("a success resets the backoff: %s", got)
	}
}

func TestConditionKeepsTransitionTimeWhileUnchanged(t *testing.T) {
	first := controller.Condition(clock.Fixed(1_780_000_000_000), nil, "Ready", true, "Available", "", opt.Some[int64](1))
	if first.LastTransitionTime == nil || *first.LastTransitionTime != "2026-05-28T20:26:40Z" {
		t.Fatalf("transition time: %v", first.LastTransitionTime)
	}
	if first.Message != nil || first.Reason == nil || *first.Reason != "Available" {
		t.Fatalf("first: %+v", first)
	}
	later := clock.Fixed(1_790_000_000_000)
	again := controller.Condition(later, []v1alpha1.Condition{first}, "Ready", true, "Available", "", opt.Some[int64](2))
	if *again.LastTransitionTime != *first.LastTransitionTime {
		t.Fatalf("kept: %s, was %s", *again.LastTransitionTime, *first.LastTransitionTime)
	}
	if again.ObservedGeneration == nil || *again.ObservedGeneration != 2 {
		t.Fatalf("generation: %v", again.ObservedGeneration)
	}
	flipped := controller.Condition(later, []v1alpha1.Condition{first}, "Ready", false, "Progressing", "0/1 available", opt.Some[int64](2))
	if flipped.Status != "False" || flipped.Message == nil || *flipped.Message != "0/1 available" {
		t.Fatalf("flipped: %+v", flipped)
	}
	if *flipped.LastTransitionTime == *first.LastTransitionTime {
		t.Fatal("a flip is a transition")
	}
	none := controller.Condition(later, nil, "Ready", true, "x", "", opt.None[int64]())
	if none.ObservedGeneration != nil {
		t.Fatal("no generation, no observedGeneration")
	}
}

func kubenConfig(name, baseDomain string) v1alpha1.KubenConfig {
	c := v1alpha1.KubenConfig{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c.Spec.BaseDomain = &baseDomain
	return c
}

func TestTheSingletonIsTheConfigNamedKubenElseTheFirst(t *testing.T) {
	if _, ok := controller.ChosenConfig(nil); ok {
		t.Fatal("no config, none chosen")
	}
	a, k := kubenConfig("a", "a.example.com"), kubenConfig("kuben", "k.example.com")
	if c, _ := controller.ChosenConfig([]v1alpha1.KubenConfig{a, k}); c.Name != "kuben" {
		t.Fatalf("chose %s", c.Name)
	}
	if c, _ := controller.ChosenConfig([]v1alpha1.KubenConfig{a, kubenConfig("b", "")}); c.Name != "a" {
		t.Fatalf("chose %s", c.Name)
	}
	if got := controller.PlatformOf([]v1alpha1.KubenConfig{a, k}).BaseDomain; got != opt.Some("k.example.com") {
		t.Fatalf("base domain %v", got)
	}
	if diff := cmp.Diff(render.DefaultPlatform(), controller.PlatformOf(nil), cmp.AllowUnexported(opt.Val[string]{},
		opt.Val[render.GatewayRef]{})); diff != "" {
		t.Fatalf("defaults (-want +got):\n%s", diff)
	}
}

func TestParsesUnitsAndCompounds(t *testing.T) {
	for in, want := range map[string]uint64{
		"90s": 90, "15m": 15 * 60, "168h": 168 * 3600, "14d": 14 * 86_400, "1h30m": 90 * 60, "0s": 0, " 2m ": 120, //nolint:gocritic // spaces are trimmed
	} {
		if got, ok := controller.ParseDuration(in); !ok || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, ok, want)
		}
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "  ", "10", "h", "1x", "-1h", "1.5h", "99999999999999999999d", "213503982334602d"} {
		if got, ok := controller.ParseDuration(bad); ok {
			t.Errorf("%q parsed as %d", bad, got)
		}
	}
}

func TestTheEmbeddedManifestIsTheChartsManifest(t *testing.T) {
	chart, err := os.ReadFile("../../../../../charts/kuben/crds/kuben.dev_all.yaml")
	if os.IsNotExist(err) {
		t.Skip("the chart's manifest is gone; the embedded copy is the source now")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(chart) != string(controller.CRDManifest()) {
		t.Fatal("kuben.dev_all.yaml differs from charts/kuben/crds/kuben.dev_all.yaml: copy it again")
	}
}

func TestRunAllRefusesIncompleteDependencies(t *testing.T) {
	if err := controller.RunAll(t.Context(), controller.Deps{}); err == nil {
		t.Fatal("no cluster, no controllers")
	}
}

package envtest_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/internal/kube/envtest"
)

// Two writers against a real API server (ADR-027, the M0 spike of
// crates/kuben-platform/tests/two_writer_cas.rs): what the materializer's
// and the agent's fencing relies on.

var apiserver envtest.Server

func TestMain(m *testing.M) { os.Exit(envtest.Main(m, &apiserver)) }

const genKey = "generation"

func cm(generation uint64) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "target"},
		Data:       map[string]string{genKey: strconv.FormatUint(generation, 10)},
	}
}

func generationOf(c *corev1.ConfigMap) uint64 {
	g, err := strconv.ParseUint(c.Data[genKey], 10, 64)
	if err != nil {
		return 0
	}
	return g
}

func configMaps(t *testing.T) typedcorev1.ConfigMapInterface {
	t.Helper()
	c := apiserver.Connect(t)
	return c.Typed.CoreV1().ConfigMaps(envtest.Namespace(t, c, "kuben-m0-cas"))
}

func live(t *testing.T, api typedcorev1.ConfigMapInterface) *corev1.ConfigMap {
	t.Helper()
	got, err := api.Get(t.Context(), "target", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// writeIfNewer is the agent's write rule: write generation only if the
// live object is older, retrying conflicts with a fresh read.
func writeIfNewer(t *testing.T, api typedcorev1.ConfigMapInterface, generation uint64) (bool, error) {
	for range 20 {
		current, err := api.Get(t.Context(), "target", metav1.GetOptions{})
		if err != nil {
			return false, err //nolint:wrapcheck // a test helper
		}
		if generationOf(current) >= generation {
			return false, nil
		}
		next := current.DeepCopy()
		next.Data = cm(generation).Data
		_, err = api.Update(t.Context(), next, metav1.UpdateOptions{})
		switch {
		case err == nil:
			return true, nil
		case !apierrors.IsConflict(err):
			return false, err //nolint:wrapcheck // a test helper
		}
	}
	t.Error("no progress after 20 conflicts")
	return false, nil
}

func TestStaleResourceVersionReplaceIsRejected(t *testing.T) {
	api := configMaps(t)
	if _, err := api.Create(t.Context(), cm(1), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	seenByA, seenByB := live(t, api), live(t, api)
	b := seenByB.DeepCopy()
	b.Data = cm(2).Data
	if _, err := api.Update(t.Context(), b, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("b writes first: %v", err)
	}
	a := seenByA.DeepCopy()
	a.Data = cm(1).Data
	if _, err := api.Update(t.Context(), a, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("a's stale write must fail with 409, got %v", err)
	}
	if g := generationOf(live(t, api)); g != 2 {
		t.Fatalf("generation %d", g)
	}
}

func TestServerSideApplyWithAStaleResourceVersionIsRejected(t *testing.T) {
	api := configMaps(t)
	apply := func(c *corev1.ConfigMap) (*corev1.ConfigMap, error) {
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		force := true
		return api.Patch(t.Context(), "target", types.ApplyPatchType, body, //nolint:wrapcheck // compared by the test
			metav1.PatchOptions{FieldManager: "kuben-m0", Force: &force})
	}
	first, err := apply(cm(1))
	if err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if _, err := apply(cm(2)); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	stale := cm(1)
	stale.ResourceVersion = first.ResourceVersion
	if _, err := apply(stale); !apierrors.IsConflict(err) {
		t.Fatalf("SSA with a stale resourceVersion must not overwrite (409), got %v", err)
	}
	if g := generationOf(live(t, api)); g != 2 {
		t.Fatalf("generation %d", g)
	}
}

func TestRacingWritersKeepTheHighestGeneration(t *testing.T) {
	api := configMaps(t)
	if _, err := api.Create(t.Context(), cm(1), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for round := range uint64(10) {
		base := 10 * (round + 1)
		var wroteNew bool
		var g errgroup.Group
		// An old writer (base+1) and a new writer (base+2) race.
		g.Go(func() error { _, err := writeIfNewer(t, api, base+1); return err })
		g.Go(func() error {
			wrote, err := writeIfNewer(t, api, base+2)
			wroteNew = wrote
			return err
		})
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		if got := generationOf(live(t, api)); got != base+2 || (!wroteNew && got != base+2) {
			t.Fatalf("round %d: a lower generation won (%d)", round, got)
		}
	}
}

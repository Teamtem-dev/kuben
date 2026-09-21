package v1alpha1_test

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Ports the tests of crates/kuben-crd/src/v1alpha1/runtime.rs.

func hash(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func runtimeObject(generation int64, input string, epoch int64) *v1alpha1.ApplicationRuntime {
	return &v1alpha1.ApplicationRuntime{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kuben.dev/v1alpha1", Kind: v1alpha1.ApplicationRuntimeKind},
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.ApplicationRuntimeSpec{
			TargetID:     "0199a0c0-0000-7000-8000-000000000001",
			LifecycleUID: "0199a0c0-0000-7000-8000-000000000002",
			ControlEpoch: epoch,
			Generation:   generation,
			InputHash:    hash(input),
			ReleaseID:    "0199a0c0-0000-7000-8000-000000000003",
			Plan: v1alpha1.PlanEnvelope{
				ID:              "0199a0c0-0000-7000-8000-000000000004",
				RendererVersion: "kuben-renderer/1",
				Digest:          hash("e"),
				Resources:       "[]",
			},
		},
	}
}

func TestANewEnvelopeIsValid(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	api.accepts("new", api.create(runtimeObject(1, "a", 0)))
	api.refuses("generation 0", api.create(runtimeObject(0, "a", 0)))
	api.refuses("negative epoch", api.create(runtimeObject(1, "a", -1)))
	bad := runtimeObject(1, "a", 0)
	bad.Spec.InputHash = "sha256:XYZ"
	api.refuses("input hash pattern", api.create(bad))
}

func TestALowerGenerationIsRejected(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	old := runtimeObject(5, "a", 0)
	api.refuses("lower generation", api.update(runtimeObject(4, "b", 0), old))
	api.accepts("a higher generation", api.update(runtimeObject(6, "b", 0), old))
}

func TestTheSameGenerationCarriesTheSameEnvelope(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	old := runtimeObject(5, "a", 0)
	api.accepts("idempotent", api.update(runtimeObject(5, "a", 0), old))
	api.refuses("another hash", api.update(runtimeObject(5, "b", 0), old))
	plan := runtimeObject(5, "a", 0)
	plan.Spec.Plan.Resources = `[{"kind":"Deployment"}]`
	api.refuses("another plan", api.update(plan, old))
}

func TestTheControlEpochNeverDecreases(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	old := runtimeObject(5, "a", 3)
	api.refuses("lower epoch", api.update(runtimeObject(6, "b", 2), old))
	api.accepts("a new epoch, the same envelope", api.update(runtimeObject(5, "a", 4), old))
}

func TestTheIdentityNeverChanges(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	old := runtimeObject(5, "a", 0)
	other := runtimeObject(6, "b", 0)
	other.Spec.TargetID = "0199a0c0-0000-7000-8000-00000000000f"
	api.refuses("target changed", api.update(other, old))
	recreated := runtimeObject(6, "b", 0)
	recreated.Spec.LifecycleUID = "0199a0c0-0000-7000-8000-00000000000f"
	api.refuses("lifecycle changed", api.update(recreated, old))
}

func TestTheObservedGenerationNeverDecreases(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ApplicationRuntimeKind)
	five, four := int64(5), int64(4)
	old := runtimeObject(5, "a", 0)
	old.Status = &v1alpha1.ApplicationRuntimeStatus{ObservedGeneration: &five}
	updated := runtimeObject(5, "a", 0)
	updated.Status = &v1alpha1.ApplicationRuntimeStatus{ObservedGeneration: &four}
	api.refuses("observed generation lowered", api.update(updated, old))
}

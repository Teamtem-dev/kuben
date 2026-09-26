package controller_test

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// now is the tests' clock: 2026-09-22T12:00:00Z.
const now = clock.Fixed(1_790_078_400_000)

// cluster is a fake cluster and the reconcilers over it.
type cluster struct {
	client.WithWatch
	rec         controller.Reconcilers
	facts       *discovery.Watch
	projections *projection.Projections
}

func newCluster(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) cluster {
	t.Helper()
	scheme, err := controller.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	if funcs.Apply == nil {
		funcs.Apply = applyWithoutSSAForNetworkPolicies
	}
	c := interceptor.NewClient(fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Project{}, &v1alpha1.Environment{}, &v1alpha1.App{}, &v1alpha1.KubenConfig{}).
		WithObjects(objects...).
		Build(), funcs)
	facts := &discovery.Watch{}
	projections := projection.New()
	return cluster{c, controller.NewReconcilers(c, facts, now, projections), facts, projections}
}

// applyWithoutSSAForNetworkPolicies works around the fake client, whose
// server-side apply fails for every NetworkPolicy ("expected objects with
// types from the same schema"): those are created or replaced instead.
func applyWithoutSSAForNetworkPolicies(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration,
	opts ...client.ApplyOption,
) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return err
	}
	if u.GetKind() != "NetworkPolicy" {
		return c.Apply(ctx, obj, opts...)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(u.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), live); err != nil {
		return c.Create(ctx, u)
	}
	u.SetResourceVersion(live.GetResourceVersion())
	return c.Update(ctx, u)
}

// decode reads JSON into a new T.
func decode[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func reconcileOnce(t *testing.T, r reconcile.Reconciler, namespace, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", namespace, name, err)
	}
	return res
}

// get reads an object of kind gvk; false when there is none.
func get(t *testing.T, c client.Client, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, u)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, namespace, name, err)
	}
	return u, err == nil
}

func mustGet(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("get %s: %v", obj.GetName(), err)
	}
}

// ready is the condition of type conditionType; false when there is none.
func conditionOf(conditions []v1alpha1.Condition, conditionType string) (v1alpha1.Condition, bool) {
	for _, c := range conditions {
		if c.Type == conditionType {
			return c, true
		}
	}
	return v1alpha1.Condition{}, false
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

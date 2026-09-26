package controller_test

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// The Rust project and environment controllers had no tests; these run
// them against the fake client.

func env(t *testing.T, name, spec string) *v1alpha1.Environment {
	t.Helper()
	e := &v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 3}}
	e.Spec = decode[v1alpha1.EnvironmentSpec](t, spec)
	return e
}

func TestAProjectCountsItsLiveEnvironments(t *testing.T) {
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop", Generation: 2}}
	c := newCluster(t, interceptor.Funcs{},
		project,
		env(t, "shop-prod", `{"project":"shop"}`),
		env(t, "shop-dev", `{"project":"shop"}`),
		env(t, "blog-prod", `{"project":"blog"}`),
	)
	if res := reconcileOnce(t, c.rec.Project, "", "shop"); res.RequeueAfter != 15*time.Minute {
		t.Fatalf("requeue %s", res.RequeueAfter)
	}
	mustGet(t, c, project)
	st := project.Status
	if st == nil || st.Environments != 2 || st.ObservedGeneration == nil || *st.ObservedGeneration != 2 {
		t.Fatalf("status: %+v", st)
	}
	ready, ok := conditionOf(st.Conditions, v1alpha1.ConditionReady)
	if !ok || ready.Status != "True" || str(ready.Reason) != "Reconciled" || ready.Message != nil ||
		str(ready.LastTransitionTime) != "2026-09-22T12:00:00Z" {
		t.Fatalf("ready: %+v", ready)
	}
	// A missing project is no error.
	reconcileOnce(t, c.rec.Project, "", "gone")
}

func namespaceKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
}

func TestAnEnvironmentGetsItsNamespaceAndGuardRails(t *testing.T) {
	e := env(t, "shop-prod", `{"project":"shop","type":"production","quota":{"pods":5}}`)
	c := newCluster(t, interceptor.Funcs{}, e)
	if res := reconcileOnce(t, c.rec.Environment, "", "shop-prod"); res.RequeueAfter != 10*time.Minute {
		t.Fatalf("requeue %s", res.RequeueAfter)
	}
	ns, ok := get(t, c, namespaceKind(), "", "kb-shop-prod")
	if !ok || ns.GetLabels()[v1alpha1.LabelEnvironment] != "shop-prod" || ns.GetLabels()[render.EnvTypeLabel] != "production" {
		t.Fatalf("namespace: %v", ns)
	}
	for _, o := range []struct{ kind, group, name string }{
		{"ResourceQuota", "", render.QuotaName},
		{"LimitRange", "", render.LimitsName},
		{"NetworkPolicy", "networking.k8s.io", render.NetpolName},
	} {
		if _, ok := get(t, c, schema.GroupVersionKind{Group: o.group, Version: "v1", Kind: o.kind}, "kb-shop-prod", o.name); !ok {
			t.Fatalf("no %s", o.kind)
		}
	}
	mustGet(t, c, e)
	if !slices.Contains(e.Finalizers, render.EnvFinalizer) {
		t.Fatalf("finalizers: %v", e.Finalizers)
	}
	st := e.Status
	if st == nil || str(st.Phase) != "Ready" || str(st.Namespace) != "kb-shop-prod" || st.DeletionScheduledAt != nil {
		t.Fatalf("status: %+v", st)
	}
	if ready, _ := conditionOf(st.Conditions, v1alpha1.ConditionReady); ready.Status != "True" || str(ready.Reason) != "Provisioned" {
		t.Fatalf("ready: %+v", ready)
	}
}

func TestAForeignNamespaceIsNeverAdopted(t *testing.T) {
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kb-shop-prod", Labels: map[string]string{"team": "x"}}}
	c := newCluster(t, interceptor.Funcs{}, env(t, "shop-prod", `{"project":"shop"}`), foreign)
	if res := reconcileOnce(t, c.rec.Environment, "", "shop-prod"); res.RequeueAfter != 5*time.Minute {
		t.Fatalf("requeue %s", res.RequeueAfter)
	}
	e := &v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}
	mustGet(t, c, e)
	ready, _ := conditionOf(e.Status.Conditions, v1alpha1.ConditionReady)
	if str(e.Status.Phase) != "Degraded" || str(ready.Reason) != "NamespaceConflict" ||
		str(ready.Message) != "namespace kb-shop-prod already exists and is not managed by this environment" {
		t.Fatalf("status: %+v %+v", e.Status, ready)
	}
	mustGet(t, c, foreign)
	if _, managed := foreign.Labels[v1alpha1.LabelManagedBy]; managed {
		t.Fatal("the foreign namespace was written")
	}
}

// deleting marks e deleted at at, with the finalizer (the fake client
// keeps such an object only while it has one).
func deleting(e *v1alpha1.Environment, at time.Time) *v1alpha1.Environment {
	e.Finalizers = []string{render.EnvFinalizer}
	ts := metav1.NewTime(at)
	e.DeletionTimestamp = &ts
	return e
}

func managedNamespace(env string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kb-" + env, Labels: map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelEnvironment: env, "keep": "me",
	}}}
}

func TestRetainHandsTheNamespaceBack(t *testing.T) {
	e := deleting(env(t, "shop-dev", `{"project":"shop"}`), time.UnixMilli(int64(now)))
	c := newCluster(t, interceptor.Funcs{}, e, managedNamespace("shop-dev"))
	reconcileOnce(t, c.rec.Environment, "", "shop-dev")
	ns, ok := get(t, c, namespaceKind(), "", "kb-shop-dev")
	if !ok {
		t.Fatal("the namespace is kept")
	}
	want := map[string]string{"keep": "me"}
	if got := ns.GetLabels(); len(got) != 1 || got["keep"] != want["keep"] {
		t.Fatalf("labels: %v", got)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "shop-dev"}, &v1alpha1.Environment{}); err == nil {
		t.Fatal("the finalizer is removed and the environment goes")
	}
}

func TestDeleteWaitsForTheGracePeriodThenPurges(t *testing.T) {
	deleted := time.UnixMilli(int64(now)).Add(-30 * time.Minute)
	e := deleting(env(t, "shop-prod", `{"project":"shop","deletionPolicy":"Delete","protection":{"deletionGrace":"2h"}}`), deleted)
	c := newCluster(t, interceptor.Funcs{}, e, managedNamespace("shop-prod"))
	if res := reconcileOnce(t, c.rec.Environment, "", "shop-prod"); res.RequeueAfter != time.Hour {
		t.Fatalf("requeue %s: at most an hour", res.RequeueAfter)
	}
	mustGet(t, c, e)
	ready, _ := conditionOf(e.Status.Conditions, v1alpha1.ConditionReady)
	if str(e.Status.Phase) != "Terminating" || str(e.Status.DeletionScheduledAt) != "2026-09-22T13:30:00Z" ||
		str(ready.Reason) != "DeletionScheduled" || str(ready.Message) != "the namespace is deleted when the grace period ends" {
		t.Fatalf("status: %+v %+v", e.Status, ready)
	}
	if _, ok := get(t, c, namespaceKind(), "", "kb-shop-prod"); !ok {
		t.Fatal("deleted before the grace period ended")
	}

	e = deleting(env(t, "shop-prod", `{"project":"shop","deletionPolicy":"Delete"}`), deleted)
	c = newCluster(t, interceptor.Funcs{}, e, managedNamespace("shop-prod"))
	if res := reconcileOnce(t, c.rec.Environment, "", "shop-prod"); res.RequeueAfter != 5*time.Second {
		t.Fatalf("requeue %s", res.RequeueAfter)
	}
	if _, ok := get(t, c, namespaceKind(), "", "kb-shop-prod"); ok {
		t.Fatal("no grace: the namespace is deleted")
	}
	mustGet(t, c, e)
	reconcileOnce(t, c.rec.Environment, "", "shop-prod")
	if err := c.Get(context.Background(), client.ObjectKey{Name: "shop-prod"}, &v1alpha1.Environment{}); err == nil {
		t.Fatal("once the namespace is gone, the finalizer is removed")
	}
}

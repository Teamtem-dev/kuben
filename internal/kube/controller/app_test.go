package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/kube/controller"
)

// The test of controller::app, and the App reconciler against the fake
// client.

func appWith(t *testing.T, annotations map[string]string, deleting bool, spec string) *v1alpha1.App {
	t.Helper()
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{
		Name: "web", Namespace: "kb-shop-prod", Annotations: annotations, UID: "uid-1", Generation: 4,
		Labels: map[string]string{v1alpha1.LabelProject: "shop", v1alpha1.LabelEnvironment: "shop-prod"},
	}}
	if deleting {
		ts := metav1.NewTime(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))
		a.DeletionTimestamp = &ts
		a.Finalizers = []string{"test/keep"}
	}
	a.Spec = decode[v1alpha1.AppSpec](t, spec)
	return a
}

const nginx = `{ "source": { "image": "nginx:1.27" }, "runtime": { "processes": { "web": { "port": 80 } } } }`

func TestADeletingOrHandedOverAppIsLeftAlone(t *testing.T) {
	if controller.HandsOff(appWith(t, map[string]string{}, false, nginx)) {
		t.Fatal("an ordinary app is reconciled")
	}
	if !controller.HandsOff(appWith(t, map[string]string{}, true, nginx)) {
		t.Fatal("being deleted")
	}
	if !controller.HandsOff(appWith(t, map[string]string{v1alpha1.AnnotationHandover: "t"}, false, nginx)) {
		t.Fatal("handed over to the agent")
	}
}

func gatewayConfig(t *testing.T) *v1alpha1.KubenConfig {
	t.Helper()
	c := &v1alpha1.KubenConfig{ObjectMeta: metav1.ObjectMeta{Name: "kuben", Generation: 1}}
	c.Spec = decode[v1alpha1.KubenConfigSpec](t, `{ "baseDomain": "apps.example.com", "gatewayClassName": "traefik" }`)
	return c
}

func kind(group, version, k string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: k}
}

func TestAnAppIsAppliedPrunedAndReported(t *testing.T) {
	app := appWith(t, nil, false, `{
		"source": { "image": "ghcr.io/acme/web:1" },
		"runtime": { "processes": {
			"web": { "port": 3000, "replicas": { "min": 2, "max": 2 } },
			"worker": { "command": ["bin/worker"] }
		} }
	}`)
	stale := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "web-old", Namespace: "kb-shop-prod", Labels: map[string]string{v1alpha1.LabelApp: "web"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "kuben.dev/v1alpha1", Kind: "App", Name: "web", UID: "uid-1"}},
	}}
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "web-theirs", Namespace: "kb-shop-prod", Labels: map[string]string{v1alpha1.LabelApp: "web"},
	}}
	c := newCluster(t, interceptor.Funcs{}, app, gatewayConfig(t), stale, foreign)
	if res := reconcileOnce(t, c.rec.App, "kb-shop-prod", "web"); res.RequeueAfter != 30*time.Second {
		t.Fatalf("not rolled out yet: requeue %s", res.RequeueAfter)
	}
	deploy, ok := get(t, c, kind("apps", "v1", "Deployment"), "kb-shop-prod", "web-web")
	if !ok {
		t.Fatal("no web deployment")
	}
	refs, _, _ := unstructured.NestedSlice(deploy.Object, "metadata", "ownerReferences")
	if len(refs) != 1 {
		t.Fatalf("owner references: %v", refs)
	}
	ref, _ := refs[0].(map[string]any)
	if _, has := ref["blockOwnerDeletion"]; has || ref["controller"] != true || ref["uid"] != "uid-1" {
		t.Fatalf("owner reference as kube-rs made it: %v", ref)
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
		want bool
	}{
		{kind("apps", "v1", "Deployment"), "web-worker", true},
		{kind("apps", "v1", "Deployment"), "web-old", false},
		{kind("apps", "v1", "Deployment"), "web-theirs", true},
		{kind("", "v1", "Service"), "web", true},
		{kind("gateway.networking.k8s.io", "v1", "HTTPRoute"), "web", true},
	} {
		if _, ok := get(t, c, o.gvk, "kb-shop-prod", o.name); ok != o.want {
			t.Errorf("%s %s exists: %v, want %v", o.gvk.Kind, o.name, ok, o.want)
		}
	}
	mustGet(t, c, app)
	st := app.Status
	if st == nil || str(st.URL) != "http://web-shop-prod.apps.example.com" || *st.ObservedGeneration != 4 {
		t.Fatalf("status: %+v", st)
	}
	ready, _ := conditionOf(st.Conditions, v1alpha1.ConditionReady)
	if ready.Status != "False" || str(ready.Reason) != "Progressing" || !strings.Contains(str(ready.Message), "web-web: ") {
		t.Fatalf("ready: %+v", ready)
	}
	exposed, _ := conditionOf(st.Conditions, controller.Exposed)
	if exposed.Status != "True" || str(exposed.Reason) != "RouteApplied" || exposed.Message != nil {
		t.Fatalf("exposed: %+v", exposed)
	}
}

func TestARolledOutAppIsReadyAndWithoutGatewayAPINotExposed(t *testing.T) {
	app := appWith(t, nil, false, nginx)
	one := int32(1)
	live := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web-web", Namespace: "kb-shop-prod", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	noGatewayAPI := interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		u, ok := obj.(interface{ GetObjectKind() schema.ObjectKind })
		if ok && u.GetObjectKind().GroupVersionKind().Group == "gateway.networking.k8s.io" {
			return &meta.NoKindMatchError{GroupKind: u.GetObjectKind().GroupVersionKind().GroupKind()}
		}
		if ok && u.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			return nil // keep the live rollout status
		}
		return c.Apply(ctx, obj, opts...)
	}}
	c := newCluster(t, noGatewayAPI, app, gatewayConfig(t), live)
	if res := reconcileOnce(t, c.rec.App, "kb-shop-prod", "web"); res.RequeueAfter != 10*time.Minute {
		t.Fatalf("rolled out: requeue %s", res.RequeueAfter)
	}
	mustGet(t, c, app)
	ready, _ := conditionOf(app.Status.Conditions, v1alpha1.ConditionReady)
	exposed, _ := conditionOf(app.Status.Conditions, controller.Exposed)
	if ready.Status != "True" || str(ready.Reason) != "Available" || app.Status.URL != nil {
		t.Fatalf("ready: %+v, url %s", ready, str(app.Status.URL))
	}
	if str(exposed.Reason) != "GatewayAPIMissing" || str(exposed.Message) != "the Gateway API CRDs are not installed" {
		t.Fatalf("exposed: %+v", exposed)
	}
}

func TestASpecProblemIsReportedAndNothingIsApplied(t *testing.T) {
	app := appWith(t, nil, false, `{ "source": { "image": "x" }, "runtime": { "processes": { "web": { "port": 80, "size": "huge" } } } }`)
	c := newCluster(t, interceptor.Funcs{}, app)
	if res := reconcileOnce(t, c.rec.App, "kb-shop-prod", "web"); res.RequeueAfter != 0 {
		t.Fatalf("waits for a change: %s", res.RequeueAfter)
	}
	mustGet(t, c, app)
	ready, _ := conditionOf(app.Status.Conditions, v1alpha1.ConditionReady)
	if ready.Status != "False" || str(ready.Reason) != "UnknownSize" || str(ready.Message) != "process `web` uses unknown size `huge`" {
		t.Fatalf("ready: %+v", ready)
	}
	if _, ok := get(t, c, kind("apps", "v1", "Deployment"), "kb-shop-prod", "web-web"); ok {
		t.Fatal("nothing is applied")
	}
}

func TestAHandedOverAppIsNotWritten(t *testing.T) {
	app := appWith(t, map[string]string{v1alpha1.AnnotationHandover: "t-1"}, false, nginx)
	c := newCluster(t, interceptor.Funcs{}, app)
	reconcileOnce(t, c.rec.App, "kb-shop-prod", "web")
	mustGet(t, c, app)
	if app.Status != nil {
		t.Fatalf("status written: %+v", app.Status)
	}
	if _, ok := get(t, c, kind("apps", "v1", "Deployment"), "kb-shop-prod", "web-web"); ok {
		t.Fatal("workloads written")
	}
}

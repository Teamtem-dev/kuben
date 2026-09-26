package projection_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fastTiming() projection.Timing {
	return projection.Timing{FirstSyncDeadline: 10 * time.Second, OptionalCheck: 20 * time.Millisecond, Poll: 5 * time.Millisecond}
}

var (
	projectsGVR = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ProjectResource)
	envsGVR     = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.EnvironmentResource)
	appsGVR     = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.AppResource)
	routesGVR   = schema.GroupVersionResource{Group: projection.GatewayGroup, Version: "v1", Resource: "httproutes"}
	certsGVR    = schema.GroupVersionResource{Group: projection.CertManagerGroup, Version: "v1", Resource: "certificates"}
)

func object(apiVersion, kind, namespace, name string, labels map[string]string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(labels)
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	return u
}

func managedPod(name string, managed bool) *corev1.Pod {
	labels := map[string]string{v1alpha1.LabelApp: "api", v1alpha1.LabelOrg: "o"}
	if managed {
		labels[v1alpha1.LabelManagedBy] = v1alpha1.LabelManagerValue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "kb-shop-prod", Labels: labels,
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
	}}
}

type cluster struct {
	typed   *fake.Clientset
	dynamic *dynamicfake.FakeDynamicClient
}

func newCluster(objects ...runtime.Object) cluster {
	typed := fake.NewClientset(managedPod("api-web-1", true), managedPod("other-1", false))
	lists := map[schema.GroupVersionResource]string{
		projectsGVR: "ProjectList", envsGVR: "EnvironmentList", appsGVR: "AppList",
		routesGVR: "HTTPRouteList", certsGVR: "CertificateList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)
	return cluster{typed: typed, dynamic: dyn}
}

func (c cluster) registry() registry.Cluster {
	return registry.Cluster{ID: registry.Primary, Typed: c.typed, Dynamic: c.dynamic}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func start(t *testing.T, c cluster, p *projection.Projections, timing projection.Timing) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- projection.RunWith(ctx, c.registry(), p, quiet(), timing) }()
	return cancel, done
}

func stop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a shutdown is not an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the informers did not stop")
	}
}

func TestTheInformersFillTheProjections(t *testing.T) {
	app := object("kuben.dev/v1alpha1", "App", "kb-shop-prod", "api", map[string]string{v1alpha1.LabelOrg: "o"},
		map[string]any{"spec": map[string]any{
			"source":  map[string]any{"image": "nginx:1.27"},
			"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": int64(80)}}},
		}})
	proj := object("kuben.dev/v1alpha1", "Project", "", "shop", nil,
		map[string]any{"spec": map[string]any{"displayName": "Shop"}})
	env := object("kuben.dev/v1alpha1", "Environment", "", "shop-prod", nil,
		map[string]any{"spec": map[string]any{"project": "shop", "type": "production"}})
	c := newCluster(app, proj, env)
	p := projection.New()
	cancel, done := start(t, c, p, fastTiming())
	defer stop(t, cancel, done)

	select {
	case <-p.Synced():
	case <-time.After(10 * time.Second):
		t.Fatalf("not synced; pending %v", p.PendingKinds())
	}
	if p.PodCount() != 1 {
		t.Fatalf("only managed pods: %d", p.PodCount())
	}
	if _, ok := p.Pod("kb-shop-prod/api-web-1"); !ok {
		t.Fatal("the managed pod")
	}
	if v, ok := p.App("kb-shop-prod", "api"); !ok || v.Processes[0].Size != "small" {
		t.Fatalf("app with the serde defaults: %+v", v)
	}
	if v, ok := p.Project("shop"); !ok || v.DisplayName != "Shop" {
		t.Fatalf("project: %+v", v)
	}
	if v, ok := p.Environment("shop-prod"); !ok || v.EnvType != "production" || v.Namespace != "kb-shop-prod" {
		t.Fatalf("environment: %+v", v)
	}

	// Watch events after the first LIST are upserts and deletes.
	rx := p.Subscribe()
	defer rx.Close()
	ctx := context.Background()
	if _, err := c.typed.CoreV1().Pods("kb-shop-prod").Create(ctx, managedPod("api-web-2", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new pod", func() bool { _, ok := p.Pod("kb-shop-prod/api-web-2"); return ok })
	if err := c.dynamic.Resource(appsGVR).Namespace("kb-shop-prod").Delete(ctx, "api", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the app delete", func() bool { _, ok := p.App("kb-shop-prod", "api"); return !ok })
	if err := c.dynamic.Resource(projectsGVR).Delete(ctx, "shop", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the project delete", func() bool { _, ok := p.Project("shop"); return !ok })
	var kinds []string
	for {
		got, ok := rx.TryRecv()
		if !ok {
			break
		}
		if got.Delta != nil {
			data, err := got.Delta.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			kind, _, _ := strings.Cut(string(data), ",")
			kinds = append(kinds, kind)
		}
	}
	want := []string{`{"kind":"pod_upsert"`, `{"kind":"app_delete"`, `{"kind":"project_delete"`}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("deltas: %v", kinds)
	}
}

// Optional kinds are watched once their API group is served, and never
// hold back readiness.
func TestOptionalKindsAreWatchedOnceServed(t *testing.T) {
	route := object("gateway.networking.k8s.io/v1", "HTTPRoute", "kb-shop-prod", "api",
		map[string]string{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue},
		map[string]any{"spec": map[string]any{
			"hostnames":  []any{"shop.acme.com"},
			"parentRefs": []any{map[string]any{"name": "kuben", "namespace": "kuben-system", "sectionName": render.HostListenerName("shop.acme.com")}},
		}})
	cert := object("cert-manager.io/v1", "Certificate", "kuben-system", render.HostSecretName("shop.acme.com"), nil,
		map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
	foreign := object("cert-manager.io/v1", "Certificate", "kuben-system", "someone-else", nil, nil)
	c := newCluster(route, cert, foreign)
	disc, ok := c.typed.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatal("fake discovery")
	}
	disc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "gateway.networking.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "httproutes", Kind: "HTTPRoute", Namespaced: true}}},
		{GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{{Name: "certificates", Kind: "Certificate", Namespaced: true}}},
	}
	// The groups are served once installed is set; until then discovery
	// fails, as on an API server that is still starting.
	var installed atomic.Bool
	c.typed.PrependReactor("get", "group", func(k8stesting.Action) (bool, runtime.Object, error) {
		if installed.Load() {
			return false, nil, nil
		}
		return true, nil, errors.New("not yet")
	})
	p := projection.New()
	cancel, done := start(t, c, p, fastTiming())
	defer stop(t, cancel, done)

	<-p.Synced()
	if _, ok := p.Route("kb-shop-prod", "api"); ok {
		t.Fatal("the Gateway API is not served yet")
	}
	installed.Store(true)
	eventually(t, "the exposure", func() bool {
		e, ok := p.Exposure("kb-shop-prod", "api")
		return ok && len(e.Hosts) == 1 && e.Hosts[0].CertificateReady.Or(false)
	})
}

func TestNoCompleteListWithinTheDeadlineIsAnError(t *testing.T) {
	c := newCluster()
	c.dynamic.PrependReactor("list", v1alpha1.AppResource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	p := projection.New()
	timing := fastTiming()
	timing.FirstSyncDeadline = 200 * time.Millisecond
	cancel, done := start(t, c, p, timing)
	defer cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no complete LIST within 0s for: apps") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no deadline")
	}
}

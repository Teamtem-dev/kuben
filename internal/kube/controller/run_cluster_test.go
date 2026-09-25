package controller_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

func gvr(plural string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: v1alpha1.SchemeGroupVersion.Group, Version: v1alpha1.SchemeGroupVersion.Version, Resource: plural}
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: never happened", what)
}

// The controllers against a real API server, as serve runs them: a
// project, its environment and an app become a namespace and the app's
// workload, and the objects report what they are waiting for. No kubelet
// runs, so nothing becomes ready.
func TestTheControllersTurnObjectsIntoWorkloads(t *testing.T) {
	c := apiserver.Connect(t)
	cluster, err := registry.NewCluster(registry.Primary, c.Config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var g errgroup.Group
	t.Cleanup(func() {
		cancel()
		if err := g.Wait(); err != nil {
			t.Errorf("controllers: %v", err)
		}
	})
	g.Go(func() error {
		return controller.RunAll(ctx, controller.Deps{ //nolint:wrapcheck // reported by the cleanup
			Logger: slog.New(slog.DiscardHandler), Cluster: cluster, Projections: projection.New(),
			Facts: &discovery.Watch{}, Health: health.New(clock.System{}), Clock: clock.System{},
		})
	})

	labels := map[string]any{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue}
	create := func(plural, namespace string, obj map[string]any) {
		t.Helper()
		ri := c.Dynamic.Resource(gvr(plural))
		var err error
		if namespace == "" {
			_, err = ri.Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
		} else {
			_, err = ri.Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatalf("create %s: %v", plural, err)
		}
	}
	object := func(kind string, meta map[string]any, spec map[string]any) map[string]any {
		return map[string]any{"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": kind, "metadata": meta, "spec": spec}
	}
	create(v1alpha1.ProjectResource, "", object(v1alpha1.ProjectKind,
		map[string]any{"name": "shop", "labels": labels}, map[string]any{"displayName": "Shop"}))
	create(v1alpha1.EnvironmentResource, "", object(v1alpha1.EnvironmentKind,
		map[string]any{"name": "shop-prod", "labels": labels}, map[string]any{"project": "shop"}))

	namespaces := c.Typed.CoreV1().Namespaces()
	eventually(t, "the environment's namespace", func() bool {
		ns, err := namespaces.Get(ctx, "kb-shop-prod", metav1.GetOptions{})
		return err == nil && ns.Labels[v1alpha1.LabelManagedBy] == v1alpha1.LabelManagerValue
	})

	create(v1alpha1.AppResource, "kb-shop-prod", object(v1alpha1.AppKind,
		map[string]any{"name": "web", "labels": labels},
		map[string]any{
			"source":  map[string]any{"image": "nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}},
		}))
	eventually(t, "the app's Deployment and Service", func() bool {
		_, derr := c.Typed.AppsV1().Deployments("kb-shop-prod").Get(ctx, "web-web", metav1.GetOptions{})
		_, serr := c.Typed.CoreV1().Services("kb-shop-prod").Get(ctx, "web", metav1.GetOptions{})
		if apierrors.IsNotFound(derr) || apierrors.IsNotFound(serr) {
			return false
		}
		return derr == nil && serr == nil
	})
	apps := c.Dynamic.Resource(gvr(v1alpha1.AppResource)).Namespace("kb-shop-prod")
	eventually(t, "the App's status", func() bool {
		app, err := apps.Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			return false
		}
		observed, found, _ := unstructured.NestedInt64(app.Object, "status", "observedGeneration")
		conditions, _, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
		return found && observed == app.GetGeneration() && len(conditions) > 0
	})
}

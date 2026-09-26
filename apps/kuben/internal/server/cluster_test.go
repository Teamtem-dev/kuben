package server_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/server"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestElectionNeedsANamespaceOnlyWhenEnabled(t *testing.T) {
	var cfg config.Config
	if e, err := server.Election(cfg); err != nil || e.IsSome() {
		t.Fatalf("off: %v %v", e, err)
	}
	cfg.Kube.LeaderElection = true
	cfg.Kube.Namespace = opt.Some("kuben-system")
	e, err := server.Election(cfg)
	got, ok := e.Get()
	if err != nil || !ok || got.Namespace != "kuben-system" || !strings.Contains(got.Identity, "_") {
		t.Fatalf("on: %+v %v", got, err)
	}
	// Outside a pod, without kube.namespace, there is nowhere for the Lease.
	cfg.Kube.Namespace = opt.None[string]()
	if _, err := server.Election(cfg); err == nil || !strings.Contains(err.Error(), "KUBEN_KUBE__NAMESPACE") {
		t.Fatalf("no namespace: %v", err)
	}
}

func fakeRegistry() *registry.Registry {
	lists := map[schema.GroupVersionResource]string{
		v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ProjectResource):     "ProjectList",
		v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.EnvironmentResource): "EnvironmentList",
		v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.AppResource):         "AppList",
	}
	return registry.Single(registry.Cluster{
		Typed:   fake.NewClientset(),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists),
	})
}

func TestAClusterMakesTheServerReadyOnceTheInformersListed(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.Roles = []config.Role{config.RoleAPI}
	h := health.New(clock.System{})
	p := projection.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := server.StartCluster(ctx, cfg, fakeRegistry(), p, h, quiet())

	deadline := time.Now().Add(10 * time.Second)
	for !h.IsReady() {
		if time.Now().After(deadline) {
			t.Fatalf("never ready; pending %v", p.PendingKinds())
		}
		time.Sleep(5 * time.Millisecond)
	}
	states := map[string]health.State{}
	for _, n := range h.Details() {
		states[n.Name] = n.State
	}
	if states["cluster"] != health.Ok {
		t.Fatalf("cluster: %v", states)
	}
	if _, ok := states["discovery"]; ok {
		t.Fatal("discovery runs on controller replicas only")
	}

	cancel()
	stopped := make(chan struct{})
	go func() { server.WaitAll(done, 10*time.Second, quiet()); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the subsystems did not stop with their context")
	}
	for i, d := range done {
		select {
		case <-d:
		default:
			t.Fatalf("subsystem %d still running", i)
		}
	}
}

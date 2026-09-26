package discovery_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeCluster is a cluster of fake clients.
type fakeCluster struct {
	typed   *fake.Clientset
	dynamic *dynamicfake.FakeDynamicClient
	disc    *fakediscovery.FakeDiscovery
}

func (f fakeCluster) cluster() registry.Cluster {
	return registry.Cluster{ID: registry.Primary, Typed: f.typed, Dynamic: f.dynamic}
}

// forbid makes verb on resource fail as RBAC would.
func forbid(f *k8stesting.Fake, verb, resource string) {
	f.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
	})
}

func resources(groupVersion string, r ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: r}
}

func unstructuredObject(apiVersion, kind string, obj map[string]any) *unstructured.Unstructured {
	obj["apiVersion"] = apiVersion
	obj["kind"] = kind
	return &unstructured.Unstructured{Object: obj}
}

// k3sCluster has the Gateway API (two versions), cert-manager, the metrics
// API and one k3s node.
func k3sCluster(t *testing.T) fakeCluster {
	t.Helper()
	gatewayCRD := unstructuredObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", map[string]any{
		"metadata": map[string]any{
			"name": "gateways.gateway.networking.k8s.io",
			"annotations": map[string]any{
				"gateway.networking.k8s.io/bundle-version": "v1.5.1",
				"gateway.networking.k8s.io/channel":        "standard",
			},
		},
	})
	traefik := unstructuredObject("gateway.networking.k8s.io/v1", "GatewayClass", object("traefik", map[string]any{
		"spec":   map[string]any{"controllerName": "traefik.io/gateway-controller"},
		"status": conditions(map[string]any{"type": "Accepted", "status": "True"}),
	}))
	broken := unstructuredObject("gateway.networking.k8s.io/v1", "GatewayClass", object("a-broken", map[string]any{
		"spec":   map[string]any{"controllerName": "example.com/gateway"},
		"status": conditions(map[string]any{"type": "Accepted", "status": "False", "message": "invalid parameters"}),
	}))
	issuer := unstructuredObject("cert-manager.io/v1", "ClusterIssuer", object("letsencrypt", map[string]any{
		"status": conditions(map[string]any{"type": "Ready", "status": "True", "message": "registered"}),
	}))
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}:       "GatewayClassList",
		{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}:                 "ClusterIssuerList",
	}, gatewayCRD, traefik, broken, issuer)

	node := readyNode("4", "8Gi", true, corev1.NodeSpec{})
	node.Status.NodeInfo.KubeletVersion = "v1.36.4+k3s1"
	storage := class("local-path", true)
	svclb := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "svclb-traefik", Namespace: "kube-system"}}
	typed := fake.NewClientset(&node, &storage, &svclb)
	disc, ok := typed.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatal("fake discovery")
	}
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.36.4+k3s1"}
	disc.Resources = []*metav1.APIResourceList{
		resources("v1", metav1.APIResource{Name: "nodes", Kind: "Node"}),
		resources("gateway.networking.k8s.io/v1",
			metav1.APIResource{Name: "gatewayclasses", Kind: "GatewayClass"},
			metav1.APIResource{Name: "gateways", Kind: "Gateway"},
			metav1.APIResource{Name: "gateways/status", Kind: "Gateway"},
			metav1.APIResource{Name: "httproutes", Kind: "HTTPRoute"},
			metav1.APIResource{Name: "grpcroutes", Kind: "GRPCRoute"}),
		resources("gateway.networking.k8s.io/v1beta1",
			metav1.APIResource{Name: "referencegrants", Kind: "ReferenceGrant"},
			metav1.APIResource{Name: "httproutes", Kind: "HTTPRoute"}),
		resources("cert-manager.io/v1", metav1.APIResource{Name: "clusterissuers", Kind: "ClusterIssuer"}),
		resources("metrics.k8s.io/v1beta1", metav1.APIResource{Name: "nodes", Kind: "NodeMetrics"}),
	}
	return fakeCluster{typed: typed, dynamic: dyn, disc: disc}
}

func facts(t *testing.T, f fakeCluster) discovery.ClusterFacts {
	t.Helper()
	return discovery.Discover(t.Context(), quiet(), f.cluster())
}

func diff(t *testing.T, want, got discovery.ClusterFacts) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("facts:\n got %s\nwant %s", show(t, got), show(t, want))
	}
}

func show(t *testing.T, f discovery.ClusterFacts) string {
	t.Helper()
	text, err := f.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func TestDiscoverReadsEveryCapability(t *testing.T) {
	got := facts(t, k3sCluster(t))
	want := discovery.ClusterFacts{
		GatewayAPI: opt.Some(discovery.GatewayAPI{
			BundleVersion: opt.Some("v1.5.1"),
			Channel:       opt.Some("standard"),
			Kinds:         []string{"GRPCRoute", "Gateway", "GatewayClass", "HTTPRoute", "ReferenceGrant"},
		}),
		GatewayClasses: []discovery.Readiness{
			{Name: "a-broken", Controller: opt.Some("example.com/gateway"), Message: opt.Some("invalid parameters")},
			{Name: "traefik", Controller: opt.Some("traefik.io/gateway-controller"), Ready: true},
		},
		CertManager:         true,
		ClusterIssuers:      []discovery.Readiness{{Name: "letsencrypt", Ready: true}},
		MetricsAPI:          true,
		KubernetesVersion:   opt.Some("v1.36.4+k3s1"),
		DefaultStorageClass: opt.Some("local-path"),
		NetworkPolicy:       opt.Some("k3s"),
		LargestNode:         opt.Some(discovery.NodeSize{CPUMillis: 4000, MemoryBytes: 8 << 30}),
	}
	diff(t, want, got)
	if !got.Serves("ReferenceGrant") || got.Serves("TLSRoute") {
		t.Fatal("served kinds")
	}
	if got.GatewayClass("a-broken") != (discovery.NotReady{Message: "invalid parameters"}) {
		t.Fatalf("a-broken: %#v", got.GatewayClass("a-broken"))
	}
}

// A probe that fails (RBAC, timeout) makes its facts unknown, never absent.
func TestFailedProbesAreUnknownNotAbsent(t *testing.T) {
	f := k3sCluster(t)
	forbid(&f.dynamic.Fake, "get", "customresourcedefinitions")
	forbid(&f.dynamic.Fake, "list", "gatewayclasses")
	forbid(&f.dynamic.Fake, "list", "clusterissuers")
	forbid(&f.typed.Fake, "get", "resource")
	forbid(&f.typed.Fake, "get", "version")
	forbid(&f.typed.Fake, "list", "storageclasses")
	forbid(&f.typed.Fake, "list", "nodes")
	got := facts(t, f)
	want := discovery.ClusterFacts{
		GatewayAPI:  opt.Some(discovery.GatewayAPI{}),
		CertManager: true,
		MetricsAPI:  true,
		Unknown: []string{
			discovery.ProbeGatewayAPIVersion,
			"resources:gateway.networking.k8s.io/v1",
			"resources:gateway.networking.k8s.io/v1beta1",
			discovery.ProbeGatewayClasses,
			discovery.ProbeClusterIssuers,
			discovery.ProbeVersion,
			discovery.ProbeStorageClasses,
			discovery.ProbeNetworkPolicy,
		},
	}
	diff(t, want, got)
	if got.Issuer("letsencrypt") != (discovery.Unknown{}) || got.GatewayClass("traefik") != (discovery.Unknown{}) {
		t.Fatal("unlisted names are unknown")
	}
	if got.LargestNode.IsSome() || got.NetworkPolicy.IsSome() {
		t.Fatal("no node facts without nodes")
	}
}

func TestDaemonSetsFailingMakesThePolicyUnknown(t *testing.T) {
	f := k3sCluster(t)
	forbid(&f.typed.Fake, "list", "daemonsets")
	got := facts(t, f)
	if got.NetworkPolicy.IsSome() || got.LargestNode.IsSome() {
		t.Fatalf("policy and node facts: %s", show(t, got))
	}
	if diff := cmp.Diff([]string{discovery.ProbeNetworkPolicy}, got.Unknown); diff != "" {
		t.Fatalf("unknown (-want +got):\n%s", diff)
	}
}

func TestWithoutAPIGroupsEverythingIsUnknown(t *testing.T) {
	f := k3sCluster(t)
	forbid(&f.typed.Fake, "get", "group")
	got := facts(t, f)
	diff(t, discovery.ClusterFacts{Unknown: []string{discovery.ProbeAPIGroups}}, got)
	if got.GatewayClass("traefik") != (discovery.Unknown{}) || got.Issuer("le") != (discovery.Unknown{}) {
		t.Fatal("offline: every named dependency is unknown")
	}
}

func TestABareClusterHasNothingMissingUnknown(t *testing.T) {
	typed := fake.NewClientset()
	disc, ok := typed.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatal("fake discovery")
	}
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.1"}
	disc.Resources = []*metav1.APIResourceList{
		resources("v1", metav1.APIResource{Name: "nodes", Kind: "Node"}),
		// The group is served but the Gateway CRD is gone: no annotations,
		// and nothing unknown.
		resources("gateway.networking.k8s.io/v1", metav1.APIResource{Name: "httproutes", Kind: "HTTPRoute"}),
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}:       "GatewayClassList",
	})
	got := facts(t, fakeCluster{typed: typed, dynamic: dyn, disc: disc})
	want := discovery.ClusterFacts{
		GatewayAPI:        opt.Some(discovery.GatewayAPI{Kinds: []string{"HTTPRoute"}}),
		KubernetesVersion: opt.Some("v1.34.1"),
	}
	diff(t, want, got)
	if got.Issuer("le") != (discovery.Missing{}) || got.GatewayClass("traefik") != (discovery.Missing{}) {
		t.Fatal("proven missing")
	}
}

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
)

const doctorPath = "/api/v1/projects/shop/environments/prod/apps/api/doctor"

// Without a Kubernetes cluster configured, the doctor answers 503 (after
// the app and the permission were checked).
func TestAppDoctorWithoutClusterAnswers503(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)
	if status := alice.status("GET", doctorPath, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("without cluster: %d", status)
	}
	if status := alice.status("GET", "/api/v1/projects/shop/environments/prod/apps/nope/doctor", nil); status != http.StatusNotFound {
		t.Fatalf("an unknown app: %d", status)
	}
}

// hosts answers lookups from a table; a host it does not know fails.
type hosts map[string][]string

func (h hosts) LookupHost(_ context.Context, host string) ([]string, error) {
	if found, ok := h[host]; ok {
		return found, nil
	}
	return nil, errors.New("no such host")
}

var (
	gatewaysGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	configsGVR  = schema.GroupVersionResource{Group: "kuben.dev", Version: "v1alpha1", Resource: "kubenconfigs"}
)

// doctorCluster is a fake cluster with Kuben's KubenConfig (traefik,
// letsencrypt) and its Gateway, programmed at 127.0.0.1.
func doctorCluster(t *testing.T) registry.Cluster {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gatewaysGVR: "GatewayList", configsGVR: "KubenConfigList"})
	objects := []struct {
		gvr       schema.GroupVersionResource
		namespace string
		obj       map[string]any
	}{
		{configsGVR, "", map[string]any{
			"apiVersion": "kuben.dev/v1alpha1", "kind": "KubenConfig",
			"metadata": map[string]any{"name": "kuben"},
			"spec":     map[string]any{"gatewayClassName": "traefik", "clusterIssuer": "letsencrypt"},
		}},
		{gatewaysGVR, "kuben-system", map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
			"metadata": map[string]any{"name": "kuben", "namespace": "kuben-system"},
			"status": map[string]any{
				"addresses":  []any{map[string]any{"type": "IPAddress", "value": "127.0.0.1"}},
				"conditions": []any{map[string]any{"type": "Programmed", "status": "True"}},
			},
		}},
	}
	for _, o := range objects {
		// Created rather than seeded: seeding guesses "gatewaies".
		if _, err := dyn.Resource(o.gvr).Namespace(o.namespace).Create(t.Context(),
			&unstructured.Unstructured{Object: o.obj}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return registry.Cluster{ID: registry.Primary, Typed: fake.NewClientset(), Dynamic: dyn}
}

// doctorServer is a server on f's store and projections that reaches
// cluster and resolves api.example.com to the Gateway's address; its URL.
func doctorServer(t *testing.T, f fixture, cluster registry.Cluster) string {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Bind = "127.0.0.1:3000"
	h := health.New(clock.System{})
	h.SetReady(true)
	server, err := api.New(api.Deps{
		Config: cfg, Store: f.store, Hasher: auth.InsecureForTests(), Health: h,
		Projections: f.projections, Images: testImages(t),
		Cluster:  opt.Some(registry.Single(cluster)),
		Resolver: hosts{"api.example.com": {"127.0.0.1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// signInAt is email signed in to the server at base.
func signInAt(t *testing.T, base, email string) *client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: base, http: &http.Client{Jar: jar}}
	if status, body, _ := c.do("POST", "/api/v1/auth/login",
		map[string]any{"email": email, "password": seedPassword}); status != http.StatusOK {
		t.Fatalf("sign in: %d %v", status, body)
	}
	return c
}

// doctorClient is alice, signed in to doctorServer.
func doctorClient(t *testing.T, f fixture, cluster registry.Cluster) *client {
	t.Helper()
	return signInAt(t, doctorServer(t, f, cluster), "alice@example.com")
}

// recordFacts records facts of the primary cluster observed at observedAt.
func recordFacts(t *testing.T, f fixture, facts discovery.ClusterFacts, observedAt int64) {
	t.Helper()
	if _, err := f.store.RecordCapabilitiesEverywhere(t.Context(), registry.Primary, facts, observedAt); err != nil {
		t.Fatal(err)
	}
}

// seedRoute is the app's HTTPRoute as the projections see it: accepted,
// with a listener of its own for api.example.com and no certificate yet.
func seedRoute(f fixture) {
	f.projections.UpsertRoute(projection.RouteView{
		Key: "kb-shop-prod/api", Namespace: "kb-shop-prod", Name: "api",
		Gateways: []string{"kuben-system/kuben"},
		Sections: []string{render.HostListenerName("api.example.com")},
		Domains:  []render.DomainClaim{{Host: "api.example.com", TLS: "auto"}},
		Accepted: opt.Some(true),
	})
}

// seen is a check without its detail and hint; a port's status is left
// out (it depends on what listens on this machine).
type seen struct{ ID, Subject, Status string }

func seenChecks(t *testing.T, body map[string]any) []seen {
	t.Helper()
	list, _ := body["checks"].([]any)
	out := make([]seen, 0, len(list))
	for _, item := range list {
		c, _ := item.(map[string]any)
		id, _ := c["id"].(string)
		subject, _ := c["subject"].(string)
		status, _ := c["status"].(string)
		if id == "port-80" || id == "port-443" {
			status = "?"
		}
		out = append(out, seen{id, subject, status})
	}
	return out
}

func checkNamed(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	list, _ := body["checks"].([]any)
	for _, item := range list {
		if c, _ := item.(map[string]any); c["id"] == id {
			return c
		}
	}
	t.Fatalf("no check %q: %v", id, body)
	return nil
}

// routes/apps/doctor.rs doctor: the checks, in the report's order, from
// the recorded facts, the Gateway, the projections and the app's domains.
func TestAppDoctorAssemblesTheChecks(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	f.seedAppProjections()
	seedRoute(f)
	recordFacts(t, f, discovery.ClusterFacts{
		GatewayClasses: []discovery.Readiness{{Name: "traefik", Ready: true}},
		CertManager:    true,
		ClusterIssuers: []discovery.Readiness{{Name: "letsencrypt", Ready: true}},
	}, clock.System{}.NowMs())
	alice := doctorClient(t, f, doctorCluster(t))

	status, body, _ := alice.do("GET", doctorPath, nil)
	if status != http.StatusOK {
		t.Fatalf("doctor: %d %v", status, body)
	}
	want := []seen{
		{"gateway-class", "traefik", "ok"},
		{"gateway", "kuben-system/kuben", "ok"},
		{"issuer", "letsencrypt", "ok"},
		{"port-80", "80", "?"},
		{"port-443", "443", "?"},
		{"route", "", "ok"},
		{"certificate", "api.example.com", "fail"},
		{"dns", "api.example.com", "ok"},
		{"claim", "api.example.com", "warn"},
		{"delegation", "api.example.com", "unknown"},
		{"proxy", "api.example.com", "unknown"},
	}
	if diff := cmp.Diff(want, seenChecks(t, body)); diff != "" {
		t.Fatalf("checks (-want +got):\n%s", diff)
	}
	if body["status"] != "fail" {
		t.Fatalf("overall: %v", body["status"])
	}
	gateway := checkNamed(t, body, "gateway")
	if hint, present := gateway["hint"]; gateway["detail"] != "programmed (127.0.0.1)" || !present || hint != nil {
		t.Fatalf("an ok check has a null hint: %v", gateway)
	}
	certificate := checkNamed(t, body, "certificate")
	if certificate["detail"] != "not issued yet" || certificate["hint"] == nil {
		t.Fatalf("certificate: %v", certificate)
	}
	graph, _ := body["graph"].(map[string]any)
	findings, isList := body["findings"].([]any)
	if graph == nil || !isList || len(findings) != 0 {
		t.Fatalf("graph and findings: %v %v", body["graph"], body["findings"])
	}

	// A viewer may read it too.
	if status, body, _ := signInAt(t, alice.base, "bob@example.com").do("GET", doctorPath, nil); status != http.StatusOK {
		t.Fatalf("viewer: %d %v", status, body)
	}
}

// Facts older than five minutes are asked of the cluster again: this fake
// serves no Gateway API, so its GatewayClass does not exist.
func TestAppDoctorAsksTheClusterWhenFactsAreOld(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	recordFacts(t, f, discovery.ClusterFacts{
		GatewayClasses: []discovery.Readiness{{Name: "traefik", Ready: true}},
	}, clock.System{}.NowMs()-api.FactsFreshMs)
	alice := doctorClient(t, f, doctorCluster(t))
	status, body, _ := alice.do("GET", doctorPath, nil)
	if status != http.StatusOK {
		t.Fatalf("doctor: %d %v", status, body)
	}
	class := checkNamed(t, body, "gateway-class")
	if class["status"] != string(doctor.StatusFail) || class["detail"] != "does not exist" {
		t.Fatalf("gateway-class: %v", class)
	}
	// No route in the projections, and the app has a host.
	if route := checkNamed(t, body, "route"); route["status"] != "fail" || route["detail"] != "the app has no route yet" {
		t.Fatalf("route: %v", route)
	}
}

// Without a KubenConfig there is no Gateway: one platform check, the port
// has no address to try, no 443 without TLS, and DNS has nothing to
// compare with.
func TestAppDoctorWithoutAGateway(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	recordFacts(t, f, discovery.ClusterFacts{}, clock.System{}.NowMs())
	empty := registry.Cluster{
		ID: registry.Primary, Typed: fake.NewClientset(),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{gatewaysGVR: "GatewayList", configsGVR: "KubenConfigList"}),
	}
	alice := doctorClient(t, f, empty)
	status, body, _ := alice.do("GET", doctorPath, nil)
	if status != http.StatusOK {
		t.Fatalf("doctor: %d %v", status, body)
	}
	want := []seen{
		{"gateway", "", "fail"},
		{"port-80", "80", "?"},
		{"route", "", "fail"},
		{"dns", "api.example.com", "unknown"},
		{"claim", "api.example.com", "warn"},
		{"delegation", "api.example.com", "unknown"},
		{"proxy", "api.example.com", "unknown"},
	}
	if diff := cmp.Diff(want, seenChecks(t, body)); diff != "" {
		t.Fatalf("checks (-want +got):\n%s", diff)
	}
	if port := checkNamed(t, body, "port-80"); port["status"] != "unknown" {
		t.Fatalf("no address to try: %v", port)
	}
}

// The report: the worst status, a null hint on an ok check, the graph
// and findings not observed yet.
func TestDoctorReportShape(t *testing.T) {
	report := api.DoctorReportOf([]doctor.Check{
		{ID: "dns", Subject: "a", Status: doctor.StatusOK, Detail: "fine"},
		{ID: "claim", Subject: "a", Status: doctor.StatusWarn, Detail: "d", Hint: opt.Some("h")},
	})
	if report.Status != "warn" || len(report.Checks) != 2 || !report.Checks[0].Hint.IsNull() ||
		report.Checks[1].Hint.Value != "h" {
		t.Fatalf("report: %+v", report)
	}
	if empty := api.DoctorReportOf(nil); empty.Status != "ok" || len(empty.Checks) != 0 {
		t.Fatalf("no checks: %+v", empty)
	}
}

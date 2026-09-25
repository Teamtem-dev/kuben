package api_test

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/dns"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/evidence"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
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
// cluster, resolves api.example.com to the Gateway's address and asks
// fake for name servers and provider accounts; its URL.
func doctorServer(t *testing.T, f fixture, cluster registry.Cluster, fake *fakeDNS) string {
	t.Helper()
	return serverOn(t, f, func(d *api.Deps) {
		d.Images = testImages(t)
		d.Cluster = opt.Some(registry.Single(cluster))
		d.Resolver = hosts{"api.example.com": {"127.0.0.1"}}
		d.DNS = fake
	})
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

// doctorClient is alice, signed in to doctorServer with a fake DNS that
// holds no record.
func doctorClient(t *testing.T, f fixture, cluster registry.Cluster) *client {
	t.Helper()
	return signInAt(t, doctorServer(t, f, cluster, &fakeDNS{}), "alice@example.com")
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
		{"delegation", "api.example.com", "ok"},
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
	// The evidence graph: an image app has no build; the fake cluster has
	// no Deployment, Service or EndpointSlice; the projections know one
	// ready pod; the app serves, so the network layers come from the checks.
	graph, _ := body["graph"].(map[string]any)
	nodes, _ := graph["nodes"].([]any)
	var layers []string
	subjects := map[string]string{}
	statuses := map[string]string{}
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		layer, _ := n["layer"].(string)
		layers = append(layers, layer)
		subjects[layer], _ = n["subject"].(string)
		statuses[layer], _ = n["status"].(string)
	}
	wantLayers := []string{"release", "deployment", "pods", "service", "endpoints", "gateway", "route", "dns", "tls"}
	if diff := cmp.Diff(wantLayers, layers); diff != "" {
		t.Fatalf("layers (-want +got):\n%s", diff)
	}
	for layer, want := range map[string][2]string{
		"deployment": {"no Deployment", "fail"},
		"pods":       {"1 of 1 pods ready", "ok"},
		"service":    {"Service api", "fail"},
		"endpoints":  {"0 ready endpoint(s)", "fail"},
		"route":      {"route", "ok"},
		"dns":        {"DNS", "warn"},
		"tls":        {"certificates", "fail"},
	} {
		if got := [2]string{subjects[layer], statuses[layer]}; got != want {
			t.Fatalf("%s: %v, want %v", layer, got, want)
		}
	}
	if edges, _ := graph["edges"].([]any); len(edges) == 0 {
		t.Fatalf("edges: %v", graph)
	}
	findings, _ := body["findings"].([]any)
	kinds := map[string]string{}
	for _, item := range findings {
		f, _ := item.(map[string]any)
		layer, _ := f["layer"].(string)
		kinds[layer], _ = f["kind"].(string)
	}
	if kinds["deployment"] != "rootCause" || kinds["service"] != "rootCause" || kinds["endpoints"] != "symptom" {
		t.Fatalf("findings: %v", body["findings"])
	}
	if first, _ := findings[0].(map[string]any); first["kind"] != "rootCause" {
		t.Fatalf("root causes first: %v", findings)
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
		{"delegation", "api.example.com", "ok"},
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
// and its findings as serde_json values (keys sorted).
func TestDoctorReportShape(t *testing.T) {
	graph := evidence.Of([]evidence.Node{
		evidence.NewNode(evidence.Pods, doctor.StatusFail, "no pods").Fact("no pod of the app is known"),
	})
	report, err := api.DoctorReportOf([]doctor.Check{
		{ID: "dns", Subject: "a", Status: doctor.StatusOK, Detail: "fine"},
		{ID: "claim", Subject: "a", Status: doctor.StatusWarn, Detail: "d", Hint: opt.Some("h")},
	}, graph, evidence.Diagnose(graph))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "warn" || len(report.Checks) != 2 || !report.Checks[0].Hint.IsNull() ||
		report.Checks[1].Hint.Value != "h" {
		t.Fatalf("report: %+v", report)
	}
	gotGraph := map[string]string{}
	for k, v := range report.Graph {
		gotGraph[k] = string(v)
	}
	wantGraph := map[string]string{
		"nodes": `[{"action":null,"evidence":["no pod of the app is known"],"layer":"pods","observedAt":null,"status":"fail","subject":"no pods"}]`,
		"edges": `[]`,
	}
	if diff := cmp.Diff(wantGraph, gotGraph); diff != "" {
		t.Fatalf("graph (-want +got):\n%s", diff)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings: %+v", report.Findings)
	}
	gotFinding := map[string]string{}
	for k, v := range report.Findings[0] {
		gotFinding[k] = string(v)
	}
	wantFinding := map[string]string{
		"kind": `"rootCause"`, "layer": `"pods"`, "status": `"fail"`, "confidence": `"high"`, "summary": `"no pods"`,
		"evidence": `["no pod of the app is known"]`, "related": `[]`, "action": `null`,
	}
	if diff := cmp.Diff(wantFinding, gotFinding); diff != "" {
		t.Fatalf("finding (-want +got):\n%s", diff)
	}
	empty, err := api.DoctorReportOf(nil, evidence.Of(nil), evidence.Diagnose(evidence.Of(nil)))
	if err != nil || empty.Status != "ok" || len(empty.Checks) != 0 || len(empty.Findings) != 0 ||
		string(empty.Graph["nodes"]) != "[]" || string(empty.Graph["edges"]) != "[]" {
		t.Fatalf("no checks: %+v %v", empty, err)
	}
}

// handOverToAgent links an agent to the primary cluster (with the runtime
// feature) and hands app api over to it; the cluster.
func handOverToAgent(t *testing.T, f fixture) ids.ClusterID {
	t.Helper()
	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	cluster, err := tn.EnsureCluster(ctx, registry.Primary)
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := tn.Project(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := tn.Environment(ctx, project.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	app, _, err := tn.App(ctx, env.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	// The ensured cluster row stays locked until the transaction ends, and
	// the agent's rows point at it: read only, and let go first.
	if err := tn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	const device = "sha256:device-a"
	if err := f.store.RecordAgentCertificate(ctx, f.org, cluster, device, clock.System{}.NowMs()+3_600_000); err != nil {
		t.Fatal(err)
	}
	if linked, err := f.store.RecordAgentLink(ctx, cluster, device, 1, []string{store.RuntimeFeature}, "1.0.0"); err != nil || !linked {
		t.Fatalf("link: %v %v", linked, err)
	}
	tn, err = f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if handed, err := tn.HandOverToAgent(ctx, app.Target); err != nil || !handed {
		t.Fatalf("hand over: %v %v", handed, err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// routes/apps/doctor.rs domain_checks and agent: the delegation from the
// name servers, the proxy from the organization's provider account, and
// the agent of an agent-delivered app, linked and then revoked.
func TestAppDoctorReadsDelegationProxyAndAgent(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	recordFacts(t, f, discovery.ClusterFacts{}, clock.System{}.NowMs())
	addProvider(t, f, "good")
	cluster := handOverToAgent(t, f)
	fake := &fakeDNS{records: []dns.ProviderRecord{{
		ID: "r1", Name: "api.example.com", RecordType: "A", Content: "203.0.113.10", Proxied: true,
	}}}
	alice := signInAt(t, doctorServer(t, f, doctorCluster(t), fake), "alice@example.com")

	status, body, _ := alice.do("GET", doctorPath, nil)
	if status != http.StatusOK {
		t.Fatalf("doctor: %d %v", status, body)
	}
	delegation := checkNamed(t, body, "delegation")
	if delegation["status"] != "ok" ||
		delegation["detail"] != "example.com is served by ada.ns.cloudflare.com (Cloudflare)" {
		t.Fatalf("delegation: %v", delegation)
	}
	if proxy := checkNamed(t, body, "proxy"); proxy["status"] != "warn" {
		t.Fatalf("proxy: %v", proxy)
	}
	agent := checkNamed(t, body, "agent")
	if detail, _ := agent["detail"].(string); agent["status"] != "ok" || !strings.HasPrefix(detail, "linked, heard from ") {
		t.Fatalf("agent: %v", agent)
	}

	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if revoked, err := tn.RevokeClusterAgent(ctx, cluster); err != nil || !revoked {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, body, _ = alice.do("GET", doctorPath, nil)
	if agent := checkNamed(t, body, "agent"); agent["status"] != "fail" ||
		agent["detail"] != "the cluster's agent was revoked" {
		t.Fatalf("revoked agent: %v", agent)
	}
}

// Without a keyring no provider account can be opened: the proxy is
// unknown, as in Rust.
func TestAppDoctorWithoutAKeyringCannotSeeTheProxy(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	recordFacts(t, f, discovery.ClusterFacts{}, clock.System{}.NowMs())
	addProvider(t, f, "good")
	fake := &fakeDNS{records: []dns.ProviderRecord{{ID: "r1", Name: "api.example.com", RecordType: "A", Proxied: true}}}
	base := serverOn(t, f, func(d *api.Deps) {
		d.Cluster = opt.Some(registry.Single(doctorCluster(t)))
		d.Resolver = hosts{}
		d.DNS = fake
		d.Keyring = opt.None[*secrets.Keyring]()
	})
	_, body, _ := signInAt(t, base, "alice@example.com").do("GET", doctorPath, nil)
	if proxy := checkNamed(t, body, "proxy"); proxy["status"] != "unknown" {
		t.Fatalf("proxy: %v", proxy)
	}
}

// An agent is stale after three heartbeats, never sooner than 30 s, and
// the product saturates instead of wrapping.
func TestAgentStaleAfterThreeHeartbeats(t *testing.T) {
	for _, c := range []struct{ heartbeat, want uint64 }{
		{0, 30},
		{10, 30},
		{11, 33},
		{60, 180},
		{math.MaxUint64 / 3, math.MaxUint64 / 3 * 3},
		{math.MaxUint64/3 + 1, math.MaxUint64},
	} {
		if got := api.AgentStaleAfter(c.heartbeat); got != c.want {
			t.Errorf("heartbeat %d: %d, want %d", c.heartbeat, got, c.want)
		}
	}
}

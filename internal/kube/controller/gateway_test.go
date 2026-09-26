package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// The tests of controller::gateway (the name tests are render's).

func platformSpec(t *testing.T, spec string) render.Platform {
	t.Helper()
	return render.PlatformFromSpec(decode[v1alpha1.KubenConfigSpec](t, spec))
}

func namedPlatform(t *testing.T, wildcard bool) render.Platform {
	t.Helper()
	spec := `{ "baseDomain": "apps.example.com", "gateway": "kuben-system/kuben", "clusterIssuer": "letsencrypt"`
	if wildcard {
		spec += `, "wildcardTlsSecret": "apps-wildcard"`
	}
	return platformSpec(t, spec+"}")
}

func ownedPlatform(t *testing.T) render.Platform {
	t.Helper()
	return platformSpec(t, `{ "baseDomain": "apps.example.com", "gatewayClassName": "traefik", "clusterIssuer": "letsencrypt" }`)
}

func hosts(pairs ...[2]string) []controller.HostClaim {
	out := make([]controller.HostClaim, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, controller.HostClaim{Claim: render.DomainClaim{Host: p[0], TLS: "auto"}, Namespace: p[1]})
	}
	return out
}

func gatewayOf(t *testing.T, p render.Platform) render.GatewayRef {
	t.Helper()
	g, ok := p.Gateway.Get()
	if !ok {
		t.Fatal("no gateway")
	}
	return g
}

// path walks JSON maps by key and arrays by index.
func path(v any, steps ...any) any {
	for _, s := range steps {
		switch k := s.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			switch a := v.(type) {
			case []any:
				if k >= len(a) {
					return nil
				}
				v = a[k]
			case []map[string]any:
				if k >= len(a) {
					return nil
				}
				v = a[k]
			default:
				return nil
			}
		}
	}
	return v
}

func byHost(t *testing.T, plan controller.ListenerPlan, host string) map[string]any {
	t.Helper()
	for _, l := range plan.Listeners {
		if l["hostname"] == host {
			return l
		}
	}
	t.Fatalf("no listener for %s", host)
	return nil
}

func TestListenerNamesAreStableAndDistinct(t *testing.T) {
	first, again := render.HostListenerName("api.acme.com"), render.HostListenerName("api.acme.com")
	if first != again || first == render.HostListenerName("www.acme.com") {
		t.Fatal("stable and distinct")
	}
	if name := render.HostListenerName("api.acme.com"); !strings.HasPrefix(name, "h-") || len(name) != 14 {
		t.Fatal(name)
	}
	if !strings.HasPrefix(render.HostSecretName("api.acme.com"), "kuben-tls-") {
		t.Fatal(render.HostSecretName("api.acme.com"))
	}
}

func TestWildcardCoversExactlyOneLabel(t *testing.T) {
	p := namedPlatform(t, true)
	if render.SectionForHost("api-shop-prod.apps.example.com", p) != render.WildcardListener {
		t.Fatal("one label is covered")
	}
	for _, host := range []string{"a.b.apps.example.com", "apps.example.com", "api.acme.com"} {
		if render.SectionForHost(host, p) == render.WildcardListener {
			t.Errorf("%s is not covered", host)
		}
	}
	if render.SectionForHost("api-shop-prod.apps.example.com", namedPlatform(t, false)) == render.WildcardListener {
		t.Fatal("no wildcard secret → per-host listener")
	}
}

func TestHostsAreFirstComeAndScopedToTheirNamespace(t *testing.T) {
	plan := controller.PlanListeners(hosts(
		[2]string{"shop.acme.com", "kb-b"}, [2]string{"shop.acme.com", "kb-a"}, [2]string{"api.acme.com", "kb-a"},
	), namedPlatform(t, false))
	if diff := cmp.Diff([]string{"shop.acme.com: owned by kb-a, also requested by kb-b"}, plan.Conflicts); diff != "" {
		t.Fatal(diff)
	}
	shop := byHost(t, plan, "shop.acme.com")
	if got := path(shop, "allowedRoutes", "namespaces", "selector", "matchLabels", "kubernetes.io/metadata.name"); got != "kb-a" {
		t.Fatalf("sorted input: kb-a claims the host first, got %v", got)
	}
	if got := path(shop, "tls", "certificateRefs", 0, "name"); got != render.HostSecretName("shop.acme.com") {
		t.Fatalf("certificate %v", got)
	}
	http := plan.Listeners[0]
	if http["name"] != "http" || path(http, "allowedRoutes", "namespaces", "from") != "Same" {
		t.Fatalf("http: %v", http)
	}
}

func TestWildcardListenerAbsorbsGeneratedHostsAndTheCapHolds(t *testing.T) {
	plan := controller.PlanListeners(hosts([2]string{"api-shop-prod.apps.example.com", "kb-shop-prod"}), namedPlatform(t, true))
	if len(plan.Listeners) != 2 || plan.Listeners[1]["hostname"] != "*.apps.example.com" {
		t.Fatalf("http + wildcard only: %v", plan.Listeners)
	}
	var many [][2]string
	for i := range 70 {
		many = append(many, [2]string{fmt.Sprintf("h%d.acme.com", i), "kb-x"})
	}
	capped := controller.PlanListeners(hosts(many...), namedPlatform(t, false))
	if len(capped.Listeners) != controller.MaxListeners || len(capped.Skipped) != 70-(controller.MaxListeners-1) {
		t.Fatalf("%d listeners, %d skipped", len(capped.Listeners), len(capped.Skipped))
	}
	if diff := cmp.Diff(capped, controller.PlanListeners(hosts(many...), namedPlatform(t, false))); diff != "" {
		t.Fatal("deterministic:", diff)
	}
}

func TestGatewayPatchAndRedirectRoute(t *testing.T) {
	p := namedPlatform(t, false)
	gw := gatewayOf(t, p)
	patch := controller.GatewayPatch(gw, p, controller.PlanListeners(hosts([2]string{"api.acme.com", "kb-a"}), p))
	if path(patch, "metadata", "annotations", controller.IssuerAnnotation) != "letsencrypt" {
		t.Fatalf("annotation: %v", patch["metadata"])
	}
	if l, _ := path(patch, "spec", "listeners").([]any); len(l) != 2 {
		t.Fatalf("listeners: %v", l)
	}
	if path(patch, "metadata", "labels") != nil || path(patch, "spec", "gatewayClassName") != nil {
		t.Fatal("an existing Gateway keeps its own class and labels")
	}
	route := controller.RedirectRouteFor(gw)
	if path(route, "spec", "parentRefs", 0, "sectionName") != render.HTTPListener ||
		path(route, "spec", "rules", 0, "filters", 0, "requestRedirect", "scheme") != "https" {
		t.Fatalf("route: %v", route)
	}
}

func TestWithoutTLSOnlyPlainHTTPIsListedForKubensNamespaces(t *testing.T) {
	p := namedPlatform(t, true)
	p.TLS = false
	plan := controller.PlanListeners(hosts([2]string{"api.acme.com", "kb-a"}, [2]string{"api.acme.com", "kb-b"}), p)
	if len(plan.Listeners) != 1 {
		t.Fatal("no HTTPS listener without a Ready issuer")
	}
	if len(plan.Conflicts) != 0 || len(plan.Skipped) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
	if path(plan.Listeners[0], "allowedRoutes", "namespaces", "selector", "matchLabels", v1alpha1.LabelManagedBy) != v1alpha1.LabelManagerValue {
		t.Fatalf("http: %v", plan.Listeners[0])
	}
	if path(controller.GatewayPatch(gatewayOf(t, p), p, plan), "metadata", "annotations") != nil {
		t.Fatal("no issuer annotation: cert-manager must not order certificates")
	}
}

func gatewayObject(labels map[string]string, status map[string]any) *unstructured.Unstructured {
	g := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": "kuben", "namespace": "kuben-system"},
		"status":     status,
	}}
	g.SetLabels(labels)
	return g
}

func TestKubenWritesOnlyAGatewayItOwns(t *testing.T) {
	owned := ownedPlatform(t)
	gw := gatewayOf(t, owned)
	if gw != render.OwnedDefaultGateway() {
		t.Fatalf("default gateway %v", gw)
	}
	mine := gatewayObject(map[string]string{v1alpha1.LabelGatewayOwner: v1alpha1.LabelManagerValue}, map[string]any{})
	theirs := gatewayObject(map[string]string{"team": "edge"}, map[string]any{})
	if controller.OwnershipOf(opt.Some(mine)) != controller.OwnedByKuben ||
		controller.OwnershipOf(opt.Some(theirs)) != controller.OwnedByOthers ||
		controller.OwnershipOf(opt.None[*unstructured.Unstructured]()) != controller.GatewayAbsent {
		t.Fatal("ownership")
	}
	if controller.Refusal(gw, owned, controller.GatewayAbsent).IsSome() || controller.Refusal(gw, owned, controller.OwnedByKuben).IsSome() {
		t.Fatal("created by Kuben, or Kuben's")
	}
	foreign, refused := controller.Refusal(gw, owned, controller.OwnedByOthers).Get()
	if !refused || foreign.OK || foreign.Reason != "GatewayNotOwned" || !strings.Contains(foreign.Message, "kuben.dev/gateway-owner=kuben") {
		t.Fatalf("foreign: %+v", foreign)
	}
	named := namedPlatform(t, false)
	if v, _ := controller.Refusal(gatewayOf(t, named), named, controller.GatewayAbsent).Get(); v.Reason != "GatewayMissing" {
		t.Fatalf("missing: %+v", v)
	}
	if controller.Refusal(gatewayOf(t, named), named, controller.OwnedByKuben).IsSome() {
		t.Fatal("dedicated by label")
	}
	patch := controller.GatewayPatch(render.OwnedDefaultGateway(), owned, controller.PlanListeners(nil, owned))
	if path(patch, "spec", "gatewayClassName") != "traefik" ||
		path(patch, "metadata", "labels", v1alpha1.LabelGatewayOwner) != v1alpha1.LabelManagerValue {
		t.Fatalf("patch: %v", patch)
	}
}

func TestReadinessFollowsProgrammedAndExplainsTheClass(t *testing.T) {
	owned := ownedPlatform(t)
	programmed := gatewayObject(nil, map[string]any{
		"conditions": []any{map[string]any{"type": "Programmed", "status": "True"}},
		"addresses":  []any{map[string]any{"type": "IPAddress", "value": "203.0.113.7"}},
	})
	none := opt.None[discovery.ClusterFacts]()
	if got := controller.Readiness(programmed, owned, none); got != (controller.Verdict{OK: true, Reason: "Programmed", Message: "programmed at 203.0.113.7"}) {
		t.Fatalf("programmed: %+v", got)
	}
	blocked := owned
	blocked.TLS = false
	if got := controller.Readiness(programmed, blocked, none); !strings.Contains(got.Message, "HTTPS is off") {
		t.Fatalf("blocked: %+v", got)
	}
	failing := gatewayObject(nil, map[string]any{
		"conditions": []any{map[string]any{"type": "Programmed", "status": "False", "message": "no address"}},
	})
	if got := controller.Readiness(failing, owned, none); got.Reason != "NotProgrammed" || got.Message != "no address" {
		t.Fatalf("failing: %+v", got)
	}
	fresh := gatewayObject(nil, map[string]any{})
	facts := discovery.ClusterFacts{GatewayAPI: opt.Some(discovery.GatewayAPI{})}
	if got := controller.Readiness(fresh, owned, opt.Some(facts)); got.Reason != "GatewayClassMissing" {
		t.Fatalf("fresh with facts: %+v", got)
	}
	if got := controller.Readiness(fresh, owned, none); got.Reason != "Pending" {
		t.Fatalf("fresh: %+v", got)
	}
}

func route(namespace, gateway string, domains ...[2]string) *projection.RouteView {
	r := &projection.RouteView{Key: namespace + "/api", Namespace: namespace, Name: "api", Gateways: []string{gateway}}
	for _, d := range domains {
		r.Domains = append(r.Domains, render.DomainClaim{Host: d[0], TLS: d[1]})
	}
	return r
}

func TestHostsComeFromTheRoutesOfThisGateway(t *testing.T) {
	gw := gatewayOf(t, namedPlatform(t, false))
	got := controller.RoutedHosts([]*projection.RouteView{
		route("kb-a", "kuben-system/kuben", [2]string{"api.acme.com", "auto"}, [2]string{"old.acme.com", "none"}),
		route("kb-b", "other/edge", [2]string{"edge.acme.com", "auto"}),
	}, gw)
	if len(got) != 2 || got[0].Namespace != "kb-a" || got[1].Namespace != "kb-a" {
		t.Fatalf("only routes of Kuben's gateway: %+v", got)
	}
}

func TestEachTLSModeGetsItsListener(t *testing.T) {
	p := namedPlatform(t, false)
	claim := func(host, tls string) render.DomainClaim { return render.DomainClaim{Host: host, TLS: tls} }
	plan := controller.PlanListeners([]controller.HostClaim{
		{Claim: claim("auto.acme.com", "auto"), Namespace: "kb-a"},
		{Claim: claim("plain.acme.com", "none"), Namespace: "kb-a"},
		{Claim: claim("own.acme.com", "acme-cert"), Namespace: "kb-a"},
	}, p)
	plain := byHost(t, plan, "plain.acme.com")
	if plain["name"] != render.PlainListenerName("plain.acme.com") || plain["protocol"] != "HTTP" || plain["port"] != uint16(80) {
		t.Fatalf("plain: %v", plain)
	}
	want := map[string]any{"kind": "Secret", "name": "acme-cert", "namespace": "kb-a"}
	if diff := cmp.Diff(want, path(byHost(t, plan, "own.acme.com"), "tls", "certificateRefs", 0)); diff != "" {
		t.Fatal(diff)
	}
	if path(byHost(t, plan, "auto.acme.com"), "tls", "certificateRefs", 0, "name") != render.HostSecretName("auto.acme.com") {
		t.Fatal("auto")
	}
	if render.SectionFor(claim("plain.acme.com", "none"), p) != render.PlainListenerName("plain.acme.com") ||
		render.SectionFor(claim("own.acme.com", "acme-cert"), p) != render.HostListenerName("own.acme.com") {
		t.Fatal("sections")
	}
	// A wildcard covers generated hosts but never an app's own certificate.
	wild := namedPlatform(t, true)
	generated := "api-shop-prod.apps.example.com"
	if render.SectionFor(claim(generated, "auto"), wild) != render.WildcardListener ||
		render.SectionFor(claim(generated, "acme-cert"), wild) != render.HostListenerName(generated) {
		t.Fatal("wildcard sections")
	}
}

func TestListenersUseTheConfiguredPorts(t *testing.T) {
	p := namedPlatform(t, false)
	p.GatewayPorts = v1alpha1.GatewayPorts{HTTP: 8000, HTTPS: 8443}
	plan := controller.PlanListeners(hosts([2]string{"api.acme.com", "kb-a"}), p)
	var ports []uint16
	for _, l := range plan.Listeners {
		ports = append(ports, l["port"].(uint16))
	}
	if diff := cmp.Diff([]uint16{8000, 8443}, ports); diff != "" {
		t.Fatal(diff)
	}
	if namedPlatform(t, false).GatewayPorts != v1alpha1.DefaultGatewayPorts() || v1alpha1.DefaultGatewayPorts().HTTPS != 443 {
		t.Fatal("defaults")
	}
}

// The gateway reconciler against the fake client.

func configWith(t *testing.T, spec string) *v1alpha1.KubenConfig {
	t.Helper()
	c := &v1alpha1.KubenConfig{ObjectMeta: metav1.ObjectMeta{Name: "kuben", Generation: 2}}
	c.Spec = decode[v1alpha1.KubenConfigSpec](t, spec)
	return c
}

func gatewayConditionOf(t *testing.T, c cluster) v1alpha1.Condition {
	t.Helper()
	config := &v1alpha1.KubenConfig{ObjectMeta: metav1.ObjectMeta{Name: "kuben"}}
	mustGet(t, c, config)
	if config.Status == nil {
		t.Fatal("no status")
	}
	cond, ok := conditionOf(config.Status.Conditions, controller.GatewayCondition)
	if !ok {
		t.Fatalf("no Gateway condition: %+v", config.Status)
	}
	return cond
}

func TestTheGatewayKubenOwnsIsWrittenAndReported(t *testing.T) {
	config := configWith(t, `{ "baseDomain": "apps.example.com", "gatewayClassName": "traefik", "clusterIssuer": "letsencrypt" }`)
	c := newCluster(t, interceptor.Funcs{}, config)
	c.projections.UpsertRoute(*route("kb-a", "kuben-system/kuben", [2]string{"api.acme.com", "auto"}))
	if err := c.rec.Gateway(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw, ok := get(t, c, kind("gateway.networking.k8s.io", "v1", "Gateway"), "kuben-system", "kuben")
	if !ok {
		t.Fatal("the Gateway is created")
	}
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	if len(listeners) != 2 || gw.GetLabels()[v1alpha1.LabelGatewayOwner] != "kuben" ||
		gw.GetAnnotations()[controller.IssuerAnnotation] != "letsencrypt" {
		t.Fatalf("gateway: %v", gw.Object)
	}
	if _, ok := get(t, c, kind("gateway.networking.k8s.io", "v1", "HTTPRoute"), "kuben-system", controller.RedirectRoute); !ok {
		t.Fatal("TLS on: the redirect route is applied")
	}
	cond := gatewayConditionOf(t, c)
	if cond.Status != "False" || str(cond.Reason) != "Pending" || str(cond.Message) != "waiting for the gateway controller" ||
		cond.ObservedGeneration == nil || *cond.ObservedGeneration != 2 {
		t.Fatalf("condition: %+v", cond)
	}

	// The issuer is not Ready: TLS goes off, the redirect route goes.
	c.facts.Publish(discovery.ClusterFacts{
		GatewayAPI:     opt.Some(discovery.GatewayAPI{}),
		ClusterIssuers: []discovery.Readiness{{Name: "letsencrypt", Ready: false}},
		GatewayClasses: []discovery.Readiness{{Name: "traefik", Ready: true}},
	})
	if err := c.rec.Gateway(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, c, kind("gateway.networking.k8s.io", "v1", "HTTPRoute"), "kuben-system", controller.RedirectRoute); ok {
		t.Fatal("TLS off: the redirect route is removed")
	}
	gw, ok = get(t, c, kind("gateway.networking.k8s.io", "v1", "Gateway"), "kuben-system", "kuben")
	if !ok {
		t.Fatal("the Gateway stays")
	}
	if listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners"); len(listeners) != 1 {
		t.Fatalf("plain HTTP only: %v", listeners)
	}
}

func TestAForeignGatewayIsReportedAndNotWritten(t *testing.T) {
	config := configWith(t, `{ "gateway": "edge/shared" }`)
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"name": "shared", "namespace": "edge", "labels": map[string]any{"team": "edge"}},
		"spec":     map[string]any{"gatewayClassName": "cilium"},
	}}
	c := newCluster(t, interceptor.Funcs{}, config, foreign)
	if err := c.rec.Gateway(context.Background()); err != nil {
		t.Fatal(err)
	}
	cond := gatewayConditionOf(t, c)
	if str(cond.Reason) != "GatewayNotOwned" {
		t.Fatalf("condition: %+v", cond)
	}
	live, ok := get(t, c, kind("gateway.networking.k8s.io", "v1", "Gateway"), "edge", "shared")
	if !ok {
		t.Fatal("the foreign Gateway stays")
	}
	if _, has, _ := unstructured.NestedSlice(live.Object, "spec", "listeners"); has {
		t.Fatal("a foreign Gateway is never written")
	}

	// Without the Gateway API nothing is read; with no gateway configured
	// the condition is dropped.
	c.facts.Publish(discovery.ClusterFacts{})
	if err := c.rec.Gateway(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cond := gatewayConditionOf(t, c); str(cond.Reason) != "GatewayAPIMissing" {
		t.Fatalf("condition: %+v", cond)
	}
	config = &v1alpha1.KubenConfig{ObjectMeta: metav1.ObjectMeta{Name: "kuben"}}
	mustGet(t, c, config)
	config.Spec.Gateway = nil
	if err := c.Update(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if err := c.rec.Gateway(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustGet(t, c, config)
	if _, ok := conditionOf(config.Status.Conditions, controller.GatewayCondition); ok {
		t.Fatal("no gateway, no condition")
	}
}

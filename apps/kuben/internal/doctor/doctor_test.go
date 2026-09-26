package doctor_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
)

// optionals lets cmp read opt.Val and netip.Addr.
var optionals = cmp.Options{
	cmp.AllowUnexported(opt.Val[string]{}, opt.Val[bool]{}, opt.Val[render.GatewayRef]{}),
	cmp.Comparer(func(a, b netip.Addr) bool { return a == b }),
}

func addrs(t *testing.T, texts ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(texts))
	for _, text := range texts {
		out = append(out, netip.MustParseAddr(text))
	}
	return out
}

// doctor.rs dns_ports_and_the_agent, its DNS part (the port and agent
// checks are TestDNSPortsAndTheAgent).
func TestDNSChecksAndVerdicts(t *testing.T) {
	gw := addrs(t, "203.0.113.7")
	other := addrs(t, "198.51.100.1")
	tests := []struct {
		name              string
		resolved, gateway []netip.Addr
		verdict           doctor.Verdict
		message           string
		status            doctor.Status
	}{
		{"ok", gw, gw, doctor.VerdictOK, "points at the gateway", doctor.StatusOK},
		{
			"mismatch", other, gw, doctor.VerdictMismatch,
			"points at 198.51.100.1 but the gateway is 203.0.113.7", doctor.StatusFail,
		},
		{
			"unresolved", nil, gw, doctor.VerdictUnresolved,
			"no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway", doctor.StatusFail,
		},
		{
			"unknown", other, nil, doctor.VerdictUnknown,
			"resolves to 198.51.100.1; the gateway reports no address to compare with", doctor.StatusUnknown,
		},
		{
			"one of several", addrs(t, "198.51.100.1", "203.0.113.7"), gw, doctor.VerdictOK,
			"points at the gateway", doctor.StatusOK,
		},
		{
			"every address named", addrs(t, "198.51.100.1", "2001:db8::1"), addrs(t, "203.0.113.7", "203.0.113.8"),
			doctor.VerdictMismatch,
			"points at 198.51.100.1, 2001:db8::1 but the gateway is 203.0.113.7, 203.0.113.8", doctor.StatusFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict, message := doctor.DNSVerdict(tt.resolved, tt.gateway)
			if verdict != tt.verdict || message != tt.message {
				t.Fatalf("DNSVerdict = (%q, %q), want (%q, %q)", verdict, message, tt.verdict, tt.message)
			}
			check := doctor.DNSCheck("a", tt.resolved, tt.gateway)
			want := doctor.Check{ID: "dns", Subject: "a", Status: tt.status, Detail: tt.message}
			if tt.status != doctor.StatusOK {
				want.Hint = opt.Some("point the record at the Gateway's address; DNS may take a while to follow")
			}
			if diff := cmp.Diff(want, check, optionals); diff != "" {
				t.Fatalf("DNSCheck (-want +got):\n%s", diff)
			}
		})
	}
}

// doctor.rs dns_ports_and_the_agent: a check that is ok shows no hint.
func TestAnOKCheckShowsNoHint(t *testing.T) {
	c := doctor.Check{ID: "x", Status: doctor.StatusOK, Detail: "fine"}.WithHint("never shown")
	if c.Hint.IsSome() {
		t.Fatalf("hint: %v", c.Hint)
	}
}

// resolver answers from a table; a host it does not know fails.
type resolver map[string][]string

func (r resolver) LookupHost(_ context.Context, host string) ([]string, error) {
	found, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return found, nil
}

// blocking answers only when its context ends.
type blocking struct{}

func (blocking) LookupHost(ctx context.Context, _ string) ([]string, error) {
	<-ctx.Done()
	return []string{"203.0.113.7"}, ctx.Err()
}

// Resolve sorts as Rust's IpAddr does (IPv4 before IPv6, numerically, not
// as text), drops duplicates and what is not an address, and prints an
// IPv4-mapped IPv6 address as Rust does.
func TestResolveSortsAddressesAsRust(t *testing.T) {
	r := resolver{
		"shop.example.com": {
			"2001:db8::1", "203.0.113.10", "203.0.113.9", "::ffff:198.51.100.1", "not an address",
			"203.0.113.9", "10.0.0.1", "fe80::1%eth0",
		},
	}
	got := doctor.Resolve(t.Context(), r, "shop.example.com")
	texts := make([]string, len(got))
	for i, ip := range got {
		texts[i] = ip.String()
	}
	want := []string{"10.0.0.1", "203.0.113.9", "203.0.113.10", "::ffff:198.51.100.1", "2001:db8::1", "fe80::1"}
	if diff := cmp.Diff(want, texts); diff != "" {
		t.Fatalf("Resolve (-want +got):\n%s", diff)
	}
	if got := doctor.Resolve(t.Context(), r, "unknown.example.com"); len(got) != 0 {
		t.Fatalf("an unknown host has no address: %v", got)
	}
}

// A lookup that does not answer in three seconds has no address.
func TestResolveGivesUpAfterItsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the deadline has passed already
	if got := doctor.Resolve(ctx, blocking{}, "slow.example.com"); len(got) != 0 {
		t.Fatalf("a lookup past its deadline has no address: %v", got)
	}
}

var gatewayGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}

func gateway(status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": "kuben", "namespace": "kuben-system"},
		"status":     status,
	}}
}

func withGateway() render.Platform {
	p := render.DefaultPlatform()
	p.Gateway = opt.Some(render.OwnedDefaultGateway())
	return p
}

// read_gateway: the addresses keep the order and the duplicates of the
// Gateway's status; only a hostname's own addresses are sorted.
func TestReadGatewayKeepsTheStatusOrder(t *testing.T) {
	gw := gateway(map[string]any{
		"addresses": []any{
			map[string]any{"type": "IPAddress", "value": "203.0.113.9"},
			map[string]any{"type": "Hostname", "value": "lb.example.com"},
			map[string]any{"type": "IPAddress", "value": "198.51.100.1"},
			map[string]any{"type": "IPAddress", "value": "203.0.113.9"},
			map[string]any{"type": "Hostname"},
		},
		"conditions": []any{
			map[string]any{"type": "Programmed", "status": "False", "message": "no load balancer"},
		},
	})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gatewayGVR: "GatewayList"})
	// Created rather than seeded: seeding guesses the resource from the
	// kind, and guesses "gatewaies".
	if _, err := client.Resource(gatewayGVR).Namespace("kuben-system").Create(t.Context(), gw, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r := resolver{"lb.example.com": {"2001:db8::2", "192.0.2.5", "192.0.2.4", "192.0.2.5"}}
	state := doctor.ReadGateway(t.Context(), client, withGateway(), r)
	want := doctor.GatewayFound{
		Programmed: opt.Some(false),
		Message:    opt.Some("no load balancer"),
		Addrs: addrs(t, "203.0.113.9", "192.0.2.4", "192.0.2.5", "2001:db8::2", "198.51.100.1",
			"203.0.113.9"),
	}
	if diff := cmp.Diff(doctor.GatewayState(want), state, optionals); diff != "" {
		t.Fatalf("ReadGateway (-want +got):\n%s", diff)
	}
}

func TestReadGatewayWithoutOne(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gatewayGVR: "GatewayList"})
	tests := []struct {
		name     string
		platform render.Platform
		want     doctor.GatewayState
	}{
		{"none configured", render.DefaultPlatform(), doctor.GatewayMissing{}},
		{"not created", withGateway(), doctor.GatewayMissing{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := doctor.ReadGateway(t.Context(), client, tt.platform, resolver{})
			if diff := cmp.Diff(tt.want, state, optionals); diff != "" {
				t.Fatalf("ReadGateway (-want +got):\n%s", diff)
			}
			if len(state.Addresses()) != 0 {
				t.Fatalf("addresses: %v", state.Addresses())
			}
		})
	}
}

// read_platform: the KubenConfig's settings; the defaults without one.
func TestReadPlatform(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "kuben.dev", Version: "v1alpha1", Resource: "kubenconfigs"}
	lists := map[schema.GroupVersionResource]string{gvr: "KubenConfigList"}
	empty := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	if diff := cmp.Diff(render.DefaultPlatform(), doctor.ReadPlatform(t.Context(), empty), optionals); diff != "" {
		t.Fatalf("without a KubenConfig (-want +got):\n%s", diff)
	}
	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kuben.dev/v1alpha1",
		"kind":       "KubenConfig",
		"metadata":   map[string]any{"name": "kuben"},
		"spec":       map[string]any{"baseDomain": "apps.example.com", "gateway": "edge/public"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, config)
	p := doctor.ReadPlatform(t.Context(), client)
	if p.BaseDomain != opt.Some("apps.example.com") ||
		p.Gateway != opt.Some(render.GatewayRef{Namespace: "edge", Name: "public"}) {
		t.Fatalf("platform: %+v", p)
	}
}

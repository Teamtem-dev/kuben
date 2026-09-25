package doctor_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// doctor.rs claims_delegation_and_proxies_are_judged.
func TestClaimsDelegationAndProxiesAreJudged(t *testing.T) {
	statuses := []struct {
		name string
		got  doctor.Check
		want doctor.Status
	}{
		{"ours", doctor.ClaimCheck("a.example.com", doctor.ClaimOurs{Domain: "example.com"}, true), doctor.StatusOK},
		{"others", doctor.ClaimCheck("a.example.com", doctor.ClaimOthers{}, false), doctor.StatusFail},
		{"nobody", doctor.ClaimCheck("a.example.com", doctor.ClaimNobody{}, false), doctor.StatusWarn},
		{"nobody, required", doctor.ClaimCheck("a.example.com", doctor.ClaimNobody{}, true), doctor.StatusFail},
		{"no name servers", doctor.DelegationCheck("a.example.com", "example.com", nil, nil), doctor.StatusFail},
		{
			"unreadable name servers",
			doctor.DelegationCheck("a.example.com", "example.com", nil, errors.New("timeout")), doctor.StatusUnknown,
		},
		{"proxied", doctor.ProxyCheck("a.example.com", opt.Some(true)), doctor.StatusWarn},
		{"no provider", doctor.ProxyCheck("a.example.com", opt.None[bool]()), doctor.StatusUnknown},
		{"not proxied", doctor.ProxyCheck("a.example.com", opt.Some(false)), doctor.StatusOK},
	}
	for _, tt := range statuses {
		if tt.got.Status != tt.want {
			t.Errorf("%s: status %q, want %q", tt.name, tt.got.Status, tt.want)
		}
	}
	ok := doctor.DelegationCheck("a.example.com", "example.com",
		[]string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"}, nil)
	want := doctor.Check{
		ID: "delegation", Subject: "a.example.com", Status: doctor.StatusOK,
		Detail: "example.com is served by ada.ns.cloudflare.com, bob.ns.cloudflare.com (Cloudflare)",
	}
	if diff := cmp.Diff(want, ok, optionals); diff != "" {
		t.Fatalf("delegation (-want +got):\n%s", diff)
	}
	mixed := doctor.DelegationCheck("a.example.com", "example.com", []string{"ada.ns.cloudflare.com", "ns1.example.net"}, nil)
	if strings.Contains(mixed.Detail, "Cloudflare") {
		t.Fatalf("not all Cloudflare: %q", mixed.Detail)
	}
	if unread := doctor.DelegationCheck("a", "example.com", nil, errors.New("timeout")); unread.Detail !=
		"the name servers of example.com could not be read: timeout" {
		t.Fatalf("detail: %q", unread.Detail)
	}
	if !doctor.ProxyCheck("a.example.com", opt.Some(true)).Hint.IsSome() {
		t.Fatal("a proxied record has a hint")
	}
}

func platform(t *testing.T, issuer string) render.Platform {
	t.Helper()
	spec := map[string]any{"gatewayClassName": "traefik"}
	if issuer != "" {
		spec["clusterIssuer"] = issuer
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var s v1alpha1.KubenConfigSpec
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return render.PlatformFromSpec(s)
}

func facts() discovery.ClusterFacts {
	return discovery.ClusterFacts{
		GatewayClasses: []discovery.Readiness{{Name: "traefik", Ready: true}},
		ClusterIssuers: []discovery.Readiness{
			{Name: "letsencrypt", Ready: false, Message: opt.Some("ACME account not registered")},
		},
	}
}

func byID(t *testing.T, checks []doctor.Check, id string) doctor.Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, checks)
	return doctor.Check{}
}

// doctor.rs the_platform_is_judged_from_facts_and_unknown_is_never_ok.
func TestThePlatformIsJudgedFromFactsAndUnknownIsNeverOK(t *testing.T) {
	found := doctor.GatewayFound{Programmed: opt.Some(true), Addrs: addrs(t, "203.0.113.7")}
	checks := doctor.PlatformChecks(opt.Some(facts()), platform(t, "letsencrypt"), found)
	if s := byID(t, checks, "gateway-class").Status; s != doctor.StatusOK {
		t.Fatalf("gateway-class: %q", s)
	}
	gw := byID(t, checks, "gateway")
	if gw.Status != doctor.StatusOK || gw.Detail != "programmed (203.0.113.7)" || gw.Subject != "kuben-system/kuben" {
		t.Fatalf("gateway: %+v", gw)
	}
	issuer := byID(t, checks, "issuer")
	if issuer.Status != doctor.StatusFail || issuer.Detail != "ACME account not registered" || !issuer.Hint.IsSome() {
		t.Fatalf("issuer: %+v", issuer)
	}
	if o := doctor.Overall(checks); o != doctor.StatusFail {
		t.Fatalf("overall: %q", o)
	}

	unknown := doctor.PlatformChecks(opt.None[discovery.ClusterFacts](), platform(t, ""), doctor.GatewayUnreadable{})
	for id, want := range map[string]doctor.Status{
		"gateway-class": doctor.StatusUnknown, "gateway": doctor.StatusUnknown, "issuer": doctor.StatusWarn,
	} {
		if s := byID(t, unknown, id).Status; s != want {
			t.Fatalf("%s: %q, want %q", id, s, want)
		}
	}
	if o := doctor.Overall(unknown); o != doctor.StatusUnknown {
		t.Fatalf("unknown outranks a warning: %q", o)
	}

	none := doctor.PlatformChecks(opt.None[discovery.ClusterFacts](), render.DefaultPlatform(), doctor.GatewayUnreadable{})
	if len(none) != 1 || none[0].Status != doctor.StatusFail {
		t.Fatalf("no gateway: %+v", none)
	}
}

// The Gateway's other states and the facts' other availabilities.
func TestGatewayAndAvailabilityVerdicts(t *testing.T) {
	p := platform(t, "letsencrypt")
	tests := []struct {
		name    string
		facts   discovery.ClusterFacts
		gateway doctor.GatewayState
		id      string
		want    doctor.Check
	}{
		{
			"gateway missing", facts(),
			doctor.GatewayMissing{},
			"gateway",
			doctor.Check{
				ID: "gateway", Subject: "kuben-system/kuben", Status: doctor.StatusFail, Detail: "does not exist",
				Hint: opt.Some("Kuben creates it once a GatewayClass is set; see the Gateway condition of KubenConfig"),
			},
		},
		{
			"gateway not programmed", facts(),
			doctor.GatewayFound{Programmed: opt.Some(false)},
			"gateway",
			doctor.Check{
				ID: "gateway", Subject: "kuben-system/kuben", Status: doctor.StatusFail, Detail: "not programmed",
				Hint: opt.Some("the Gateway controller's log says why"),
			},
		},
		{
			"gateway without controller", facts(),
			doctor.GatewayFound{},
			"gateway",
			doctor.Check{
				ID: "gateway", Subject: "kuben-system/kuben", Status: doctor.StatusUnknown,
				Detail: "no controller has answered for it yet",
			},
		},
		{
			"programmed without address", facts(),
			doctor.GatewayFound{Programmed: opt.Some(true)},
			"gateway",
			doctor.Check{
				ID: "gateway", Subject: "kuben-system/kuben", Status: doctor.StatusOK,
				Detail: "programmed (no address reported)",
			},
		},
		{
			"class missing",
			discovery.ClusterFacts{},
			doctor.GatewayMissing{},
			"gateway-class",
			doctor.Check{
				ID: "gateway-class", Subject: "traefik", Status: doctor.StatusFail, Detail: "does not exist",
				Hint: opt.Some("install the Gateway controller for this class (k3s: Traefik with the Gateway provider; elsewhere e.g. Envoy Gateway)"),
			},
		},
		{
			"issuer not ready, no reason",
			discovery.ClusterFacts{ClusterIssuers: []discovery.Readiness{{Name: "letsencrypt"}}},
			doctor.GatewayMissing{},
			"issuer",
			doctor.Check{
				ID: "issuer", Subject: "letsencrypt", Status: doctor.StatusFail, Detail: "exists but is not ready",
				Hint: opt.Some("kubectl describe clusterissuer shows why (an ACME account, a CA Secret)"),
			},
		},
		{
			"issuer probe failed",
			discovery.ClusterFacts{Unknown: []string{discovery.ProbeClusterIssuers}},
			doctor.GatewayMissing{},
			"issuer",
			doctor.Check{
				ID: "issuer", Subject: "letsencrypt", Status: doctor.StatusUnknown,
				Detail: "the cluster could not be asked yet",
				Hint:   opt.Some("kubectl describe clusterissuer shows why (an ACME account, a CA Secret)"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := byID(t, doctor.PlatformChecks(opt.Some(tt.facts), p, tt.gateway), tt.id)
			if diff := cmp.Diff(tt.want, got, optionals); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
}

// doctor.rs a_route_and_its_certificates_explain_themselves.
func TestARouteAndItsCertificatesExplainThemselves(t *testing.T) {
	host := func(name, tls string, ready opt.Val[bool]) projection.HostExposure {
		h := projection.HostExposure{Host: name, TLS: tls, CertificateReady: ready}
		if r, ok := ready.Get(); ok && !r {
			h.CertificateMessage = opt.Some("challenge pending")
		}
		return h
	}
	exposure := projection.ExposureView{
		Accepted: opt.Some(true),
		Hosts: []projection.HostExposure{
			host("a.example.com", "auto", opt.Some(true)),
			host("b.example.com", "auto", opt.Some(false)),
			host("c.example.com", "none", opt.None[bool]()),
			host("d.example.com", "secret", opt.None[bool]()),
		},
	}
	checks := doctor.ExposureChecks(opt.Some(exposure), true)
	type seen struct {
		ID, Subject string
		Status      doctor.Status
	}
	got := make([]seen, len(checks))
	for i, c := range checks {
		got[i] = seen{c.ID, c.Subject, c.Status}
	}
	want := []seen{
		{"route", "", doctor.StatusOK},
		{"certificate", "a.example.com", doctor.StatusOK},
		{"certificate", "b.example.com", doctor.StatusFail},
		{"certificate", "c.example.com", doctor.StatusWarn},
		{"certificate", "d.example.com", doctor.StatusUnknown},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("checks (-want +got):\n%s", diff)
	}
	if checks[2].Detail != "challenge pending" {
		t.Fatalf("detail: %q", checks[2].Detail)
	}
	if s := doctor.ExposureChecks(opt.None[projection.ExposureView](), true)[0].Status; s != doctor.StatusFail {
		t.Fatalf("no route with hosts: %q", s)
	}
	if s := doctor.ExposureChecks(opt.None[projection.ExposureView](), false)[0].Status; s != doctor.StatusWarn {
		t.Fatalf("no route without hosts: %q", s)
	}
	waiting := exposure
	waiting.Accepted = opt.None[bool]()
	if s := doctor.ExposureChecks(opt.Some(waiting), true)[0].Status; s != doctor.StatusUnknown {
		t.Fatalf("waiting: %q", s)
	}
	refused := exposure
	refused.Accepted = opt.Some(false)
	if c := doctor.ExposureChecks(opt.Some(refused), true)[0]; c.Status != doctor.StatusFail ||
		c.Detail != "refused by the Gateway" {
		t.Fatalf("refused: %+v", c)
	}
}

// doctor.rs dns_ports_and_the_agent: the port, agent and overall parts
// (the DNS part is TestDNSChecksAndVerdicts).
func TestDNSPortsAndTheAgent(t *testing.T) {
	tests := []struct {
		name string
		got  doctor.Check
		id   string
		want doctor.Status
	}{
		{"port 80 reached", doctor.PortCheck(80, doctor.PortReached{}), "port-80", doctor.StatusOK},
		{"port 443 refused", doctor.PortCheck(443, doctor.PortUnreached{Reason: "refused"}), "port-443", doctor.StatusFail},
		{"port 443 no address", doctor.PortCheck(443, doctor.PortNoAddress{}), "port-443", doctor.StatusUnknown},
		{"agent fresh", doctor.AgentCheck(doctor.AgentSeen{Age: opt.Some[uint64](5)}, 30), "agent", doctor.StatusOK},
		{"agent stale", doctor.AgentCheck(doctor.AgentSeen{Age: opt.Some[uint64](300)}, 30), "agent", doctor.StatusFail},
		{"agent never linked", doctor.AgentCheck(doctor.AgentSeen{}, 30), "agent", doctor.StatusFail},
		{"agent revoked", doctor.AgentCheck(doctor.AgentRevoked{}, 30), "agent", doctor.StatusFail},
		{"no agent", doctor.AgentCheck(doctor.AgentNone{}, 30), "agent", doctor.StatusFail},
	}
	for _, tt := range tests {
		if tt.got.ID != tt.id || tt.got.Status != tt.want {
			t.Errorf("%s: (%q, %q), want (%q, %q)", tt.name, tt.got.ID, tt.got.Status, tt.id, tt.want)
		}
	}
	if c := doctor.AgentCheck(doctor.AgentSeen{Age: opt.Some[uint64](30)}, 30); c.Detail != "linked, heard from 30s ago" {
		t.Fatalf("at the limit: %+v", c)
	}
	if c := doctor.PortCheck(80, doctor.PortReached{}); c.Subject != "80" || c.Hint.IsSome() {
		t.Fatalf("port 80: %+v", c)
	}
	if o := doctor.Overall(nil); o != doctor.StatusOK {
		t.Fatalf("overall of none: %q", o)
	}
}

// Overall orders the verdicts ok < warn < unknown < fail.
func TestOverallIsTheWorstVerdict(t *testing.T) {
	check := func(s doctor.Status) doctor.Check { return doctor.Check{Status: s} }
	tests := []struct {
		checks []doctor.Check
		want   doctor.Status
	}{
		{[]doctor.Check{check(doctor.StatusOK)}, doctor.StatusOK},
		{[]doctor.Check{check(doctor.StatusWarn), check(doctor.StatusOK)}, doctor.StatusWarn},
		{[]doctor.Check{check(doctor.StatusWarn), check(doctor.StatusUnknown)}, doctor.StatusUnknown},
		{[]doctor.Check{check(doctor.StatusFail), check(doctor.StatusUnknown)}, doctor.StatusFail},
	}
	for _, tt := range tests {
		if got := doctor.Overall(tt.checks); got != tt.want {
			t.Errorf("Overall(%v) = %q, want %q", tt.checks, got, tt.want)
		}
	}
}

// dialer fails every dial with err.
type dialer struct{ err error }

func (d dialer) DialContext(context.Context, string, string) (net.Conn, error) { return nil, d.err }

// hanging answers only when its context ends.
type hanging struct{}

func (hanging) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// probe_port: the first address only, a failure named `ip:port` as Rust.
func TestProbePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port) //nolint:gosec // a TCP port
	if got := doctor.ProbePort(t.Context(), &net.Dialer{}, addrs(t, "127.0.0.1", "192.0.2.1"), port); got != doctor.PortProbe(doctor.PortReached{}) {
		t.Fatalf("listening: %#v", got)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	refused, ok := doctor.ProbePort(t.Context(), &net.Dialer{}, addrs(t, "127.0.0.1"), port).(doctor.PortUnreached)
	if !ok || !strings.HasPrefix(refused.Reason, "127.0.0.1:") || !strings.HasSuffix(refused.Reason, "connection refused") ||
		strings.Contains(refused.Reason, "dial tcp") {
		t.Fatalf("closed: %#v", refused)
	}
	if got := doctor.ProbePort(t.Context(), &net.Dialer{}, nil, 80); got != doctor.PortProbe(doctor.PortNoAddress{}) {
		t.Fatalf("no address: %#v", got)
	}
	v6 := doctor.ProbePort(t.Context(), dialer{err: errors.New("unreachable")}, addrs(t, "2001:db8::1"), 443)
	if want := (doctor.PortUnreached{Reason: "2001:db8::1:443: unreachable"}); v6 != doctor.PortProbe(want) {
		t.Fatalf("IPv6: %#v", v6)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	late := doctor.ProbePort(ctx, hanging{}, addrs(t, "192.0.2.1"), 443)
	if want := (doctor.PortUnreached{Reason: "192.0.2.1:443: no answer within 3s"}); late != doctor.PortProbe(want) {
		t.Fatalf("no answer: %#v", late)
	}
}

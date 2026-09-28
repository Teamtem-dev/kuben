package outbound_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/outbound"
)

// answers is a resolver whose answers for a host change from one lookup to
// the next: the n-th lookup gets the n-th answer, the last one after.
type answers struct {
	mu      sync.Mutex
	hosts   map[string][][]netip.Addr
	lookups int
}

func (a *answers) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lookups++
	list, ok := a.hosts[host]
	if !ok || len(list) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	next := list[0]
	if len(list) > 1 {
		a.hosts[host] = list[1:]
	}
	return next, nil
}

func resolving(host string, addrs ...[]string) *answers {
	a := &answers{hosts: map[string][][]netip.Addr{}}
	for _, answer := range addrs {
		parsed := make([]netip.Addr, 0, len(answer))
		for _, s := range answer {
			parsed = append(parsed, netip.MustParseAddr(s))
		}
		a.hosts[host] = append(a.hosts[host], parsed)
	}
	return a
}

// dials records where connections were opened and opens none.
type dials struct {
	mu        sync.Mutex
	addresses []string
}

var errNotConnected = errors.New("not connected (test)")

func (d *dials) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addresses = append(d.addresses, address)
	return nil, errNotConnected
}

func (d *dials) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addresses...)
}

func TestTheGuardRefusesHostsThatResolveToPrivateAddresses(t *testing.T) {
	for _, answer := range [][]string{{"10.0.0.7"}, {"203.0.113.10", "127.0.0.1"}, {"169.254.169.254"}, {"::1"}, {"fd00::1"}} {
		d := &dials{}
		g := outbound.Guard{Resolver: resolving("git.example.com", answer), Dialer: d}
		_, err := g.DialContext(t.Context(), "tcp", "git.example.com:443")
		var private outbound.PrivateAddress
		if !errors.As(err, &private) || private.Host != "git.example.com" {
			t.Errorf("%v: got %v", answer, err)
		}
		if len(d.seen()) != 0 {
			t.Errorf("%v: dialed %v", answer, d.seen())
		}
	}
}

func TestTheGuardRefusesPrivateAddressLiterals(t *testing.T) {
	d := &dials{}
	g := outbound.Guard{Resolver: resolving("unused"), Dialer: d}
	for _, address := range []string{"127.0.0.1:5000", "[::1]:443", "10.1.2.3:443", "[::ffff:192.168.1.1]:443"} {
		_, err := g.DialContext(t.Context(), "tcp", address)
		var private outbound.PrivateAddress
		if !errors.As(err, &private) || !strings.Contains(err.Error(), "is a private address") {
			t.Errorf("%s: got %v", address, err)
		}
	}
	if len(d.seen()) != 0 {
		t.Errorf("dialed %v", d.seen())
	}
}

func TestTheGuardConnectsToTheAddressesItChecked(t *testing.T) {
	// The second lookup would rebind the name to a private address; the
	// guard resolves once and connects to what it checked.
	r := resolving("git.example.com", []string{"203.0.113.10", "2001:db8::10"}, []string{"10.0.0.7"})
	d := &dials{}
	g := outbound.Guard{Resolver: r, Dialer: d}
	if _, err := g.DialContext(t.Context(), "tcp", "git.example.com:443"); !errors.Is(err, errNotConnected) {
		t.Fatalf("got %v", err)
	}
	if diff := cmp.Diff([]string{"203.0.113.10:443", "[2001:db8::10]:443"}, d.seen()); diff != "" {
		t.Errorf("dialed (-want +got):\n%s", diff)
	}
	if r.lookups != 1 {
		t.Errorf("%d lookups", r.lookups)
	}
}

func TestTheGuardSaysWhenAHostDoesNotResolve(t *testing.T) {
	g := outbound.Guard{Resolver: resolving("elsewhere.example"), Dialer: &dials{}}
	_, err := g.DialContext(t.Context(), "tcp", "git.example.com:443")
	if err == nil || !strings.Contains(err.Error(), "cannot resolve git.example.com") {
		t.Errorf("got %v", err)
	}
}

func request(t *testing.T, rt http.RoundTripper, target string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func noProxy(*http.Request) (*url.URL, error) { return nil, nil } //nolint:nilnil // no proxy is no error

func TestTransportsConnectThroughTheGuard(t *testing.T) {
	d := &dials{}
	g := outbound.Guard{Resolver: resolving("gitlab.internal", []string{"10.0.0.7"}), Dialer: d}
	err := request(t, outbound.NewTransportWithProxy(false, g, noProxy), "https://gitlab.internal/api/v4/user")
	var private outbound.PrivateAddress
	if !errors.As(err, &private) {
		t.Errorf("guarded: got %v", err)
	}
	if len(d.seen()) != 0 {
		t.Errorf("guarded: dialed %v", d.seen())
	}
}

func TestTheProxyIsDialedAsItIsAndTheTargetChecked(t *testing.T) {
	proxy := func(*http.Request) (*url.URL, error) { return url.Parse("http://10.0.0.3:3128") }
	r := resolving("git.example.com", []string{"203.0.113.10"})
	r.hosts["gitlab.internal"] = [][]netip.Addr{{netip.MustParseAddr("10.0.0.7")}}
	d := &dials{}
	rt := outbound.NewTransportWithProxy(false, outbound.Guard{Resolver: r, Dialer: d}, proxy)
	if err := request(t, rt, "https://git.example.com/api/v1/user"); !errors.Is(err, errNotConnected) {
		t.Fatalf("a public target: got %v", err)
	}
	if diff := cmp.Diff([]string{"10.0.0.3:3128"}, d.seen()); diff != "" {
		t.Errorf("dialed (-want +got):\n%s", diff)
	}
	err := request(t, rt, "https://gitlab.internal/api/v4/user")
	var private outbound.PrivateAddress
	if !errors.As(err, &private) {
		t.Errorf("a private target behind the proxy: got %v", err)
	}
	if len(d.seen()) != 1 {
		t.Errorf("dialed %v", d.seen())
	}
}

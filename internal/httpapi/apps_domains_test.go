package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
)

// tableResolver answers from a table; a host it does not know fails.
type tableResolver map[string][]string

func (r tableResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	found, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return found, nil
}

// The checks of an app's hosts through an injected resolver: each host's
// addresses sorted as Rust's IpAddr (IPv4 first, numerically) without
// duplicates, the Gateway's addresses as given, the hosts in their order.
func TestCheckHostsSortsAndJudges(t *testing.T) {
	gateway := []netip.Addr{netip.MustParseAddr("203.0.113.9"), netip.MustParseAddr("192.0.2.1")}
	r := tableResolver{
		"shop.example.com": {"2001:db8::1", "203.0.113.10", "203.0.113.9", "203.0.113.9"},
		"api.example.com":  {"198.51.100.20", "198.51.100.3"},
	}
	checks, err := httpapi.CheckHosts(t.Context(), r, []string{"shop.example.com", "api.example.com", "gone.example.com"}, gateway)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"203.0.113.9", "192.0.2.1"}
	want := []gen.DomainCheck{
		{
			Host: "shop.example.com", Addresses: []string{"203.0.113.9", "203.0.113.10", "2001:db8::1"},
			Expected: expected, Status: "ok", Message: "points at the gateway",
		},
		{
			Host: "api.example.com", Addresses: []string{"198.51.100.3", "198.51.100.20"}, Expected: expected,
			Status: "mismatch", Message: "points at 198.51.100.3, 198.51.100.20 but the gateway is 203.0.113.9, 192.0.2.1",
		},
		{
			Host: "gone.example.com", Addresses: []string{}, Expected: expected, Status: "unresolved",
			Message: "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway",
		},
	}
	if diff := cmp.Diff(want, checks); diff != "" {
		t.Fatalf("checks (-want +got):\n%s", diff)
	}

	unknown, err := httpapi.CheckHosts(t.Context(), r, []string{"api.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantUnknown := []gen.DomainCheck{{
		Host: "api.example.com", Addresses: []string{"198.51.100.3", "198.51.100.20"}, Expected: []string{},
		Status: "unknown", Message: "resolves to 198.51.100.3, 198.51.100.20; the gateway reports no address to compare with",
	}}
	if diff := cmp.Diff(wantUnknown, unknown); diff != "" {
		t.Fatalf("without gateway addresses (-want +got):\n%s", diff)
	}
}

// Without a Kubernetes cluster configured, checkAppDomains answers 503.
func TestCheckAppDomainsWithoutClusterAnswers503(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)
	status, body, _ := alice.do("GET", "/api/v1/projects/shop/environments/prod/apps/api/domains", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without cluster, got %d: %v", status, body)
	}
	if detail, _ := body["detail"].(string); detail != "unavailable: no kubernetes cluster configured" {
		t.Fatalf("detail: %v", body)
	}
}

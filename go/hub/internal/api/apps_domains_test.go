package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
)

func TestDNSVerdict(t *testing.T) {
	gw := []string{"203.0.113.7"}
	other := []string{"198.51.100.1"}

	status, msg := api.DNSVerdict(gw, gw)
	assert.Equal(t, "ok", status)
	assert.Equal(t, "points at the gateway", msg)

	status, msg = api.DNSVerdict(other, gw)
	assert.Equal(t, "mismatch", status)
	assert.Equal(t, "points at 198.51.100.1 but the gateway is 203.0.113.7", msg)

	status, msg = api.DNSVerdict(nil, gw)
	assert.Equal(t, "unresolved", status)
	assert.Equal(t, "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway", msg)

	status, msg = api.DNSVerdict(other, nil)
	assert.Equal(t, "unknown", status)
	assert.Equal(t, "resolves to 198.51.100.1; the gateway reports no address to compare with", msg)

	multiple := []string{"198.51.100.1", "203.0.113.7"}
	status, msg = api.DNSVerdict(multiple, gw)
	assert.Equal(t, "ok", status)
	assert.Equal(t, "points at the gateway", msg)
}

type mockDNSResolver map[string][]string

func (m mockDNSResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	return m[host], nil
}

func TestDNSResolverInjectable(t *testing.T) {
	resolver := mockDNSResolver{
		"api.example.com": {"203.0.113.7"},
	}
	res, err := resolver.LookupHost(context.Background(), "api.example.com")
	assert.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.7"}, res)
}

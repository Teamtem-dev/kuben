package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/outbound"
)

func TestOrganizationsCannotReachPrivateAddressesByDefault(t *testing.T) {
	git, registries := httpapi.IntegrationTransports(bareServer(t, nil))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1/api/v4/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = git.RoundTrip(req) //nolint:bodyclose // refused before any answer
	var private outbound.PrivateAddress
	if !errors.As(err, &private) {
		t.Errorf("a Git provider on a loopback address: %v", err)
	}
	// The cloud metadata address, over HTTPS (go-containerregistry uses
	// plain HTTP, which a check refuses anyway, for RFC 1918 addresses).
	err = registries.CheckLogin(t.Context(), "169.254.169.254", oci.Login{Username: "u", Password: "p"})
	if err == nil || !oci.IsUnreachable(err) || !strings.Contains(err.Error(), "private address") {
		t.Errorf("a registry on a link-local address: %v", err)
	}
}

func TestOperatorsMayAllowPrivateAddresses(t *testing.T) {
	git, _ := httpapi.IntegrationTransports(bareServer(t, func(c *config.Config) {
		c.Integrations.AllowPrivateHosts = true
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1:1/api/v4/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = git.RoundTrip(req) //nolint:bodyclose // nothing listens there
	var private outbound.PrivateAddress
	if err == nil || errors.As(err, &private) {
		t.Errorf("allowed: %v", err)
	}
}

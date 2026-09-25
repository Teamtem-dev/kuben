package api_test

import (
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

func valid(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

func invalid(t *testing.T, what string, err error) {
	t.Helper()
	if kerr.CodeOf(err) != kerr.Validation {
		t.Errorf("%s: %v", what, err)
	}
}

// validate.rs labels.
func TestLabels(t *testing.T) {
	valid(t, "shop-2", api.DNSLabel("n", "shop-2", 40))
	for _, bad := range []string{"", "Shop", "-a", "a-", "a_b", strings.Repeat("a", 41)} {
		invalid(t, bad, api.DNSLabel("n", bad, 40))
	}
}

// validate.rs hostnames.
func TestHostnames(t *testing.T) {
	valid(t, "api.example.com", api.Hostname("api.example.com"))
	valid(t, "API.Example.com", api.Hostname("API.Example.com"))
	for _, bad := range []string{"localhost", "a..b", "-a.com", "a.com/x"} {
		invalid(t, bad, api.Hostname(bad))
	}
}

// validate.rs env_names_images_keys_quantities.
func TestEnvNamesImagesKeysQuantities(t *testing.T) {
	valid(t, "DATABASE_URL", api.EnvVarName("DATABASE_URL"))
	valid(t, "_X1", api.EnvVarName("_X1"))
	invalid(t, "1X", api.EnvVarName("1X"))
	invalid(t, "A-B", api.EnvVarName("A-B"))
	valid(t, "digest", api.Image("ghcr.io/acme/api@sha256:abc"))
	for _, bad := range []string{"nginx latest", "", "--help", "nginx\x7f"} {
		invalid(t, bad, api.Image(bad))
	}
	valid(t, "tls.crt", api.SecretKey("tls.crt"))
	invalid(t, "a/b", api.SecretKey("a/b"))
	valid(t, "500m", api.Quantity("cpu", "500m"))
	valid(t, "4Gi", api.Quantity("memory", "4Gi"))
	invalid(t, "lots", api.Quantity("cpu", "lots"))
}

// validate.rs emails_and_time_zones (the email half is also pinned in
// members_test.go).
func TestEmailsAndTimeZones(t *testing.T) {
	valid(t, "carol", api.ValidEmail("carol@example.com"))
	for _, bad := range []string{"carol", "@x.io", "a@b", "a b@c.io", "a@b@c.io"} {
		invalid(t, bad, api.ValidEmail(bad))
	}
	valid(t, "Europe/Berlin", api.TimeZone("Europe/Berlin"))
	valid(t, "UTC", api.TimeZone("UTC"))
	invalid(t, "Europe Berlin", api.TimeZone("Europe Berlin"))
	invalid(t, "empty", api.TimeZone(""))
}

package httpapi_test

import (
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
)

func valid(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

func invalid(t *testing.T, what string, err error) {
	t.Helper()
	if kerrors.CodeOf(err) != kerrors.Validation {
		t.Errorf("%s: %v", what, err)
	}
}

// validate.rs labels.
func TestLabels(t *testing.T) {
	valid(t, "shop-2", httpapi.DNSLabel("n", "shop-2", 40))
	for _, bad := range []string{"", "Shop", "-a", "a-", "a_b", strings.Repeat("a", 41)} {
		invalid(t, bad, httpapi.DNSLabel("n", bad, 40))
	}
}

// validate.rs hostnames.
func TestHostnames(t *testing.T) {
	valid(t, "api.example.com", httpapi.Hostname("api.example.com"))
	valid(t, "API.Example.com", httpapi.Hostname("API.Example.com"))
	for _, bad := range []string{"localhost", "a..b", "-a.com", "a.com/x"} {
		invalid(t, bad, httpapi.Hostname(bad))
	}
}

// validate.rs env_names_images_keys_quantities.
func TestEnvNamesImagesKeysQuantities(t *testing.T) {
	valid(t, "DATABASE_URL", httpapi.EnvVarName("DATABASE_URL"))
	valid(t, "_X1", httpapi.EnvVarName("_X1"))
	invalid(t, "1X", httpapi.EnvVarName("1X"))
	invalid(t, "A-B", httpapi.EnvVarName("A-B"))
	valid(t, "digest", httpapi.Image("ghcr.io/acme/api@sha256:abc"))
	for _, bad := range []string{"nginx latest", "", "--help", "nginx\x7f"} {
		invalid(t, bad, httpapi.Image(bad))
	}
	valid(t, "tls.crt", httpapi.SecretKey("tls.crt"))
	invalid(t, "a/b", httpapi.SecretKey("a/b"))
	valid(t, "500m", httpapi.Quantity("cpu", "500m"))
	valid(t, "4Gi", httpapi.Quantity("memory", "4Gi"))
	invalid(t, "lots", httpapi.Quantity("cpu", "lots"))
}

// validate.rs emails_and_time_zones (the email half is also pinned in
// members_test.go).
func TestEmailsAndTimeZones(t *testing.T) {
	valid(t, "carol", httpapi.ValidEmail("carol@example.com"))
	for _, bad := range []string{"carol", "@x.io", "a@b", "a b@c.io", "a@b@c.io"} {
		invalid(t, bad, httpapi.ValidEmail(bad))
	}
	valid(t, "Europe/Berlin", httpapi.TimeZone("Europe/Berlin"))
	valid(t, "UTC", httpapi.TimeZone("UTC"))
	invalid(t, "Europe Berlin", httpapi.TimeZone("Europe Berlin"))
	invalid(t, "empty", httpapi.TimeZone(""))
}

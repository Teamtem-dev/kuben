package build_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

func TestFetchTokensNeverPrintTheirToken(t *testing.T) {
	token := build.FetchToken{Token: "ghs_secret", ExpiresAt: 1_893_456_000_000}
	data, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	shown := []string{
		fmt.Sprint(token), fmt.Sprintf("%v", token), fmt.Sprintf("%+v", token), fmt.Sprintf("%#v", token),
		fmt.Sprintf("%s", token), fmt.Sprintf("%q", token), fmt.Sprintf("%x", token), fmt.Sprintf("%d", token),
		fmt.Sprintf("%v", &token), fmt.Sprintf("%+v", []build.FetchToken{token}), string(data),
	}
	for _, s := range shown {
		if strings.Contains(s, "ghs_secret") || strings.Contains(s, fmt.Sprintf("%x", "ghs_secret")) {
			t.Errorf("the token is shown: %s", s)
		}
	}
	if want := "FetchToken{Token: <redacted>, ExpiresAt: 1893456000000}"; token.String() != want {
		t.Errorf("String() = %q, want %q", token.String(), want)
	}
}

func TestErrorsReadAsInRust(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{build.NotFound{What: "acme/shop@main"}, "not found: acme/shop@main"},
		{build.Refused{Reason: "installation 7: HTTP 403 Forbidden"}, "refused: installation 7: HTTP 403 Forbidden"},
		{build.Unavailable{Reason: "timed out"}, "unavailable: timed out"},
		{build.ManifestMissing{What: "ghcr.io/acme/web@sha256:ab"}, "the registry has no manifest ghcr.io/acme/web@sha256:ab"},
		{build.RegistryUnavailable{Reason: "HTTP 502 Bad Gateway"}, "the registry is unavailable: HTTP 502 Bad Gateway"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("%#v: %q, want %q", c.err, got, c.want)
		}
	}
}

func TestProviderErrorsAreFoundThroughWrapping(t *testing.T) {
	err := fmt.Errorf("reading the head: %w", build.Refused{Reason: "suspended"})
	var pe build.ProviderError
	if !errors.As(err, &pe) {
		t.Fatal("not a ProviderError")
	}
	var refused build.Refused
	if !errors.As(err, &refused) || refused.Reason != "suspended" {
		t.Errorf("variant %#v", refused)
	}
	var ve build.VerifyError
	if errors.As(fmt.Errorf("x: %w", build.ManifestMissing{What: "m"}), &ve) != true {
		t.Error("not a VerifyError")
	}
}

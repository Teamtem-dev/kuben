package oci_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
)

const digestText = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func mustDigest(t *testing.T, s string) artifact.Digest {
	t.Helper()
	d, err := artifact.ParseDigest(s)
	if err != nil {
		t.Fatalf("digest %q: %v", s, err)
	}
	return d
}

// digestOpt compares artifact.Digest (unexported field) by its text.
var digestOpt = cmp.Comparer(func(a, b artifact.Digest) bool { return a == b })

// Rust: references_are_normalized_like_docker.
func TestReferencesAreNormalizedLikeDocker(t *testing.T) {
	cases := []struct {
		image, registry, path string
		reference             oci.Reference
	}{
		{"nginx", "docker.io", "library/nginx", oci.Tag{Name: "latest"}},
		{"nginx:1.27", "docker.io", "library/nginx", oci.Tag{Name: "1.27"}},
		{"nginxinc/nginx-unprivileged:1.27-alpine", "docker.io", "nginxinc/nginx-unprivileged", oci.Tag{Name: "1.27-alpine"}},
		{"ghcr.io/acme/web:v2", "ghcr.io", "acme/web", oci.Tag{Name: "v2"}},
		{"registry.example.com:5000/team/api", "registry.example.com:5000", "team/api", oci.Tag{Name: "latest"}},
		{"localhost/web:dev", "localhost", "web", oci.Tag{Name: "dev"}},
		// Beyond the Rust cases: the edges of the grammar.
		{"docker.io/nginx", "docker.io", "library/nginx", oci.Tag{Name: "latest"}},
		{"localhost:5000/web", "localhost:5000", "web", oci.Tag{Name: "latest"}},
		{"nginx:V1.2_rc-1", "docker.io", "library/nginx", oci.Tag{Name: "V1.2_rc-1"}},
		{"nginx:" + strings.Repeat("x", 128), "docker.io", "library/nginx", oci.Tag{Name: strings.Repeat("x", 128)}},
	}
	for _, c := range cases {
		got, err := oci.Parse(c.image)
		if err != nil {
			t.Errorf("%s: %v", c.image, err)
			continue
		}
		want := oci.ImageRef{Registry: c.registry, Path: c.path, Reference: c.reference}
		if diff := cmp.Diff(want, got, digestOpt); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.image, diff)
		}
	}
	pinned, err := oci.Parse("ghcr.io/acme/web:v2@" + digestText)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(oci.Reference(oci.Digest{Digest: mustDigest(t, digestText)}), pinned.Reference, digestOpt); diff != "" {
		t.Errorf("pinned (-want +got):\n%s", diff)
	}
	if got := pinned.Repository(); got != "ghcr.io/acme/web" {
		t.Errorf("repository %q", got)
	}
}

// Rust: malformed_references_are_refused.
func TestMalformedReferencesAreRefused(t *testing.T) {
	for _, bad := range []string{
		"",
		"nginx latest",
		"Nginx",
		"nginx:",
		"nginx@latest",
		"ghcr.io/",
		"a//b",
		"nginx:" + strings.Repeat("x", 129),
		// Beyond the Rust cases.
		"nginx:1.0+build",
		"nginx@sha256:0123",
		"/nginx",
		"nginx:tag@" + digestText + "x",
	} {
		_, err := oci.Parse(bad)
		var invalid oci.Invalid
		if !errors.As(err, &invalid) || invalid.Image != bad {
			t.Errorf("%q: got %v, want Invalid", bad, err)
		}
	}
}

func TestErrorsReadAsInRust(t *testing.T) {
	cases := []struct {
		err  oci.ResolveError
		want string
	}{
		{oci.Invalid{Image: "x y"}, "`x y` is not an image reference"},
		{oci.NotFound{Image: "redis:7"}, "the registry has no image `redis:7`"},
		{oci.Unauthorized{Image: "ghcr.io/a/b:1"}, "the registry of `ghcr.io/a/b:1` refuses the pull: add a login for it to the environment's registries, or give repository@sha256:… instead"},
		{oci.Unreachable{Image: "nginx", Reason: "HTTP 503 Service Unavailable"}, "cannot reach the registry of `nginx`: HTTP 503 Service Unavailable"},
		{oci.RateLimited{Image: "nginx", RetryAfter: 7}, "the registry of `nginx` limits requests; retry in 7s"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
		wrapped := fmt.Errorf("resolving: %w", c.err)
		unreachable := errors.As(c.err, new(oci.Unreachable))
		if got := oci.IsUnreachable(wrapped); got != unreachable {
			t.Errorf("IsUnreachable(%T) = %v", c.err, got)
		}
		var re oci.ResolveError
		if !errors.As(wrapped, &re) || !errors.Is(wrapped, c.err) {
			t.Errorf("errors.As(%T) found %v", c.err, re)
		}
	}
	if oci.IsUnreachable(errors.New("other")) || oci.IsUnreachable(nil) {
		t.Error("IsUnreachable of a foreign error")
	}
}

func TestLoginsNeverPrintThePassword(t *testing.T) {
	l := oci.Login{Username: "bot", Password: "s3cret"}
	if got, want := l.Basic(), "Basic Ym90OnMzY3JldA=="; got != want {
		t.Errorf("Basic %q, want %q", got, want)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		for _, arg := range []any{l, &l, struct{ L oci.Login }{l}} {
			if got := fmt.Sprintf(verb, arg); strings.Contains(got, "s3cret") || !strings.Contains(got, "bot") {
				t.Errorf("%s of %T: %q", verb, arg, got)
			}
		}
	}
}

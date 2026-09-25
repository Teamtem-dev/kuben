package oci_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/oci"
)

// noNetwork fails the test on any request.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("unexpected request %s %s", r.Method, r.URL)
	return nil, errors.New("no network in this test")
}

// Rust: pinned_references_and_fixed_answers_need_no_registry.
func TestPinnedReferencesAndFixedAnswersNeedNoRegistry(t *testing.T) {
	ctx := t.Context()
	none := opt.None[oci.Login]()
	image := "ghcr.io/acme/web@" + digestText
	resolved, err := oci.NewRegistryWith(noNetwork{t}).ResolveAs(ctx, image, none)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Pinned(); got != image {
		t.Errorf("pinned %q", got)
	}
	// The zero Registry needs no network for a digest either.
	_, err = (oci.Registry{}).ResolveAs(ctx, image, opt.Some(oci.Login{Username: "u", Password: "p"}))
	if err != nil {
		t.Error(err)
	}

	fixed := oci.Fixed{"nginx:1.27": mustDigest(t, digestText)}
	nginx, err := fixed.ResolveAs(ctx, "nginx:1.27", none)
	if err != nil {
		t.Fatal(err)
	}
	want := oci.Resolved{Repository: "docker.io/library/nginx", Digest: mustDigest(t, digestText), Given: "nginx:1.27"}
	if diff := cmp.Diff(want, nginx, digestOpt); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got := nginx.Pinned(); got != "docker.io/library/nginx@"+digestText {
		t.Errorf("pinned %q", got)
	}
	_, err = fixed.ResolveAs(ctx, "redis:7", none)
	if !errors.Is(err, oci.NotFound{Image: "redis:7"}) {
		t.Errorf("redis:7: %v", err)
	}
}

func TestFixedAnswersKnowEverySpellingOfAnImage(t *testing.T) {
	ctx := t.Context()
	none := opt.None[oci.Login]()
	other := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixed := oci.Fixed{
		"docker.io/library/nginx:1.27": mustDigest(t, digestText),
		"nginx:1.26":                   mustDigest(t, other),
		"nginx@" + other:               mustDigest(t, other),
		"ghcr.io/acme/web:v1":          mustDigest(t, other),
		"not an image":                 mustDigest(t, other),
	}
	for _, spelling := range []string{"nginx:1.27", "docker.io/nginx:1.27", "docker.io/library/nginx:1.27"} {
		got, err := fixed.ResolveAs(ctx, spelling, none)
		if err != nil {
			t.Errorf("%s: %v", spelling, err)
			continue
		}
		want := oci.Resolved{Repository: "docker.io/library/nginx", Digest: mustDigest(t, digestText), Given: spelling}
		if diff := cmp.Diff(want, got, digestOpt); diff != "" {
			t.Errorf("%s (-want +got):\n%s", spelling, diff)
		}
	}
	if _, err := fixed.ResolveAs(ctx, "Nginx", none); !errors.As(err, new(oci.Invalid)) {
		t.Errorf("an invalid reference: %v", err)
	}
	tags, err := fixed.ListTags(ctx, "docker.io/library/nginx", none)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"1.27", "1.26"}, tags); diff != "" {
		t.Errorf("tags (-want +got):\n%s", diff)
	}
	tags, err = oci.Fixed(nil).ListTags(ctx, "redis", none)
	if err != nil || tags == nil || len(tags) != 0 {
		t.Errorf("no tags: %v %v", tags, err)
	}
	if _, err := fixed.ListTags(ctx, "a//b", none); !errors.As(err, new(oci.Invalid)) {
		t.Errorf("an invalid repository: %v", err)
	}
	var _ oci.Resolver = fixed
	var _ oci.Resolver = oci.Registry{}
}

func TestLoginsNeverTravelAsJSON(t *testing.T) {
	data, err := json.Marshal(oci.Login{Username: "bot", Password: "s3cret"})
	if err != nil || string(data) != `{"username":"bot"}` {
		t.Fatalf("%s %v", data, err)
	}
	if !oci.IsRateLimited(oci.RateLimited{Image: "x", RetryAfter: 7}) || oci.IsRateLimited(oci.NotFound{Image: "x"}) {
		t.Fatal("IsRateLimited")
	}
}

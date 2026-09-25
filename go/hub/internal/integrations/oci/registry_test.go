package oci_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oci"
)

// host is the registry name the tests use; the test transport reaches the
// test server whatever the name.
const host = "example.com"

var bot = oci.Login{Username: "bot", Password: "s3cret"}

// serve runs handler on httptest's in-memory network (no socket: the
// sandbox and CI alike) and returns a transport that sends every request,
// whatever its host, to it over TLS.
func serve(t *testing.T, handler http.Handler) http.RoundTripper {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	// Connections cut at Close are logged as TLS handshake errors: noise.
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	return srv.Client().Transport
}

// newRegistry is go-containerregistry's in-memory registry, quiet.
func newRegistry() http.Handler {
	return registry.New(registry.Logger(slog.NewLogLogger(slog.DiscardHandler, slog.LevelInfo)))
}

// push writes a random image as ref and returns its digest.
func push(t *testing.T, tr http.RoundTripper, ref string, auth authn.Authenticator) artifact.Digest {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	err = remote.Write(tag, img, remote.WithTransport(tr), remote.WithAuth(auth), remote.WithContext(t.Context()))
	if err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return mustDigest(t, d.String())
}

func TestTagsResolveToThePushedDigest(t *testing.T) {
	tr := serve(t, newRegistry())
	want := push(t, tr, host+"/acme/web:v1", authn.Anonymous)
	got, err := oci.NewRegistryWith(tr).ResolveAs(t.Context(), host+"/acme/web:v1", opt.None[oci.Login]())
	if err != nil {
		t.Fatal(err)
	}
	expected := oci.Resolved{Repository: host + "/acme/web", Digest: want, Given: host + "/acme/web:v1"}
	if diff := cmp.Diff(expected, got, digestOpt); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// stripDigest drops Docker-Content-Digest from HEAD answers and counts the
// manifest GETs.
type stripDigest struct {
	next http.Handler
	gets *atomic.Int32
}

func (s stripDigest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
		s.gets.Add(1)
	}
	if r.Method == http.MethodHead {
		w = headerless{w}
	}
	s.next.ServeHTTP(w, r)
}

type headerless struct{ http.ResponseWriter }

func (h headerless) WriteHeader(code int) {
	h.Header().Del("Docker-Content-Digest")
	h.ResponseWriter.WriteHeader(code)
}

func TestARegistryWithoutDigestHeadersIsAskedWithGet(t *testing.T) {
	var gets atomic.Int32
	reg := newRegistry()
	tr := serve(t, stripDigest{next: reg, gets: &gets})
	want := push(t, tr, host+"/acme/web:v1", authn.Anonymous)
	gets.Store(0)
	got, err := oci.NewRegistryWith(tr).ResolveAs(t.Context(), host+"/acme/web:v1", opt.None[oci.Login]())
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != want {
		t.Errorf("digest %s, want %s", got.Digest, want)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("%d manifest GETs, want 1", n)
	}
}

func TestUnknownTagsAndRepositoriesAreNotFound(t *testing.T) {
	tr := serve(t, newRegistry())
	push(t, tr, host+"/acme/web:v1", authn.Anonymous)
	r := oci.NewRegistryWith(tr)
	for _, image := range []string{host + "/acme/web:v9", host + "/acme/nothing:v1"} {
		_, err := r.ResolveAs(t.Context(), image, opt.None[oci.Login]())
		if !errors.Is(err, oci.NotFound{Image: image}) {
			t.Errorf("%s: %v", image, err)
		}
	}
	_, err := r.ListTags(t.Context(), host+"/acme/nothing", opt.None[oci.Login]())
	if !errors.Is(err, oci.NotFound{Image: host + "/acme/nothing"}) {
		t.Errorf("listing: %v", err)
	}
}

func TestRateLimitedRegistriesSayWhenToRetry(t *testing.T) {
	var requests atomic.Int32
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	r := oci.NewRegistryWith(tr)
	image := host + "/acme/web:v1"
	_, err := r.ResolveAs(t.Context(), image, opt.None[oci.Login]())
	if !errors.Is(err, oci.RateLimited{Image: image, RetryAfter: 7}) {
		t.Errorf("resolve: %v", err)
	}
	if oci.IsUnreachable(err) {
		t.Error("a rate limit is not unreachable")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests: a 429 must not be retried", n)
	}
	_, err = r.ListTags(t.Context(), host+"/acme/web", opt.None[oci.Login]())
	if !errors.Is(err, oci.RateLimited{Image: host + "/acme/web", RetryAfter: 7}) {
		t.Errorf("list: %v", err)
	}
}

func TestFailingRegistriesAreUnreachable(t *testing.T) {
	var requests atomic.Int32
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	image := host + "/acme/web:v1"
	_, err := oci.NewRegistryWith(tr).ResolveAs(t.Context(), image, opt.None[oci.Login]())
	if !errors.Is(err, oci.Unreachable{Image: image, Reason: "HTTP 503 Service Unavailable"}) || !oci.IsUnreachable(err) {
		t.Errorf("resolve: %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests: a 503 must not be retried", n)
	}
}

// basicAuth lets through requests that carry the login's Basic value.
func basicAuth(next http.Handler, login oci.Login) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != login.Basic() {
			w.Header().Set("WWW-Authenticate", `Basic realm="kuben-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestBasicRegistriesNeedTheLogin(t *testing.T) {
	tr := serve(t, basicAuth(newRegistry(), bot))
	want := push(t, tr, host+"/acme/private:v1", &authn.Basic{Username: bot.Username, Password: bot.Password})
	r := oci.NewRegistryWith(tr)
	image := host + "/acme/private:v1"
	for _, login := range []opt.Val[oci.Login]{opt.None[oci.Login](), opt.Some(oci.Login{Username: "bot", Password: "wrong"})} {
		_, err := r.ResolveAs(t.Context(), image, login)
		if !errors.Is(err, oci.Unauthorized{Image: image}) {
			t.Errorf("%v: %v", login, err)
		}
	}
	got, err := r.ResolveAs(t.Context(), image, opt.Some(bot))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != want {
		t.Errorf("digest %s, want %s", got.Digest, want)
	}
	tags, err := r.ListTags(t.Context(), host+"/acme/private", opt.Some(bot))
	if err != nil || !cmp.Equal(tags, []string{"v1"}) {
		t.Errorf("tags %v, %v", tags, err)
	}
}

// bearerAuth is a registry behind a token service at /token that hands out
// "tok" for the login's Basic value.
func bearerAuth(next http.Handler, login oci.Login) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.Header.Get("Authorization") != login.Basic() || r.URL.Query().Get("service") != "kuben-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+host+`/token",service="kuben-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestBearerRegistriesExchangeTheLoginForAToken(t *testing.T) {
	tr := serve(t, bearerAuth(newRegistry(), bot))
	want := push(t, tr, host+"/acme/private:v1", &authn.Basic{Username: bot.Username, Password: bot.Password})
	r := oci.NewRegistryWith(tr)
	image := host + "/acme/private:v1"
	got, err := r.ResolveAs(t.Context(), image, opt.Some(bot))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != want {
		t.Errorf("digest %s, want %s", got.Digest, want)
	}
	// The token service refuses the anonymous pull.
	_, err = r.ResolveAs(t.Context(), image, opt.None[oci.Login]())
	if !errors.Is(err, oci.Unauthorized{Image: image}) {
		t.Errorf("anonymous: %v", err)
	}
}

func TestUnusableChallengesAreUnauthorized(t *testing.T) {
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer service="no-realm"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	image := host + "/acme/web:v1"
	_, err := oci.NewRegistryWith(tr).ResolveAs(t.Context(), image, opt.None[oci.Login]())
	if !errors.Is(err, oci.Unauthorized{Image: image}) {
		t.Errorf("got %v", err)
	}
}

func TestTagsAreListed(t *testing.T) {
	tr := serve(t, newRegistry())
	for _, tag := range []string{"v2", "latest", "v1"} {
		push(t, tr, host+"/acme/web:"+tag, authn.Anonymous)
	}
	tags, err := oci.NewRegistryWith(tr).ListTags(t.Context(), host+"/acme/web", opt.None[oci.Login]())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"latest", "v1", "v2"}, tags); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// endlessTags answers every tag page with size tags and a Link to the next
// page, forever; it counts the pages.
func endlessTags(t *testing.T, size int, pages *atomic.Int32) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		if r.URL.Path != "/v2/acme/web/tags/list" || r.URL.Query().Get("n") != "1000" {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		page := pages.Add(1)
		tags := make([]string, size)
		for i := range tags {
			tags[i] = fmt.Sprintf(`"p%03d-%04d"`, page, i)
		}
		w.Header().Set("Link", fmt.Sprintf(`</v2/acme/web/tags/list?n=1000&last=p%03d>; rel="next"`, page))
		_, _ = fmt.Fprintf(w, `{"name":"acme/web","tags":[%s]}`, strings.Join(tags, ","))
	})
}

// Rust: tag_pages_follow_link_headers (the Link half; the Link header is now
// read by go-containerregistry, so the pages are checked end to end).
func TestTagPagesFollowLinkHeaders(t *testing.T) {
	cases := []struct {
		pageSize, wantPages, wantTags int
	}{
		{1000, 10, oci.MaxTags}, // capped by MaxTags
		{100, 20, 2000},         // capped by the 20 pages
	}
	for _, c := range cases {
		var pages atomic.Int32
		tr := serve(t, endlessTags(t, c.pageSize, &pages))
		tags, err := oci.NewRegistryWith(tr).ListTags(t.Context(), host+"/acme/web", opt.None[oci.Login]())
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != c.wantTags || int(pages.Load()) != c.wantPages {
			t.Errorf("page size %d: %d tags in %d pages, want %d in %d", c.pageSize, len(tags), pages.Load(), c.wantTags, c.wantPages)
		}
		if len(tags) > 0 && (tags[0] != "p001-0000" || tags[len(tags)-1] != fmt.Sprintf("p%03d-%04d", c.wantPages, c.pageSize-1)) {
			t.Errorf("page size %d: tags %s … %s", c.pageSize, tags[0], tags[len(tags)-1])
		}
	}
}

func TestOversizedAnswersAreRefused(t *testing.T) {
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		_, _ = fmt.Fprintf(w, `{"name":"acme/web","tags":["%s"]}`, strings.Repeat("x", 5<<20))
	}))
	_, err := oci.NewRegistryWith(tr).ListTags(t.Context(), host+"/acme/web", opt.None[oci.Login]())
	if !oci.IsUnreachable(err) || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("got %v", err)
	}
}

// tlsFails is a transport to a registry that speaks plain HTTP only.
type tlsFails struct{ inner http.RoundTripper }

func (f tlsFails) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		return nil, errors.New("tls: first record does not look like a TLS handshake")
	}
	return f.inner.RoundTrip(r)
}

func TestPlainHTTPIsNeverUsed(t *testing.T) {
	var requests atomic.Int32
	tr := serve(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	// go-containerregistry falls back to plain HTTP for 127.0.0.1:port;
	// Kuben refuses to.
	image := "127.0.0.1:5000/acme/web:v1"
	_, err := oci.NewRegistryWith(tlsFails{tr}).ResolveAs(t.Context(), image, opt.None[oci.Login]())
	if !oci.IsUnreachable(err) || !strings.Contains(err.Error(), "HTTPS only") {
		t.Errorf("got %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d plain HTTP requests", n)
	}
}

// Rust: tag_pages_follow_link_headers (the Retry-After half).
func TestRetryAfterIsReadLikeRust(t *testing.T) {
	cases := map[string]uint64{
		"":                              60,
		"7":                             7,
		" 7 ":                           7,
		"+7":                            7,
		"99999":                         3600,
		"0":                             0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 60,
		"-1":                            60,
		"99999999999999999999999":       60,
	}
	for value, want := range cases {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		if got := oci.RetryAfter(h); got != want {
			t.Errorf("%q: %d, want %d", value, got, want)
		}
	}
}

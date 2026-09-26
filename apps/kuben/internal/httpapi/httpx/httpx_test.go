package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
)

func TestClientIPTrustsOnlyTheLastForwardedHop(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.1.1:5000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.7")
	trusting := config.DefaultSecurityCfg()
	trusting.TrustForwardedFor = true
	if got := httpx.ClientIP(r, trusting); got != opt.Some("10.0.0.7") {
		t.Errorf("got %v", got)
	}
	if got := httpx.ClientIP(r, config.DefaultSecurityCfg()); got != opt.Some("10.1.1.1") {
		t.Errorf("not trusted → peer, got %v", got)
	}
	elsewhere := trusting
	elsewhere.TrustedProxies = []string{"10.42.0.0/16"}
	if got := httpx.ClientIP(r, elsewhere); got != opt.Some("10.1.1.1") {
		t.Errorf("a peer outside the trusted proxies, got %v", got)
	}
	bare := httptest.NewRequest("GET", "/", nil)
	bare.RemoteAddr = ""
	if got := httpx.ClientIP(bare, trusting); got.IsSome() {
		t.Errorf("got %v", got)
	}
}

func TestCSRFGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := httpx.CSRFGuard(func(w http.ResponseWriter) { w.WriteHeader(403) }, ok)
	cases := []struct {
		method  string
		headers map[string]string
		want    int
	}{
		{"GET", nil, 204},
		{"POST", nil, 403},
		{"POST", map[string]string{httpx.ClientHeader: "console"}, 204},
		{"POST", map[string]string{httpx.ClientHeader: "console", "Sec-Fetch-Site": "cross-site"}, 403},
		{"DELETE", map[string]string{httpx.ClientHeader: "console", "Sec-Fetch-Site": "same-origin"}, 204},
		{"POST", map[string]string{"Authorization": "Bearer kbn_pat_x"}, 204},
		// Presence decides, as in Rust: an empty client header passes, an
		// empty Sec-Fetch-Site or Authorization is still a header.
		{"POST", map[string]string{httpx.ClientHeader: ""}, 204},
		{"POST", map[string]string{httpx.ClientHeader: "console", "Sec-Fetch-Site": ""}, 403},
		{"POST", map[string]string{"Authorization": ""}, 204},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/v1/x", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %v: got %d, want %d", c.method, c.headers, rec.Code, c.want)
		}
	}
}

func TestWrapGivesIDsAndAppliesCookies(t *testing.T) {
	h := httpx.Wrap(config.DefaultSecurityCfg(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := httpx.RequestFrom(r.Context())
		if req.ID == "" || req.UserAgent != "kuben-test" {
			t.Errorf("got %+v", req)
		}
		if v, ok := req.Cookie("kuben_session"); !ok || v != "abc" {
			t.Errorf("cookie %q %v", v, ok)
		}
		httpx.SetCookie(r.Context(), &http.Cookie{Name: "n", Value: "v", Path: "/"})
		httpx.SetHeader(r.Context(), "Retry-After", "3")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("ok"))
	}))
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("User-Agent", "kuben-test")
	r.AddCookie(&http.Cookie{Name: "kuben_session", Value: "abc"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 201 || !strings.HasPrefix(rec.Header().Get("Set-Cookie"), "n=v") || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get(httpx.RequestIDHeader) == "" {
		t.Fatal("no request id")
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set(httpx.RequestIDHeader, "given")
	rec = httptest.NewRecorder()
	httpx.Wrap(config.DefaultSecurityCfg(), http.NotFoundHandler()).ServeHTTP(rec, r)
	if rec.Header().Get(httpx.RequestIDHeader) != "given" {
		t.Fatal("the client's id is kept")
	}
}

func TestJSONIsServedWithoutACharset(t *testing.T) {
	for set, want := range map[string]string{
		"application/json; charset=utf-8": "application/json",
		"application/problem+json":        "application/problem+json",
		"text/html; charset=utf-8":        "text/html; charset=utf-8",
	} {
		rec := httptest.NewRecorder()
		httpx.Wrap(config.DefaultSecurityCfg(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", set)
			_, _ = w.Write([]byte("{}"))
		})).ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x", nil))
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: %s, want %s", set, got, want)
		}
	}
}

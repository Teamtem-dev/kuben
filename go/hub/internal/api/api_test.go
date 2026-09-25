package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/web"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

const password = "correct horse battery staple"

// client is a browser: it keeps cookies and sends the console's header.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newServer(t *testing.T, edit func(*config.Config)) *client {
	t.Helper()
	c, _ := newServerWithStore(t, edit)
	return c
}

// newServerWithStore is newServer with its store, for tests that seed it.
func newServerWithStore(t *testing.T, edit func(*config.Config)) (*client, *store.Store) {
	t.Helper()
	c, st, _ := newServerWithProjections(t, edit)
	return c, st
}

// The digests tests/http.rs resolves nginx tags to.
const (
	nginx127 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	nginx126 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// testImages is tests/http.rs images(): fixed answers, no registry.
func testImages(t *testing.T) oci.Fixed {
	t.Helper()
	images := oci.Fixed{}
	for image, text := range map[string]string{"nginx:1.27": nginx127, "nginx:1.26": nginx126} {
		d, err := artifact.ParseDigest(text)
		if err != nil {
			t.Fatal(err)
		}
		images[image] = d
	}
	return images
}

// newServerWithProjections is newServerWithStore with the projections the
// server reads, for tests that play the cluster.
func newServerWithProjections(t *testing.T, edit func(*config.Config)) (*client, *store.Store, *projection.Projections) {
	t.Helper()
	st := pgtest.Store(t)
	p := projection.New()
	cfg := config.Default()
	cfg.Server.Bind = "127.0.0.1:3000" // loopback: no setup token needed
	cfg.Security.LoginMaxFailures = 3
	if edit != nil {
		edit(&cfg)
	}
	h := health.New(clock.System{})
	h.SetReady(true)
	server, err := api.New(api.Deps{
		Config:      cfg,
		Store:       st,
		Hasher:      auth.InsecureForTests(),
		Health:      h,
		Projections: p,
		Images:      privateImages{testImages(t)},
		Keyring:     opt.Some(testKeyring()),
		SSO:         testSSO(t, cfg),
		Console:     web.NewFS(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>console")}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}, st, p
}

func (c *client) do(method, path string, body any, headers ...string) (int, map[string]any, http.Header) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(httpx.ClientHeader, "console")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i]) // an empty value means "without the header"
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	resp := c.send(req)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

// send runs req; the response is never nil.
func (c *client) send(req *http.Request) *http.Response {
	c.t.Helper()
	resp, err := c.http.Do(req)
	if err != nil || resp == nil {
		c.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
		return &http.Response{Body: http.NoBody}
	}
	return resp
}

func (c *client) get(path string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.send(req)
}

func (c *client) setup(t *testing.T) {
	t.Helper()
	status, body, headers := c.do("POST", "/api/v1/setup", map[string]any{
		"org_name": "ACME", "email": "Admin@Example.com", "password": password,
	})
	if status != 200 || body["email"] != "admin@example.com" || body["via"] != "session" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if !strings.Contains(headers.Get("Set-Cookie"), auth.CookieNameDev+"=") {
		t.Fatalf("setup signs in: %v", headers)
	}
}

func TestFirstRunSignInAndOut(t *testing.T) {
	c := newServer(t, nil)
	status, body, _ := c.do("GET", "/api/v1/setup", nil)
	if status != 200 || body["needed"] != true || body["token_required"] != false || body["secure"] != true {
		t.Fatalf("setup status: %d %v", status, body)
	}
	if status, body, _ := c.do("POST", "/api/v1/setup", map[string]any{"org_name": " ", "email": "a@b.c", "password": password}); status != 422 || body["code"] != "validation_failed" {
		t.Fatalf("an empty org name: %d %v", status, body)
	}
	c.setup(t)
	if status, body, _ := c.do("POST", "/api/v1/setup", map[string]any{"org_name": "X", "email": "b@example.com", "password": password}); status != 404 {
		t.Fatalf("setup is done: %d %v", status, body)
	}
	if status, body, _ := c.do("GET", "/api/v1/me", nil); status != 200 || body["email"] != "admin@example.com" || body["display_name"] != nil {
		t.Fatalf("me: %d %v", status, body)
	}
	resp := c.get("/api/v1/projects")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("projects: %d %s", resp.StatusCode, raw)
	}
	if status, body, _ := c.do("GET", "/api/v1/projects/nope", nil); status != 404 || body["detail"] != "not found: project `nope`" {
		t.Fatalf("unknown project: %d %v", status, body)
	}
	if status, _, _ := c.do("POST", "/api/v1/auth/logout", nil); status != 204 {
		t.Fatalf("logout: %d", status)
	}
	if status, body, _ := c.do("GET", "/api/v1/me", nil); status != 401 || body["code"] != "unauthorized" {
		t.Fatalf("signed out: %d %v", status, body)
	}
	if status, _, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"email": "admin@example.com", "password": "wrong"}); status != 401 {
		t.Fatalf("a wrong password: %d", status)
	}
	if status, body, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"email": " ADMIN@example.com", "password": password}); status != 200 || body["via"] != "session" {
		t.Fatalf("login: %d %v", status, body)
	}
	if status, _, _ := c.do("GET", "/api/v1/me", nil); status != 200 {
		t.Fatalf("me after login: %d", status)
	}
}

func TestRepeatedFailuresAreThrottled(t *testing.T) {
	c := newServer(t, nil)
	c.setup(t)
	for range 3 {
		if status, _, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"email": "admin@example.com", "password": "wrong"}); status != 401 {
			t.Fatalf("got %d", status)
		}
	}
	status, body, headers := c.do("POST", "/api/v1/auth/login", map[string]any{"email": "admin@example.com", "password": password})
	if status != 429 || body["code"] != "rate_limited" || headers.Get("Retry-After") == "" {
		t.Fatalf("throttled even with the right password: %d %v %v", status, body, headers)
	}
}

func TestMutationsNeedTheConsoleHeader(t *testing.T) {
	c := newServer(t, nil)
	status, body, _ := c.do("POST", "/api/v1/auth/logout", nil, httpx.ClientHeader, "")
	if status != 403 || body["code"] != "forbidden" {
		t.Fatalf("got %d %v", status, body)
	}
	status, _, _ = c.do("POST", "/api/v1/auth/logout", nil, "Sec-Fetch-Site", "cross-site")
	if status != 403 {
		t.Fatalf("a cross-site request: %d", status)
	}
}

func TestRoutingAndConsole(t *testing.T) {
	c := newServer(t, nil)
	if status, body, _ := c.do("GET", "/api/v1/nothing-here", nil); status != 404 || body["detail"] != "not found: no route for /api/v1/nothing-here" {
		t.Fatalf("unknown API route: %d %v", status, body)
	}
	resp := c.get("/projects/shop")
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Security-Policy") != web.CSP || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("console: %d %v", resp.StatusCode, resp.Header)
	}
	for _, probe := range []string{"/livez", "/readyz"} {
		resp := c.get(probe)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", probe, resp.StatusCode)
		}
	}
}

func TestSetupNeedsATokenOffLoopback(t *testing.T) {
	c := newServer(t, func(cfg *config.Config) {
		cfg.Server.Bind = "0.0.0.0:3000"
		cfg.Security.InsecureSetup = true
		cfg.Server.StateDir = optString(t.TempDir())
	})
	status, body, _ := c.do("GET", "/api/v1/setup", nil)
	if status != 200 || body["token_required"] != true {
		t.Fatalf("got %d %v", status, body)
	}
	status, body, _ = c.do("POST", "/api/v1/setup", map[string]any{"org_name": "ACME", "email": "a@example.com", "password": password, "token": "wrong"})
	if status != 403 || body["code"] != "forbidden" {
		t.Fatalf("a wrong token: %d %v", status, body)
	}
}

func optString(s string) opt.Val[string] { return opt.Some(s) }

// An anonymous client learns nothing from validation: authentication comes
// before the body is read, as in the Rust extractors.
func TestAnonymousRequestsAreRefusedBeforeDecoding(t *testing.T) {
	c := newServer(t, nil)
	if status, body, _ := c.do("POST", "/api/v1/tokens", map[string]any{"name": 42}); status != 401 || body["code"] != "unauthorized" {
		t.Fatalf("got %d %v", status, body)
	}
	if status, _, _ := c.do("GET", "/api/v1/audit?limit=abc", nil); status != 401 {
		t.Fatalf("got %d", status)
	}
	if status, body, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"email": 1}); status != 422 || body["code"] != "validation_failed" {
		t.Fatalf("a public operation still validates: %d %v", status, body)
	}
}

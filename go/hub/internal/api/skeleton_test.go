package api_test

import (
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
)

// The walking-skeleton tests of tests/http.rs, one Go test per Rust test.

// setupBody is tests/http.rs SETUP_BODY.
func setupBody() map[string]any {
	return map[string]any{"org_name": "ACME", "email": "Owner@Example.com", "password": "a-long-first-password"}
}

// emptyApp is tests/http.rs empty_app_with: no account yet, the setup token
// file in a directory of the test's own, cookies not Secure.
func emptyApp(t *testing.T, bind string, dir string, edit func(*config.Config)) *client {
	t.Helper()
	return newServer(t, func(cfg *config.Config) {
		cfg.Security.CookieSecure = config.CookieFixed(false)
		cfg.Server.Bind = bind
		cfg.Server.StateDir = optString(dir)
		if edit != nil {
			edit(cfg)
		}
	})
}

func TestHealthEndpoints(t *testing.T) {
	c := newFixture(t).c
	for _, probe := range []string{"/livez", "/readyz"} {
		resp := c.get(probe)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", probe, resp.StatusCode)
		}
	}
}

func TestUnknownAPIRouteIsJSON404NotSPA(t *testing.T) {
	resp := newFixture(t).c.get("/api/v1/nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
}

func TestMeRequiresAuth(t *testing.T) {
	status, problem, _ := newFixture(t).browser().do("GET", "/api/v1/me", nil)
	if status != http.StatusUnauthorized || problem["code"] != "unauthorized" {
		t.Fatalf("%d %v", status, problem)
	}
}

func TestLoginWithoutCSRFHeaderIsForbidden(t *testing.T) {
	status, _, _ := newFixture(t).browser().do("POST", "/api/v1/auth/login",
		map[string]any{"email": "alice@example.com", "password": seedPassword}, httpx.ClientHeader, "")
	if status != http.StatusForbidden {
		t.Fatalf("%d", status)
	}
}

func TestSessionLifecycle(t *testing.T) {
	f := newFixture(t)
	b := f.browser()
	status, _, headers := b.do("POST", "/api/v1/auth/login", map[string]any{"email": "Alice@Example.com", "password": seedPassword})
	if status != http.StatusOK {
		t.Fatalf("login: %d", status)
	}
	set := headers.Get("Set-Cookie")
	if !strings.HasPrefix(set, auth.CookieNameDev+"=") || !strings.Contains(set, "HttpOnly") ||
		!strings.Contains(set, "SameSite=Lax") || !strings.Contains(set, "Max-Age") {
		t.Fatalf("cookie: %s", set)
	}
	status, me, _ := b.do("GET", "/api/v1/me", nil)
	if status != http.StatusOK || me["email"] != "alice@example.com" || me["via"] != "session" {
		t.Fatalf("me: %d %v", status, me)
	}
	if status, _, _ := b.do("POST", "/api/v1/auth/logout", nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	if status, _, _ := b.do("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", status)
	}
}

func TestWrongPasswordIsUnauthorized(t *testing.T) {
	_, status, _ := newFixture(t).trySignIn("alice@example.com", "nope")
	if status != http.StatusUnauthorized {
		t.Fatalf("%d", status)
	}
}

func TestOpenAPIDocsAreServedByTheServer(t *testing.T) {
	resp := newFixture(t).c.get("/api/docs")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"title":"Kuben API"`) {
		t.Fatalf("%d %v %.200s", resp.StatusCode, err, body)
	}
}

func TestScenario1LoginIsThrottledPerClientAndIgnoresForgedHops(t *testing.T) {
	f := newFixtureWith(t, func(cfg *config.Config) {
		cfg.Security.LoginMaxFailures = 3
		cfg.Security.TrustForwardedFor = true
	})
	try := func(password, forwardedFor string) (int, http.Header) {
		status, _, headers := f.browser().do("POST", "/api/v1/auth/login",
			map[string]any{"email": "alice@example.com", "password": password}, "X-Forwarded-For", forwardedFor)
		return status, headers
	}
	for range 3 {
		if status, _ := try("wrong", "6.6.6.6, 10.0.0.1"); status != http.StatusUnauthorized {
			t.Fatalf("a wrong password: %d", status)
		}
	}
	// Same proxy-appended hop, forged first hop, correct password: still blocked.
	status, headers := try(seedPassword, "1.2.3.4, 10.0.0.1")
	retry, err := strconv.ParseUint(headers.Get("Retry-After"), 10, 64)
	if status != http.StatusTooManyRequests || err != nil || retry == 0 {
		t.Fatalf("throttled: %d %v", status, headers)
	}
	if status, _ := try(seedPassword, "6.6.6.6, 10.0.0.2"); status != http.StatusOK {
		t.Fatalf("another client is unaffected: %d", status)
	}
}

func TestSetupCreatesTheAdminAndSignsIn(t *testing.T) {
	c := emptyApp(t, "127.0.0.1:3000", t.TempDir(), nil)
	status, body, _ := c.do("GET", "/api/v1/setup", nil)
	if diff := cmp.Diff(map[string]any{"needed": true, "token_required": false, "secure": true}, body); status != http.StatusOK || diff != "" {
		t.Fatalf("status: %d %s", status, diff)
	}
	if status, _, _ := c.do("POST", "/api/v1/setup", setupBody()); status != http.StatusOK {
		t.Fatalf("setup: %d", status)
	}
	if status, me, _ := c.do("GET", "/api/v1/me", nil); status != http.StatusOK || me["email"] != "owner@example.com" {
		t.Fatalf("signed in as the new admin: %d %v", status, me)
	}
	if _, body, _ := c.do("GET", "/api/v1/setup", nil); body["needed"] != false {
		t.Fatalf("after setup: %v", body)
	}
	if status, _, _ := c.do("POST", "/api/v1/setup", setupBody()); status != http.StatusNotFound {
		t.Fatalf("only ever one first admin: %d", status)
	}
}

func TestSetupOnAPublicAddressNeedsTheInstallerToken(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Server.StateDir = optString(dir)
	token, err := api.IssueSetupToken(cfg)
	if err != nil {
		t.Fatal(err)
	}
	right := setupBody()
	right["token"] = token

	// Plain HTTP from another machine (the proxy says so, not over HTTPS):
	// no password travels, token or not.
	plain := emptyApp(t, "0.0.0.0:3000", dir, func(c *config.Config) { c.Security.TrustForwardedFor = true })
	remote := []string{"X-Forwarded-For", "203.0.113.9"}
	_, body, _ := plain.do("GET", "/api/v1/setup", nil, remote...)
	if diff := cmp.Diff(map[string]any{"needed": true, "token_required": true, "secure": false}, body); diff != "" {
		t.Fatalf("plain status: %s", diff)
	}
	status, problem, _ := plain.do("POST", "/api/v1/setup", right, remote...)
	detail, _ := problem["detail"].(string)
	if status != http.StatusForbidden || problem["code"] != "insecure_transport" || !strings.Contains(detail, "ssh -L") {
		t.Fatalf("plain setup: %d %v", status, problem)
	}

	// Behind the TLS proxy: the token decides.
	https := slices.Concat(remote, []string{"X-Forwarded-Proto", "https"})
	_, body, _ = plain.do("GET", "/api/v1/setup", nil, https...)
	if diff := cmp.Diff(map[string]any{"needed": true, "token_required": true, "secure": true}, body); diff != "" {
		t.Fatalf("https status: %s", diff)
	}
	if status, _, _ := plain.do("POST", "/api/v1/setup", setupBody(), https...); status != http.StatusForbidden {
		t.Fatalf("no token at all: %d", status)
	}
	wrong := setupBody()
	wrong["token"] = "nope"
	if status, problem, _ := plain.do("POST", "/api/v1/setup", wrong, https...); status != http.StatusForbidden || problem["code"] != "forbidden" {
		t.Fatalf("wrong token: %d %v", status, problem)
	}
	if status, _, _ := plain.do("POST", "/api/v1/setup", right, https...); status != http.StatusOK {
		t.Fatalf("right token: %d", status)
	}
	if _, err := os.Stat(api.SetupTokenPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("used tokens are removed: %v", err)
	}
}

func TestSetupRejectsWeakInput(t *testing.T) {
	c := emptyApp(t, "127.0.0.1:3000", t.TempDir(), nil)
	for what, body := range map[string]map[string]any{
		"email":    {"org_name": "ACME", "email": "nope", "password": "a-long-first-password"},
		"password": {"org_name": "ACME", "email": "a@b.c", "password": "short"},
		"org":      {"org_name": "  ", "email": "a@b.c", "password": "a-long-first-password"},
	} {
		if status, _, _ := c.do("POST", "/api/v1/setup", body); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", what, status)
		}
	}
	if _, body, _ := c.do("GET", "/api/v1/setup", nil); body["needed"] != true {
		t.Fatalf("nothing was created: %v", body)
	}
}

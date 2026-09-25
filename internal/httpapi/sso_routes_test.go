package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso/ssotest"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// testSSO is tests/http.rs setup_with's single sign-on: the configured
// client talking to the fake provider with the fixture's keys, at a time
// its tokens are valid; none unless enabled.
func testSSO(t *testing.T, cfg config.Config) opt.Val[*sso.Client] {
	t.Helper()
	if !cfg.SSO.Enabled {
		return opt.None[*sso.Client]()
	}
	fx, err := ssotest.Load()
	if err != nil {
		t.Fatal(err)
	}
	c, err := sso.FromConfig(cfg.SSO, cfg.Server.PublicURL, "acme")
	if err != nil {
		t.Fatalf("sso: %v", err)
	}
	return opt.Some(c.WithProvider(&ssotest.FakeIdp{}, fx.JWKS, clock.Fixed(ssotest.Now*1000)))
}

// auth/sso.rs the_state_cookie_is_scoped_and_short_lived (the redirect
// itself is checked by the scenarios below).
func TestTheStateCookieIsScopedAndShortLived(t *testing.T) {
	secure := config.Default()
	secure.Security.CookieSecure = config.CookieFixed(true)
	c := httpapi.SSOState(secure, "abc", 600)
	if got := c.String(); got != "kuben_sso=abc; Path=/api/v1/auth/sso; Max-Age=600; HttpOnly; Secure; SameSite=Lax" {
		t.Errorf("cookie: %s", got)
	}
	plain := config.Default()
	plain.Security.CookieSecure = config.CookieFixed(false)
	if httpapi.SSOState(plain, "", 0).Secure {
		t.Error("secure over http")
	}
	removal := httpapi.SSOStateRemoval(plain).String()
	if removal != "kuben_sso=; Path=/api/v1/auth/sso; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; HttpOnly; SameSite=Lax" {
		t.Errorf("removal: %s", removal)
	}
}

// ssoConfig is tests/http.rs sso_app's configuration.
func ssoConfig(cfg *config.Config) {
	cfg.Security.CookieSecure = config.CookieFixed(false)
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com")
	cfg.SSO.Enabled = true
	cfg.SSO.Issuer = opt.Some("https://idp.example.com")
	cfg.SSO.ClientID = opt.Some("kuben-console")
	cfg.SSO.ClientSecret = "s3cret"
	cfg.SSO.Groups = map[string]string{"platform-admins": "admin", "devs": "developer"}
	cfg.SSO.AllowedDomains = []string{"example.com"}
}

// ssoCall is one browser request that keeps redirects to itself: the
// status, the Location, the Set-Cookie pairs (`name=value`) and the JSON
// body. cookie is the Cookie header, empty for none.
func (f fixture) ssoCall(path, cookie string) (int, string, []string, any) {
	f.t.Helper()
	req, err := http.NewRequest("GET", f.c.base+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set(httpx.ClientHeader, "console")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	var body any
	_ = json.Unmarshal(raw, &body)
	var cookies []string
	for _, c := range resp.Header.Values("Set-Cookie") {
		pair, _, _ := strings.Cut(c, ";")
		cookies = append(cookies, pair)
	}
	return resp.StatusCode, resp.Header.Get("Location"), cookies, body
}

// pendingSSO is tests/http.rs pending_sso: a sign-in of the browser
// holding state, expecting nonce.
func (f fixture) pendingSSO(state, nonce string) {
	f.t.Helper()
	pending := store.PendingSSO{Nonce: nonce, Verifier: strings.Repeat("v", 43), ReturnTo: "/projects"}
	if err := f.store.BeginSSO(f.t.Context(), auth.SHA256([]byte(state)), pending,
		time.Now().UnixMilli()+600_000); err != nil {
		f.t.Fatalf("begin: %v", err)
	}
}

// ssoCallback is tests/http.rs sso_callback: where the callback of code
// redirects the browser holding cookie, and the cookies it sets.
func (f fixture) ssoCallback(code, state, cookie string) (string, []string) {
	f.t.Helper()
	status, to, cookies, _ := f.ssoCall(fmt.Sprintf("/api/v1/auth/sso/callback?code=%s&state=%s", code, state), cookie)
	if status != http.StatusSeeOther {
		f.t.Fatalf("callback: %d", status)
	}
	return to, cookies
}

// tests/http.rs m4_sso_start_binds_the_state_to_the_browser.
func TestM4SSOStartBindsTheStateToTheBrowser(t *testing.T) {
	f := newUsersFixture(t, ssoConfig)
	if status, _, _, info := f.ssoCall("/api/v1/auth/sso", ""); status != http.StatusOK ||
		info.(map[string]any)["enabled"] != true {
		t.Fatalf("info: %d %v", status, info)
	}
	status, location, cookies, _ := f.ssoCall("/api/v1/auth/sso/start?returnTo=//evil.example", "")
	if status != http.StatusSeeOther {
		t.Fatalf("start: %d", status)
	}
	if !strings.HasPrefix(location, "https://idp.example.com/authorize?response_type=code") ||
		!strings.Contains(location, "code_challenge_method=S256") {
		t.Fatalf("location: %s", location)
	}
	var state string
	for kv := range strings.SplitSeq(location, "&") {
		if v, ok := strings.CutPrefix(kv, "state="); ok {
			state = v
		}
	}
	if state == "" {
		t.Fatalf("no state in %s", location)
	}
	var bound string
	for _, c := range cookies {
		if strings.HasPrefix(c, "kuben_sso=") {
			bound = c
		}
	}
	if bound != "kuben_sso="+state {
		t.Errorf("state cookie %q, state %q", bound, state)
	}
}

// tests/http.rs m4_sso_signs_mapped_people_in_once.
func TestM4SSOSignsMappedPeopleInOnce(t *testing.T) {
	f := newUsersFixture(t, ssoConfig)
	state := strings.Repeat("s", 43)
	browser := "kuben_sso=" + state
	f.pendingSSO(state, "nonce-1")
	to, cookies := f.ssoCallback("valid", state, browser)
	if to != "/projects" {
		t.Fatalf("to: %s", to)
	}
	var session string
	for _, c := range cookies {
		if !strings.HasPrefix(c, "kuben_sso=") {
			session = c
		}
	}
	if session == "" {
		t.Fatalf("no session cookie: %v", cookies)
	}
	status, _, _, me := f.ssoCall("/api/v1/me", session)
	if m, _ := me.(map[string]any); status != http.StatusOK || m["email"] != "carol@example.com" {
		t.Fatalf("me: %d %v", status, me)
	}
	_, _, _, members := f.ssoCall("/api/v1/members", session)
	list, _ := members.([]any)
	var role any
	for _, m := range list {
		if member, _ := m.(map[string]any); member["email"] == "carol@example.com" {
			role = member["role"]
		}
	}
	if role != "admin" {
		t.Errorf("mapped from platform-admins: %v", members)
	}
	if to, _ := f.ssoCallback("valid", state, browser); to != "/login?error=sso" {
		t.Errorf("a state works once: %s", to)
	}
}

// tests/http.rs m4_sso_refuses_everything_else.
func TestM4SSORefusesEverythingElse(t *testing.T) {
	f := newUsersFixture(t, ssoConfig)
	fresh := func(n int) string { return fmt.Sprintf("%043d", n) }
	f.pendingSSO(fresh(0), "nonce-1")
	if to, _ := f.ssoCallback("valid", fresh(0), "kuben_sso=other"); to != "/login?error=sso" {
		t.Errorf("another browser's cookie: %s", to)
	}
	f.pendingSSO(fresh(1), "another-nonce")
	if to, _ := f.ssoCallback("valid", fresh(1), "kuben_sso="+fresh(1)); to != "/login?error=sso" {
		t.Errorf("a token of another sign-in: %s", to)
	}
	for n, code := range []string{"no_role", "outsider", "unverified", "wrong_aud", "nope"} {
		state := fresh(n + 2)
		f.pendingSSO(state, "nonce-1")
		to, cookies := f.ssoCallback(code, state, "kuben_sso="+state)
		if to != "/login?error=sso" {
			t.Errorf("%s: %s", code, to)
		}
		for _, c := range cookies {
			if !strings.HasPrefix(c, "kuben_sso=") {
				t.Errorf("a session for %s: %v", code, cookies)
			}
		}
	}
}

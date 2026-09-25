package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oidc"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func createCIPolicy(t *testing.T, text string) *gen.CreateCiPolicy {
	t.Helper()
	var req gen.CreateCiPolicy
	if err := req.UnmarshalJSON([]byte(text)); err != nil {
		t.Fatal(err)
	}
	return &req
}

const ciPolicyBody = `{"name": "shop-deploy", "project": "shop", "repository": "acme/shop",
	"repositoryId": 123456, "repositoryOwnerId": 42, "refs": ["refs/heads/main"]}`

// Ported from routes/ci.rs: bodies_become_deny_by_default_policies.
func TestBodiesBecomeDenyByDefaultPolicies(t *testing.T) {
	p, err := api.TrustPolicy(createCIPolicy(t, ciPolicyBody))
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != perm.Developer || !cmp.Equal(p.Events, []string{"push", "workflow_dispatch", "release"}) ||
		p.TokenTTLSecs != ci.DefaultCITokenTTLSecs {
		t.Fatalf("defaults: %+v", p)
	}
	owner := createCIPolicy(t, ciPolicyBody)
	owner.Role = gen.NewOptNilString("owner")
	if _, err := api.TrustPolicy(owner); err == nil {
		t.Error("an owner policy")
	}
	fork := createCIPolicy(t, ciPolicyBody)
	fork.Refs = []string{"refs/pull/*"}
	if _, err := api.TrustPolicy(fork); err == nil {
		t.Error("a pull-request ref")
	}
	var unknown gen.CreateCiPolicy
	if err := unknown.UnmarshalJSON([]byte(`{"name": "x", "admin": true}`)); err == nil {
		t.Error("unknown fields are refused")
	}
}

// Ported from routes/ci.rs: bearer_tokens_are_read_strictly.
func TestBearerTokensAreReadStrictly(t *testing.T) {
	h := http.Header{}
	if token, ok := api.Bearer(h); ok {
		t.Errorf("no header: %q", token)
	}
	h.Set("Authorization", "Basic abc")
	if token, ok := api.Bearer(h); ok {
		t.Errorf("basic: %q", token)
	}
	h.Set("Authorization", "Bearer  ey.x.y ")
	if token, ok := api.Bearer(h); !ok || token != "ey.x.y" {
		t.Errorf("bearer: %q %v", token, ok)
	}
}

// Ported from routes/ci.rs: token_names_say_where_they_came_from.
func TestTokenNamesSayWhereTheyCameFrom(t *testing.T) {
	var claims ci.GithubClaims
	if err := json.Unmarshal([]byte(`{"iss": "i", "aud": "a", "sub": "s", "jti": "j", "iat": 1, "exp": 2,
		"repository": "acme/shop", "repository_id": "1", "repository_owner": "acme",
		"repository_owner_id": "2", "ref": "refs/heads/main", "event_name": "push", "run_id": "77"}`), &claims); err != nil {
		t.Fatal(err)
	}
	policy, err := api.TrustPolicy(createCIPolicy(t, ciPolicyBody))
	if err != nil {
		t.Fatal(err)
	}
	long := store.CIPolicy{
		Org: ids.New[ids.Org](), Project: ids.New[ids.Project](), Name: strings.Repeat("x", 70),
		Repository: "acme/shop", Policy: policy, CreatedBy: ids.New[ids.User](),
	}
	if n := utf8.RuneCountInString(api.CITokenName(long, claims)); n != 64 {
		t.Errorf("a long name has %d characters", n)
	}
	short := long
	short.Name = "deploy"
	if got := api.CITokenName(short, claims); got != "ci:deploy:acme/shop#77" {
		t.Errorf("name: %s", got)
	}
}

const (
	// ciNow is between the fixture tokens' `iat` and `exp` (Unix seconds).
	ciNow      int64 = 1_800_000_100
	ciAudience       = "https://kuben.example.com"
)

// ciFixture is crates/kuben-api/src/testdata/github-oidc.json: the keys of
// a test issuer and tokens signed with them.
type ciFixture struct {
	JWKS   json.RawMessage   `json:"jwks"`
	Tokens map[string]string `json:"tokens"`
}

func loadCIFixture(t *testing.T) ciFixture {
	t.Helper()
	raw, err := os.ReadFile("oidc/testdata/github-oidc.json")
	if err != nil {
		t.Fatal(err)
	}
	var f ciFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// ciTrust is tests/http.rs trusted(): the app on CI trust with shop's
// policy for acme/shop's main branch, made by alice, and GitHub played by
// a fake issuer serving the fixture's keys.
type ciTrust struct {
	fixture
	alice  *client
	policy string
	tokens map[string]string
	// ci is a workflow runner: no cookie, no console header.
	ci *client
}

func ciConfig(cfg *config.Config) {
	cfg.CI.GithubActions = true
	cfg.CI.GithubOIDCAudience = opt.Some(ciAudience)
}

func newCITrust(t *testing.T) ciTrust {
	t.Helper()
	f := newFixtureWith(t, ciConfig)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	if status, body, _ := bob.do("POST", "/api/v1/ci/trust-policies", json.RawMessage(ciPolicyBody)); status != 403 {
		t.Fatalf("viewers cannot trust CI: %d %v", status, body)
	}
	status, made, _ := alice.do("POST", "/api/v1/ci/trust-policies", json.RawMessage(ciPolicyBody))
	if status != 201 {
		t.Fatalf("create: %d %v", status, made)
	}
	id, _ := made["id"].(string)

	fx := loadCIFixture(t)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(fx.JWKS)
	}))
	t.Cleanup(issuer.Close)
	// The tokens are GitHub's (their `iss`); their keys come from the fake
	// issuer, read over plain http, at a time the tokens are valid.
	at := clock.Fixed(ciNow * 1000)
	keys := oidc.NewKeyCache(oidc.JWKSURL(issuer.URL), outbound.New(true, time.Second), at, nil)
	verifier := oidc.NewGithubWith(ci.GithubActionsIssuer, ciAudience, keys, at)

	cfg := config.Default()
	ciConfig(&cfg)
	server, err := api.New(api.Deps{Config: cfg, Store: f.store})
	if err != nil {
		t.Fatal(err)
	}
	// The exchange runs beside the fixture's server, on the same database.
	mux := http.NewServeMux()
	mux.Handle(api.CIExchangePath, server.CIExchangeOn(verifier))
	exchange := httptest.NewServer(mux)
	t.Cleanup(exchange.Close)
	return ciTrust{
		fixture: f, alice: alice, policy: id, tokens: fx.Tokens,
		ci: &client{t: t, base: exchange.URL, http: &http.Client{}},
	}
}

// exchange posts the fixture token name for policy.
func (c ciTrust) exchange(name, policy string) (int, map[string]any) {
	c.t.Helper()
	status, body, _ := c.ci.do("POST", api.CIExchangePath, map[string]any{"policy": policy},
		"Authorization", "Bearer "+c.tokens[name], httpx.ClientHeader, "")
	return status, body
}

// Ported from tests/http.rs: m4_untrusted_ci_tokens_get_nothing.
func TestM4UntrustedCITokensGetNothing(t *testing.T) {
	c := newCITrust(t)
	for _, name := range []string{"tampered", "wrong_aud", "wrong_iss", "unknown_kid", "pull_request"} {
		if status, body := c.exchange(name, c.policy); status != 401 || body["detail"] != "the CI token is not trusted" {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}
	if status, _ := c.exchange("valid", ids.New[ids.Token]().String()); status != 401 {
		t.Errorf("an unknown policy: %d", status)
	}
	if status, _, _ := c.ci.do("POST", api.CIExchangePath, map[string]any{"policy": c.policy}, httpx.ClientHeader, ""); status != 401 {
		t.Errorf("no provider token: %d", status)
	}
}

// Ported from tests/http.rs: m4_trusted_ci_gets_a_scoped_token_once.
func TestM4TrustedCIGetsAScopedTokenOnce(t *testing.T) {
	c := newCITrust(t)
	status, issued := c.exchange("valid", c.policy)
	if status != 201 || issued["role"] != "developer" {
		t.Fatalf("exchange: %d %v", status, issued)
	}
	token, _ := issued["token"].(string)
	if status, replay := c.exchange("valid", c.policy); status != 401 {
		t.Fatalf("a provider token works once: %d %v", status, replay)
	}
	asCI := func(method, path string, body any) int {
		status, _ := c.bearer(token, method, path, body)
		return status
	}
	if status := asCI("GET", "/api/v1/projects/shop/environments", nil); status != 200 {
		t.Errorf("environments: %d", status)
	}
	if status := asCI("POST", deployments, deployBody(0)); status != 202 {
		t.Errorf("deploy: %d", status)
	}
	if status := asCI("GET", "/api/v1/members", nil); status != 403 {
		t.Errorf("project-scoped: %d", status)
	}
	if status := asCI("POST", "/api/v1/ci/trust-policies", json.RawMessage(ciPolicyBody)); status != 403 {
		t.Errorf("tokens cannot mint trust: %d", status)
	}
	if status, body, _ := c.alice.do("DELETE", "/api/v1/ci/trust-policies/"+c.policy, nil); status != 204 {
		t.Fatalf("revoke: %d %v", status, body)
	}
	if status := asCI("GET", "/api/v1/projects/shop/environments", nil); status != 401 {
		t.Errorf("revoked with its policy: %d", status)
	}
	status, listed := c.alice.list("/api/v1/ci/trust-policies")
	if status != 200 || len(listed) != 1 {
		t.Fatalf("list: %d %v", status, listed)
	}
	if _, ok := listed[0]["revokedAt"].(float64); !ok {
		t.Errorf("revokedAt: %v", listed[0])
	}
}

// The exchange is mounted outside the session and CSRF layers (a runner
// has neither), answers only POST, and has a body limit of its own (lib.rs
// router).
func TestTheExchangeIsMountedOnItsOwn(t *testing.T) {
	off := newFixture(t).browser()
	status, body, _ := off.do("POST", api.CIExchangePath, map[string]any{"policy": "x"}, httpx.ClientHeader, "")
	if status != 404 || body["detail"] != "CI trust is not configured" {
		t.Fatalf("CI trust off: %d %v", status, body)
	}
	on := newFixtureWith(t, ciConfig).browser()
	status, body, _ = on.do("POST", api.CIExchangePath, map[string]any{"policy": "x"}, httpx.ClientHeader, "")
	want := map[string]any{"title": "unauthorized", "detail": "the CI token is not trusted"}
	if status != 401 || !cmp.Equal(body, want) {
		t.Fatalf("no provider token, no session, no console header: %d %v", status, body)
	}
	status, body, _ = on.do("POST", api.CIExchangePath, "x", "Authorization", "Bearer ey.x.y", httpx.ClientHeader, "")
	if status != 422 || body["detail"] != `send {"policy": "<id>"}` {
		t.Fatalf("a body that is not a request: %d %v", status, body)
	}
	status, _, headers := on.do("GET", api.CIExchangePath, nil)
	if status != 405 || headers.Get("Allow") != "POST" {
		t.Fatalf("GET: %d %v", status, headers)
	}
	big := map[string]any{"policy": strings.Repeat("x", 4<<10)}
	if status, _, _ := on.do("POST", api.CIExchangePath, big, httpx.ClientHeader, ""); status != 413 {
		t.Fatalf("a body over 4 KiB: %d", status)
	}
}

package api_test

import (
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
)

func TestTokenNames(t *testing.T) {
	cases := map[string]bool{
		"ci":                     true,
		"":                       false,
		strings.Repeat("é", 64):  true,
		strings.Repeat("é", 65):  false,
		"line\nbreak":            false,
		"bell\x07":               false,
		"github actions (main)":  true,
		"c1 control \u0085 here": false,
	}
	for name, want := range cases {
		if got := api.ValidTokenName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

// tests/http.rs scenario3_api_tokens_are_capped_scoped_and_revocable.
// Routes not ported yet are replaced by ported ones with the same
// authorization: GET /audit for the viewer cap (was POST /projects) and
// GET /projects/shop for the project token (was .../environments).
func TestScenario3APITokensAreCappedScopedAndRevocable(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	status, created, _ := alice.do("POST", "/api/v1/tokens", map[string]any{"name": "ci", "role": "viewer"})
	if status != 201 {
		t.Fatalf("create: %d %v", status, created)
	}
	token, _ := created["token"].(string)
	info, _ := created["info"].(map[string]any)
	prefix, _ := info["prefix"].(string)
	if !strings.HasPrefix(token, "kbn_pat_") || prefix == "" || !strings.HasPrefix(token, prefix) {
		t.Fatalf("token %q, prefix %q", token, prefix)
	}
	if info["role"] != "viewer" || info["project"] != nil || info["environment"] != nil ||
		info["revoked"] != false || info["expires_at"] == nil || info["last_used_at"] != nil || info["name"] != "ci" {
		t.Fatalf("info: %v", info)
	}

	if status, me := f.bearer(token, "GET", "/api/v1/me", nil); status != 200 || me["via"] != "token" {
		t.Fatalf("me: %d %v", status, me)
	}
	if status, _ := f.bearer(token, "GET", "/api/v1/projects", nil); status != 200 {
		t.Fatalf("projects: %d", status)
	}
	if status, _ := f.bearer(token, "GET", "/api/v1/audit", nil); status != 403 {
		t.Fatalf("viewer cap: %d", status)
	}
	if status, _ := f.bearer(token, "POST", "/api/v1/tokens", map[string]any{"name": "x"}); status != 403 {
		t.Fatalf("tokens cannot mint tokens: %d", status)
	}
	if status, _ := f.bearer(token, "GET", "/api/v1/tokens", nil); status != 403 {
		t.Fatalf("tokens cannot list tokens: %d", status)
	}
	last := token[len(token)-1]
	forged := token[:len(token)-1] + map[bool]string{true: "B", false: "A"}[last == 'A']
	if status, _ := f.bearer(forged, "GET", "/api/v1/me", nil); status != 401 {
		t.Fatalf("forged: %d", status)
	}

	status, scoped, _ := alice.do("POST", "/api/v1/tokens", map[string]any{"name": "deploy", "role": "developer", "project": "shop"})
	scopedInfo, _ := scoped["info"].(map[string]any)
	if status != 201 || scopedInfo["project"] != "shop" || scopedInfo["environment"] != nil {
		t.Fatalf("scoped: %d %v", status, scoped)
	}
	scopedToken, _ := scoped["token"].(string)
	if status, _ := f.bearer(scopedToken, "GET", "/api/v1/projects/shop", nil); status != 200 {
		t.Fatalf("the project token reads its project: %d", status)
	}
	if status, _ := f.bearer(scopedToken, "GET", "/api/v1/members", nil); status != 403 {
		t.Fatalf("a project token has no org-wide authority: %d", status)
	}

	id, _ := info["id"].(string)
	if status := alice.status("DELETE", "/api/v1/tokens/"+id, nil); status != 204 {
		t.Fatalf("revoke: %d", status)
	}
	if status, _ := f.bearer(token, "GET", "/api/v1/me", nil); status != 401 {
		t.Fatalf("a revoked token: %d", status)
	}
}

func TestTokenRules(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	refused := []struct {
		body   map[string]any
		status int
		detail string
	}{
		{map[string]any{"name": "  "}, 422, "validation failed: name must be 1–64 printable characters"},
		{map[string]any{"name": "x", "role": "owner"}, 422, "validation failed: tokens are capped at `admin`"},
		{map[string]any{"name": "x", "role": "root"}, 422, "validation failed: unknown role `root`"},
		{map[string]any{"name": "x", "environment": "prod"}, 422, "validation failed: environment requires project"},
		{map[string]any{"name": "x", "project": "secret-project"}, 404, "not found: project `secret-project`"},
		{map[string]any{"name": "x", "project": "shop", "environment": "nope"}, 404, "not found: environment `nope`"},
		{map[string]any{"name": "x", "expires_in_days": 0}, 422, "validation failed: expires_in_days must be between 1 and 365"},
		{map[string]any{"name": "x", "expires_in_days": 366}, 422, "validation failed: expires_in_days must be between 1 and 365"},
	}
	for _, c := range refused {
		if status, body, _ := alice.do("POST", "/api/v1/tokens", c.body); status != c.status || body["detail"] != c.detail {
			t.Errorf("%v: %d %v", c.body, status, body)
		}
	}
	bob := f.signIn("bob@example.com", seedPassword)
	status, body, _ := bob.do("POST", "/api/v1/tokens", map[string]any{"name": "x"})
	if status != 422 || body["detail"] != "validation failed: a `viewer` cannot create a `developer` token" {
		t.Fatalf("above the caller's role: %d %v", status, body)
	}

	status, env, _ := alice.do("POST", "/api/v1/tokens", map[string]any{
		"name": " staging ", "role": "admin", "project": "shop", "environment": "prod", "expires_in_days": nil,
	})
	envInfo, _ := env["info"].(map[string]any)
	if status != 201 || envInfo["name"] != "staging" || envInfo["project"] != "shop" || envInfo["environment"] != "shop-prod" {
		t.Fatalf("environment token: %d %v", status, env)
	}
	status, tokens := alice.list("/api/v1/tokens")
	if status != 200 || len(tokens) != 1 || tokens[0]["environment"] != "shop-prod" || tokens[0]["role"] != "admin" {
		t.Fatalf("list: %d %v", status, tokens)
	}
	if status, _ := bob.list("/api/v1/tokens"); status != 200 {
		t.Fatalf("everyone lists their own tokens: %d", status)
	}
	id, _ := envInfo["id"].(string)
	if status, body, _ := bob.do("DELETE", "/api/v1/tokens/"+id, nil); status != 404 || body["detail"] != "not found: token `"+id+"`" {
		t.Fatalf("someone else's token: %d %v", status, body)
	}
	if status, body, _ := alice.do("DELETE", "/api/v1/tokens/nope", nil); status != 404 || body["detail"] != "not found: token `nope`" {
		t.Fatalf("not an id: %d %v", status, body)
	}
	if status := alice.status("DELETE", "/api/v1/tokens/"+id, nil); status != 204 {
		t.Fatalf("revoke: %d", status)
	}
	if status := alice.status("DELETE", "/api/v1/tokens/"+id, nil); status != 404 {
		t.Fatalf("revoked already: %d", status)
	}
	if _, tokens := alice.list("/api/v1/tokens"); len(tokens) != 1 || tokens[0]["revoked"] != true {
		t.Fatalf("a revoked token stays listed: %v", tokens)
	}
}

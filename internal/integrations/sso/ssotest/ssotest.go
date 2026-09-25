// Package ssotest is a test identity provider for single sign-on: the
// FakeIdp of crates/kuben-api/src/sso.rs and tests/http.rs, answering from
// memory with the ID tokens of the Rust fixture src/testdata/sso-oidc.json
// (a copy, embedded).
package ssotest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/oidc"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso"
)

// Now is a time (Unix seconds) at which the fixture's tokens are valid.
const Now int64 = 1_800_000_100

//go:embed sso-oidc.json
var fixture []byte

// Fixture is the provider's keys and the ID tokens it signed, by name
// (`valid`, `developer`, `unverified`, `outsider`, `no_role`, `wrong_aud`).
type Fixture struct {
	JWKS   oidc.JWKS         `json:"jwks"`
	Tokens map[string]string `json:"tokens"`
}

// Load is the fixture.
func Load() (Fixture, error) {
	var f Fixture
	if err := json.Unmarshal(fixture, &f); err != nil {
		return Fixture{}, fmt.Errorf("the SSO fixture: %w", err)
	}
	return f, nil
}

// FakeIdp is a provider answering from memory: its discovery document
// names https://idp.example.com (or Issuer), and its token endpoint hands
// out the fixture token named by the code, or 400 for an unknown code.
type FakeIdp struct {
	// Issuer is the issuer the discovery document claims.
	Issuer opt.Val[string]

	mu    sync.Mutex // guards forms
	forms []string
}

var _ sso.IdentityProvider = (*FakeIdp)(nil)

// Forms are the forms posted to the token endpoint, in order.
func (f *FakeIdp) Forms() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forms...)
}

// Get answers the discovery document.
func (f *FakeIdp) Get(_ context.Context, url string, _ int64) ([]byte, error) {
	if url != "https://idp.example.com/.well-known/openid-configuration" {
		return nil, fmt.Errorf("unexpected GET %s", url)
	}
	return json.Marshal(map[string]string{ //nolint:gosec // endpoints, not credentials
		"issuer":                 f.Issuer.Or("https://idp.example.com"),
		"authorization_endpoint": "https://idp.example.com/authorize",
		"token_endpoint":         "https://idp.example.com/token",
		"jwks_uri":               "https://idp.example.com/jwks",
	})
}

// PostForm answers the token endpoint.
func (f *FakeIdp) PostForm(_ context.Context, url, form, basic string) (int, []byte, error) {
	if url != "https://idp.example.com/token" || basic == "" {
		return 0, nil, fmt.Errorf("unexpected POST %s", url)
	}
	f.mu.Lock()
	f.forms = append(f.forms, form)
	f.mu.Unlock()
	var code string
	for kv := range strings.SplitSeq(form, "&") {
		if c, ok := strings.CutPrefix(kv, "code="); ok {
			code = c
			break
		}
	}
	fx, err := Load()
	if err != nil {
		return 0, nil, err
	}
	token, ok := fx.Tokens[code]
	if !ok {
		return http.StatusBadRequest, []byte(`{"error":"invalid_grant"}`), nil
	}
	body, err := json.Marshal(map[string]string{"id_token": token, "token_type": "Bearer"})
	return http.StatusOK, body, err
}

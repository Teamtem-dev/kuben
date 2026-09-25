package oidc_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oidc"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
)

const (
	// now is between the fixture tokens' `iat` and `exp`.
	now      int64 = 1_800_000_100
	audience       = "https://kuben.example.com"
)

// fixture is the Rust fixture crates/kuben-api/src/testdata/github-oidc.json:
// the issuer's keys and tokens signed with them.
type fixture struct {
	JWKS   json.RawMessage   `json:"jwks"`
	Tokens map[string]string `json:"tokens"`
}

func load(t *testing.T) (oidc.JWKS, fixture) {
	t.Helper()
	raw, err := os.ReadFile("testdata/github-oidc.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var jwks oidc.JWKS
	if err := json.Unmarshal(f.JWKS, &jwks); err != nil {
		t.Fatal(err)
	}
	return jwks, f
}

func token(t *testing.T, name string) string {
	t.Helper()
	_, f := load(t)
	tok, ok := f.Tokens[name]
	if !ok {
		t.Fatalf("no fixture token %s", name)
	}
	return tok
}

func verifier(t *testing.T, clk clock.Clock) *oidc.Github {
	t.Helper()
	jwks, _ := load(t)
	return oidc.NewGithubWith(ci.GithubActionsIssuer, audience, oidc.FixedKeys(jwks), clk)
}

// Ported from oidc.rs: a_signed_github_token_is_accepted.
func TestASignedGithubTokenIsAccepted(t *testing.T) {
	claims, err := verifier(t, clock.System{}).VerifyAt(t.Context(), token(t, "valid"), now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.RepositoryID != 123_456 || claims.GitRef != "refs/heads/main" || claims.Environment != opt.Some("production") {
		t.Fatalf("claims: %+v", claims)
	}
}

// Ported from oidc.rs: forged_and_foreign_tokens_are_refused.
func TestForgedAndForeignTokensAreRefused(t *testing.T) {
	v := verifier(t, clock.System{})
	for name, want := range map[string]error{
		"tampered":    oidc.ErrSignature,
		"unknown_kid": oidc.ErrUnknownKey,
		"wrong_aud":   ci.TokenAudience,
		"wrong_iss":   ci.TokenIssuer,
	} {
		if _, err := v.VerifyAt(t.Context(), token(t, name), now); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	if _, err := v.VerifyAt(t.Context(), token(t, "valid"), 1_800_000_300+61); !errors.Is(err, ci.TokenExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func encode(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }

// Ported from oidc.rs: only_rs256_with_a_named_key_is_considered.
func TestOnlyRS256WithANamedKeyIsConsidered(t *testing.T) {
	jwks, _ := load(t)
	valid := token(t, "valid")
	_, rest, _ := strings.Cut(valid, ".")
	for _, header := range []string{
		`{"alg":"none","kid":"kuben-test-1"}`,
		`{"alg":"HS256","kid":"kuben-test-1"}`,
		`{"alg":"RS256","kid":"kuben-test-1","crit":["exp"]}`,
	} {
		if _, err := oidc.VerifyRS256(encode(header)+"."+rest, jwks); !errors.Is(err, oidc.ErrAlgorithm) {
			t.Errorf("%s: %v", header, err)
		}
	}
	if _, err := oidc.VerifyRS256(encode(`{"alg":"RS256"}`)+"."+rest, jwks); !errors.Is(err, oidc.ErrUnknownKey) {
		t.Errorf("no kid: %v", err)
	}
	for _, malformed := range []string{"", "a.b", "a.b.c.d", "!!.!!.!!", strings.Repeat("a", oidc.MaxToken+1)} {
		if _, err := oidc.VerifyRS256(malformed, jwks); !errors.Is(err, oidc.ErrMalformed) {
			t.Errorf("%.10s: %v", malformed, err)
		}
	}
	hmacKey := oidc.JWKS{Keys: []oidc.JWK{{Kid: opt.Some("kuben-test-1"), Kty: "oct", Alg: opt.Some("HS256")}}}
	if _, err := oidc.VerifyRS256(valid, hmacKey); !errors.Is(err, oidc.ErrUnknownKey) {
		t.Errorf("an HMAC key: %v", err)
	}
}

// Ported from oidc.rs: the_clock_decides_expiry.
func TestTheClockDecidesExpiry(t *testing.T) {
	if _, err := verifier(t, clock.Fixed(1_900_000_000_000)).Verify(t.Context(), token(t, "valid")); !errors.Is(err, ci.TokenExpired) {
		t.Fatalf("late: %v", err)
	}
	if _, err := verifier(t, clock.Fixed(now*1000)).Verify(t.Context(), token(t, "valid")); err != nil {
		t.Fatalf("in time: %v", err)
	}
}

// Ported from oidc.rs: the_jwks_url_comes_from_the_issuer.
func TestTheJWKSURLComesFromTheIssuer(t *testing.T) {
	v := oidc.NewGithub("https://token.actions.githubusercontent.com/", audience, outbound.New(false, oidc.Timeout), clock.System{}, nil)
	if got := v.Keys().URL(); got != "https://token.actions.githubusercontent.com/.well-known/jwks" {
		t.Fatalf("url: %s", got)
	}
	if v.Issuer() != ci.GithubActionsIssuer {
		t.Fatalf("issuer: %s", v.Issuer())
	}
}

// issuer is a fake issuer serving the fixture's keys; down makes it fail.
type issuer struct {
	jwks    json.RawMessage
	fetches atomic.Int32
	down    atomic.Bool
}

func (i *issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.well-known/jwks" {
		http.NotFound(w, r)
		return
	}
	i.fetches.Add(1)
	if i.down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	_, _ = w.Write(i.jwks)
}

// A clock tests move by hand.
type manual struct{ ms atomic.Int64 }

func (m *manual) NowMs() int64 { return m.ms.Load() }

func (m *manual) advance(d time.Duration) { m.ms.Add(d.Milliseconds()) }

// Keys come from the issuer's JWKS URL, are cached, are refreshed for an
// unknown key at most once a minute, and stay in use while the issuer is
// down (KeyCache in oidc.rs).
func TestKeysAreFetchedCachedAndRefreshed(t *testing.T) {
	_, f := load(t)
	fake := &issuer{jwks: f.JWKS}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	clk := &manual{}
	clk.ms.Store(now * 1000)
	keys := oidc.NewKeyCache(oidc.JWKSURL(srv.URL), outbound.New(true, time.Second), clk, nil)
	v := oidc.NewGithubWith(ci.GithubActionsIssuer, audience, keys, clk)
	ctx := t.Context()
	// The clock ages the keys; the tokens are checked at `now` throughout.

	if _, err := v.VerifyAt(ctx, token(t, "valid"), now); err != nil || fake.fetches.Load() != 1 {
		t.Fatalf("first: %v after %d fetches", err, fake.fetches.Load())
	}
	if _, err := v.VerifyAt(ctx, token(t, "valid"), now); err != nil || fake.fetches.Load() != 1 {
		t.Fatalf("cached: %v after %d fetches", err, fake.fetches.Load())
	}
	if _, err := v.VerifyAt(ctx, token(t, "unknown_kid"), now); !errors.Is(err, oidc.ErrUnknownKey) || fake.fetches.Load() != 1 {
		t.Fatalf("an unknown key within the floor: %v after %d fetches", err, fake.fetches.Load())
	}
	clk.advance(time.Minute)
	if _, err := v.VerifyAt(ctx, token(t, "unknown_kid"), now); !errors.Is(err, oidc.ErrUnknownKey) || fake.fetches.Load() != 2 {
		t.Fatalf("an unknown key refreshes: %v after %d fetches", err, fake.fetches.Load())
	}

	fake.down.Store(true)
	clk.advance(10 * time.Minute)
	if _, err := v.VerifyAt(ctx, token(t, "valid"), now); err != nil || fake.fetches.Load() != 3 {
		t.Fatalf("stale keys stay while the issuer is down: %v after %d fetches", err, fake.fetches.Load())
	}
	clk.advance(time.Minute)
	var unavailable oidc.UnavailableError
	if _, err := v.VerifyAt(ctx, token(t, "unknown_kid"), now); !errors.As(err, &unavailable) || unavailable.Reason != "HTTP 502 Bad Gateway" {
		t.Fatalf("an unknown key while the issuer is down: %v", err)
	}

	fresh := oidc.NewGithubWith(ci.GithubActionsIssuer, audience,
		oidc.NewKeyCache(oidc.JWKSURL(srv.URL), outbound.New(true, time.Second), clk, nil), clk)
	if _, err := fresh.VerifyAt(ctx, token(t, "valid"), now); !errors.As(err, &unavailable) {
		t.Fatalf("no keys yet and the issuer down: %v", err)
	}
	httpsOnly := oidc.NewGithub(srv.URL, audience, outbound.New(false, time.Second), clk, nil)
	if _, err := httpsOnly.VerifyAt(ctx, token(t, "valid"), now); !errors.As(err, &unavailable) {
		t.Fatalf("keys only over https: %v", err)
	}
}

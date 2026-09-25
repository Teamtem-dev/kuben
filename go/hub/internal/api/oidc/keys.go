package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/outbound"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
)

const (
	// Timeout bounds each read of an issuer's keys, as it bounded the
	// Rust HttpsGet.
	Timeout = 10 * time.Second
	maxJWKS = 256 << 10
	// keysTTL is how long keys are used before they are read again.
	keysTTL = 10 * time.Minute
	// refreshFloor is how often, at most, an unknown key triggers a
	// refresh.
	refreshFloor = time.Minute
)

// KeyCache is the signing keys of one issuer, read from its JWKS URL and
// cached.
type KeyCache struct {
	url    string
	client *outbound.Client
	clock  clock.Clock
	logger *slog.Logger
	// fixed keys were given up front and are never fetched.
	fixed bool

	// mu guards jwks and fetched; a fetch holds it for writing, so
	// concurrent refreshes wait for one another as they did behind the
	// Rust RwLock.
	mu      sync.RWMutex
	jwks    JWKS
	fetched opt.Val[int64] // Unix milliseconds
}

// NewKeyCache is keys read from url through client; clk ages them. A nil
// logger is slog's default.
func NewKeyCache(url string, client *outbound.Client, clk clock.Clock, logger *slog.Logger) *KeyCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &KeyCache{url: url, client: client, clock: clk, logger: logger}
}

// FixedKeys is exactly jwks, never fetched (tests, air-gapped installs).
func FixedKeys(jwks JWKS) *KeyCache {
	return &KeyCache{jwks: jwks, fixed: true, clock: clock.System{}, logger: slog.Default()}
}

// URL is where the keys are read.
func (c *KeyCache) URL() string { return c.url }

func (c *KeyCache) fetch(ctx context.Context) (JWKS, error) {
	if c.client == nil {
		return JWKS{}, UnavailableError{Reason: "no HTTP client"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return JWKS{}, UnavailableError{Reason: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	status, body, err := c.client.Fetch(ctx, req, maxJWKS)
	if err != nil {
		return JWKS{}, UnavailableError{Reason: err.Error()}
	}
	if status != http.StatusOK {
		return JWKS{}, UnavailableError{Reason: fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))}
	}
	var jwks JWKS
	if err := json.Unmarshal(body, &jwks); err != nil {
		return JWKS{}, UnavailableError{Reason: err.Error()}
	}
	return jwks, nil
}

// cached is the keys if they need no fetch: fixed, or read less than the
// TTL ago and, for an unknown key, less than the refresh floor ago.
func (c *KeyCache) cached(unknownKey bool) (JWKS, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.fixed {
		return c.jwks, true
	}
	at, ok := c.fetched.Get()
	if !ok {
		return JWKS{}, false
	}
	age := time.Duration(clock.SaturatingSub(c.clock.NowMs(), at)) * time.Millisecond
	return c.jwks, age < keysTTL && (!unknownKey || age < refreshFloor)
}

// keys is the current keys; fetched when stale, or when unknownKey and
// the last fetch is older than the refresh floor.
func (c *KeyCache) keys(ctx context.Context, unknownKey bool) (JWKS, error) {
	if jwks, fresh := c.cached(unknownKey); fresh {
		return jwks, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	jwks, err := c.fetch(ctx)
	switch {
	case err == nil:
		c.jwks, c.fetched = jwks, opt.Some(c.clock.NowMs())
		return jwks, nil
	case c.fetched.IsSome() && !unknownKey:
		// Keep answering with the old keys while the issuer is down.
		c.logger.Warn("OIDC keys not refreshed; the cached ones stay in use", "error", err)
		return c.jwks, nil
	default:
		return JWKS{}, err
	}
}

// Verify is the payload of token if one of the issuer's keys signed it; an
// unknown key refreshes the keys once.
func (c *KeyCache) Verify(ctx context.Context, token string) ([]byte, error) {
	jwks, err := c.keys(ctx, false)
	if err != nil {
		return nil, err
	}
	payload, err := VerifyRS256(token, jwks)
	if !errors.Is(err, ErrUnknownKey) || c.fixed {
		return payload, err
	}
	if jwks, err = c.keys(ctx, true); err != nil {
		return nil, err
	}
	return VerifyRS256(token, jwks)
}

// JWKSURL is where the keys of issuer are read.
func JWKSURL(issuer string) string {
	return strings.TrimRight(issuer, "/") + "/.well-known/jwks"
}

// Github verifies GitHub Actions OIDC tokens for one issuer and audience.
type Github struct {
	issuer   string
	audience string
	keys     *KeyCache
	clock    clock.Clock
}

// NewGithub is a verifier that reads the keys of issuer from its JWKS URL
// through client (https only in production).
func NewGithub(issuer, audience string, client *outbound.Client, clk clock.Clock, logger *slog.Logger) *Github {
	return NewGithubWith(issuer, audience, NewKeyCache(JWKSURL(issuer), client, clk, logger), clk)
}

// NewGithubWith is a verifier on keys: [FixedKeys] for tests and
// air-gapped installs. clk says when now is.
func NewGithubWith(issuer, audience string, keys *KeyCache, clk clock.Clock) *Github {
	return &Github{issuer: strings.TrimRight(issuer, "/"), audience: audience, keys: keys, clock: clk}
}

// Issuer is the one issuer trusted, without trailing slashes.
func (g *Github) Issuer() string { return g.issuer }

// Keys is where the verifier reads its keys.
func (g *Github) Keys() *KeyCache { return g.keys }

// Verify is the claims of token, if it is a valid token of this issuer for
// this audience now.
func (g *Github) Verify(ctx context.Context, token string) (ci.GithubClaims, error) {
	return g.VerifyAt(ctx, token, g.clock.NowMs()/1000)
}

// VerifyAt is the claims of token at now (Unix seconds).
func (g *Github) VerifyAt(ctx context.Context, token string, now int64) (ci.GithubClaims, error) {
	payload, err := g.keys.Verify(ctx, token)
	if err != nil {
		return ci.GithubClaims{}, err
	}
	var claims ci.GithubClaims
	if json.Unmarshal(payload, &claims) != nil {
		return ci.GithubClaims{}, ErrMalformed
	}
	if err := claims.Check(g.issuer, g.audience, now); err != nil {
		return ci.GithubClaims{}, err //nolint:wrapcheck // a ci.TokenError, matched on
	}
	return claims, nil
}

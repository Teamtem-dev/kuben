// Package sso is the OpenID Connect client for single sign-on (M4.3):
// discovery, the authorization URL with PKCE, the code exchange and ID
// token verification. It replaces crates/kuben-api/src/sso.rs; tokens are
// verified by package oidc (RS256 over the provider's JWKS), the claims and
// the policy are package core/sso.
//
// The provider's discovery document must name the configured issuer, and
// its keys and endpoints are used only from there. ID tokens are accepted
// only when RS256-signed by one of those keys, for this client and this
// sign-in's nonce.
package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	coresso "github.com/Teamtem-dev/kuben/internal/core/sso"
	"github.com/Teamtem-dev/kuben/internal/integrations/oidc"
	"github.com/Teamtem-dev/kuben/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
)

// MaxDocument is the largest answer read from the provider, in bytes.
const MaxDocument = 256 << 10

// Why a sign-in failed. People see one answer; the reason is logged. A
// person or token the policy refuses fails with the [coresso.Denied]
// itself.
type (
	// UnavailableError is a provider that cannot be reached or read.
	UnavailableError struct{ Reason string }
	// CodeRefusedError is a code the token endpoint did not exchange.
	CodeRefusedError struct{ Reason string }
	// TokenError is an ID token that did not verify (an oidc error).
	TokenError struct{ Err error }
	// ConfigError is a configuration that cannot work.
	ConfigError struct{ Reason string }
)

func (e UnavailableError) Error() string {
	return "the identity provider is unavailable: " + e.Reason
}

func (e CodeRefusedError) Error() string {
	return "the identity provider refused the code: " + e.Reason
}

func (e TokenError) Error() string { return "the ID token was not accepted: " + e.Err.Error() }

// Unwrap is the oidc error.
func (e TokenError) Unwrap() error { return e.Err }

func (e ConfigError) Error() string {
	return "the SSO configuration is incomplete: " + e.Reason
}

// IdentityProvider is what sign-in needs from the provider: reading its
// documents, and posting a form to its token endpoint.
type IdentityProvider interface {
	// Get is the body of url if it answers 200, at most limit bytes.
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
	// PostForm posts form to url with HTTP Basic client authentication
	// basic; the answer's status and body, whatever the status.
	PostForm(ctx context.Context, url, form, basic string) (int, []byte, error)
}

// Discovery is the parts of the provider's discovery document Kuben uses.
type Discovery struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
}

// UnmarshalJSON reads a discovery document: the four members must be
// there, the rest is ignored.
func (d *Discovery) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // the JSON error is the answer
	}
	var out Discovery
	for _, err := range []error{
		jsonx.Required(o, "issuer", &out.Issuer),
		jsonx.Required(o, "authorization_endpoint", &out.AuthorizationEndpoint),
		jsonx.Required(o, "token_endpoint", &out.TokenEndpoint),
		jsonx.Required(o, "jwks_uri", &out.JWKSURI),
	} {
		if err != nil {
			return err
		}
	}
	*d = out
	return nil
}

type provider struct {
	discovery Discovery
	keys      *oidc.KeyCache
}

// Encode percent-encodes a query or form value: everything but ASCII
// letters, digits and `-._~`, in upper-case hex.
func Encode(value string) string {
	var b strings.Builder
	for i := range len(value) {
		c := value[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func https(url string) bool { return strings.HasPrefix(url, "https://") }

// Challenge is the PKCE S256 challenge of verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomValue is a URL-safe random value of 32 bytes (43 characters).
func RandomValue() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails (Go ≥ 1.24)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Client is the single sign-on client of this installation.
type Client struct {
	issuer       string
	clientID     string
	clientSecret config.Secret
	redirectURI  string
	scopes       []string
	groupClaim   string
	policy       coresso.Policy
	displayName  string
	orgSlug      string
	http         IdentityProvider
	// outbound reads the provider's keys; nil with fixed keys.
	outbound *outbound.Client
	// fixedKeys are keys given up front (tests).
	fixedKeys opt.Val[oidc.JWKS]
	clock     clock.Clock

	// mu guards provider; it is held while the provider is discovered,
	// so concurrent sign-ins wait for one discovery (Rust's OnceCell).
	mu       sync.Mutex
	provider opt.Val[provider]
}

// FromConfig is the client cfg describes, answering at publicURL, joining
// people to defaultOrg unless the configuration names another. The error
// is a [ConfigError] when a required setting is missing or invalid.
func FromConfig(cfg config.SsoCfg, publicURL opt.Val[string], defaultOrg string) (*Client, error) {
	missing := func(what string) error { return ConfigError{Reason: "sso." + what + " is required"} }
	issuer, ok := cfg.Issuer.Get()
	if !ok {
		return nil, missing("issuer")
	}
	clientID, ok := cfg.ClientID.Get()
	if !ok {
		return nil, missing("client_id")
	}
	var secret config.Secret
	switch path, fromFile := cfg.ClientSecretFile.Get(); {
	case fromFile:
		text, err := os.ReadFile(path) //nolint:gosec // an operator-configured path
		if err != nil {
			return nil, ConfigError{Reason: fmt.Sprintf("cannot read sso.client_secret_file %s: %v", path, err)}
		}
		secret = config.Secret(strings.TrimSpace(string(text)))
	case cfg.ClientSecret.IsSet():
		secret = cfg.ClientSecret
	default:
		return nil, missing("client_secret_file")
	}
	public, ok := publicURL.Get()
	if !ok || !https(public) {
		return nil, ConfigError{Reason: "server.public_url must be an https URL for SSO"}
	}
	policy, err := cfg.Policy()
	if err != nil {
		return nil, ConfigError{Reason: err.Error()}
	}
	if len(policy.Groups) == 0 && policy.DefaultRole.IsNone() {
		return nil, ConfigError{Reason: "map at least one group to a role (sso.groups) or set sso.default_role"}
	}
	client := outbound.New(false, oidc.Timeout)
	return &Client{
		issuer:       strings.TrimRight(issuer, "/"),
		clientID:     clientID,
		clientSecret: secret,
		redirectURI:  strings.TrimRight(public, "/") + "/api/v1/auth/sso/callback",
		scopes:       cfg.Scopes,
		groupClaim:   cfg.GroupClaim,
		policy:       policy,
		displayName:  cfg.DisplayName,
		orgSlug:      cfg.Org.Or(defaultOrg),
		http:         HTTPS{Client: client},
		outbound:     client,
		clock:        clock.System{},
	}, nil
}

// WithProvider is the same client talking to idp with fixed keys, reading
// the time from clk (tests). Call it before the client is used.
func (c *Client) WithProvider(idp IdentityProvider, keys oidc.JWKS, clk clock.Clock) *Client {
	c.http, c.fixedKeys, c.clock, c.outbound = idp, opt.Some(keys), clk, nil
	return c
}

// Format shows the issuer and the client id only, whatever the verb: the
// client secret never reaches a log (Rust's finish_non_exhaustive Debug).
func (c *Client) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sso.Client{issuer: %q, clientID: %q, ..}", c.issuer, c.clientID) //nolint:errcheck // fmt.State cannot report
}

// DisplayName is the sign-in button's label.
func (c *Client) DisplayName() string { return c.displayName }

// OrgSlug is the organization people join.
func (c *Client) OrgSlug() string { return c.orgSlug }

// Issuer is the provider's issuer, without trailing slashes.
func (c *Client) Issuer() string { return c.issuer }

// RedirectURI is where the provider sends the browser back.
func (c *Client) RedirectURI() string { return c.redirectURI }

// discover is the provider, read from its discovery document once.
func (c *Client) discover(ctx context.Context) (provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.provider.Get(); ok {
		return p, nil
	}
	body, err := c.http.Get(ctx, c.issuer+"/.well-known/openid-configuration", MaxDocument)
	if err != nil {
		return provider{}, UnavailableError{Reason: err.Error()}
	}
	var d Discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return provider{}, UnavailableError{Reason: err.Error()}
	}
	if strings.TrimRight(d.Issuer, "/") != c.issuer {
		return provider{}, ConfigError{Reason: fmt.Sprintf("the provider calls itself %s, not %s", d.Issuer, c.issuer)}
	}
	if !https(d.AuthorizationEndpoint) || !https(d.TokenEndpoint) || !https(d.JWKSURI) {
		return provider{}, ConfigError{Reason: "the provider's endpoints must be https"}
	}
	keys := oidc.NewKeyCache(d.JWKSURI, c.outbound, c.clock, nil)
	if jwks, ok := c.fixedKeys.Get(); ok {
		keys = oidc.FixedKeys(jwks)
	}
	p := provider{discovery: d, keys: keys}
	c.provider = opt.Some(p)
	return p, nil
}

// AuthorizeURL is where to send the browser to sign in.
func (c *Client) AuthorizeURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	p, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	separator := "?"
	if strings.Contains(p.discovery.AuthorizationEndpoint, "?") {
		separator = "&"
	}
	return p.discovery.AuthorizationEndpoint + separator +
		"response_type=code&client_id=" + Encode(c.clientID) +
		"&redirect_uri=" + Encode(c.redirectURI) +
		"&scope=" + Encode(strings.Join(c.scopes, " ")) +
		"&state=" + Encode(state) +
		"&nonce=" + Encode(nonce) +
		"&code_challenge=" + Challenge(verifier) + "&code_challenge_method=S256", nil
}

// SignIn is the person behind code, if the provider and the policy admit
// them.
func (c *Client) SignIn(ctx context.Context, code, verifier, nonce string) (coresso.Person, error) {
	p, err := c.discover(ctx)
	if err != nil {
		return coresso.Person{}, err
	}
	idToken, err := c.exchange(ctx, p, code, verifier)
	if err != nil {
		return coresso.Person{}, err
	}
	payload, err := p.keys.Verify(ctx, idToken)
	if err != nil {
		return coresso.Person{}, TokenError{Err: err}
	}
	var claims coresso.IDClaims
	if json.Unmarshal(payload, &claims) != nil {
		return coresso.Person{}, TokenError{Err: oidc.ErrMalformed}
	}
	if err := claims.Check(c.issuer, c.clientID, nonce, c.clock.NowMs()/1000); err != nil {
		return coresso.Person{}, err //nolint:wrapcheck // a coresso.Denied, matched on
	}
	person, err := c.policy.Admit(claims, claims.Groups(c.groupClaim))
	if err != nil {
		return coresso.Person{}, err //nolint:wrapcheck // a coresso.Denied, matched on
	}
	return person, nil
}

// exchange trades code for an ID token at the token endpoint.
func (c *Client) exchange(ctx context.Context, p provider, code, verifier string) (string, error) {
	form := "grant_type=authorization_code&code=" + Encode(code) +
		"&redirect_uri=" + Encode(c.redirectURI) +
		"&code_verifier=" + Encode(verifier) +
		"&client_id=" + Encode(c.clientID)
	basic := base64.StdEncoding.EncodeToString([]byte(Encode(c.clientID) + ":" + Encode(c.clientSecret.Expose())))
	status, body, err := c.http.PostForm(ctx, p.discovery.TokenEndpoint, form, basic)
	if err != nil {
		return "", UnavailableError{Reason: err.Error()}
	}
	if status < 200 || status > 299 {
		return "", CodeRefusedError{Reason: httpStatus(status)}
	}
	var o jsonx.Object
	var idToken opt.Val[string]
	if err := json.Unmarshal(body, &o); err != nil {
		return "", CodeRefusedError{Reason: err.Error()}
	}
	if err := jsonx.Optional(o, "id_token", &idToken); err != nil {
		return "", CodeRefusedError{Reason: err.Error()}
	}
	token, ok := idToken.Get()
	if !ok {
		return "", CodeRefusedError{Reason: "no id_token"}
	}
	return token, nil
}

// httpStatus is a status as http's StatusCode displayed it: `HTTP 400 Bad Request`.
func httpStatus(status int) string {
	return strings.TrimSpace(fmt.Sprintf("HTTP %d %s", status, http.StatusText(status)))
}

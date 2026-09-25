package sso_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	coresso "github.com/Teamtem-dev/kuben/internal/core/sso"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso/ssotest"
)

func ssoConfig() config.SsoCfg {
	cfg := config.DefaultSsoCfg()
	cfg.Enabled = true
	cfg.Issuer = opt.Some("https://idp.example.com/")
	cfg.ClientID = opt.Some("kuben-console")
	cfg.ClientSecret = "s3cret"
	cfg.Groups = map[string]string{"platform-admins": "admin", "devs": "developer"}
	cfg.AllowedDomains = []string{"example.com"}
	return cfg
}

func client(t *testing.T, idp *ssotest.FakeIdp) *sso.Client {
	t.Helper()
	fx, err := ssotest.Load()
	if err != nil {
		t.Fatal(err)
	}
	c, err := sso.FromConfig(ssoConfig(), opt.Some("https://kuben.example.com/"), "acme")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c.WithProvider(idp, fx.JWKS, clock.Fixed(ssotest.Now*1000))
}

func TestConfigurationIsCheckedUpFront(t *testing.T) {
	ok, err := sso.FromConfig(ssoConfig(), opt.Some("https://kuben.example.com"), "acme")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if ok.OrgSlug() != "acme" || ok.Issuer() != "https://idp.example.com" ||
		ok.RedirectURI() != "https://kuben.example.com/api/v1/auth/sso/callback" {
		t.Errorf("client: %s %s %s", ok.OrgSlug(), ok.Issuer(), ok.RedirectURI())
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if text := fmt.Sprintf(format, ok); strings.Contains(text, "s3cret") {
			t.Errorf("%s shows the secret: %s", format, text)
		}
	}
	edit := func(f func(*config.SsoCfg)) config.SsoCfg {
		cfg := ssoConfig()
		f(&cfg)
		return cfg
	}
	cases := []struct {
		name string
		cfg  config.SsoCfg
		url  opt.Val[string]
	}{
		{"no issuer", edit(func(c *config.SsoCfg) { c.Issuer = opt.None[string]() }), opt.Some("https://kuben.example.com")},
		{"no secret", edit(func(c *config.SsoCfg) { c.ClientSecret = "" }), opt.Some("https://kuben.example.com")},
		{"http", ssoConfig(), opt.Some("http://kuben.example.com")},
		{"no public url", ssoConfig(), opt.None[string]()},
		{"no groups", edit(func(c *config.SsoCfg) { c.Groups = map[string]string{} }), opt.Some("https://kuben.example.com")},
	}
	for _, c := range cases {
		var cfgErr sso.ConfigError
		if _, err := sso.FromConfig(c.cfg, c.url, "acme"); !errors.As(err, &cfgErr) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	defaulted := edit(func(c *config.SsoCfg) {
		c.Groups = map[string]string{}
		c.DefaultRole = opt.Some("viewer")
	})
	if _, err := sso.FromConfig(defaulted, opt.Some("https://k.example.com"), "acme"); err != nil {
		t.Errorf("a default role: %v", err)
	}
}

func TestTheAuthorizationURLCarriesPKCEAndTheNonce(t *testing.T) {
	c := client(t, &ssotest.FakeIdp{})
	const verifier = "verifier-verifier-verifier-verifier-verif"
	url, err := c.AuthorizeURL(t.Context(), "st@te", "nonce-1", verifier)
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	for _, want := range []string{
		"redirect_uri=https%3A%2F%2Fkuben.example.com%2Fapi%2Fv1%2Fauth%2Fsso%2Fcallback",
		"scope=openid%20email%20profile",
		"state=st%40te",
		"nonce=nonce-1",
		"code_challenge=" + sso.Challenge(verifier) + "&code_challenge_method=S256",
	} {
		if !strings.Contains(url, want) {
			t.Errorf("%s lacks %s", url, want)
		}
	}
	if !strings.HasPrefix(url, "https://idp.example.com/authorize?response_type=code&client_id=kuben-console") {
		t.Errorf("url: %s", url)
	}
	// RFC 7636 appendix B.
	if got := sso.Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge: %s", got)
	}
	if a, b := sso.RandomValue(), sso.RandomValue(); len(a) != 43 || a == b {
		t.Errorf("random values: %s %s", a, b)
	}
}

func TestAValidCodeSignsTheMappedPersonIn(t *testing.T) {
	idp := &ssotest.FakeIdp{}
	c := client(t, idp)
	carol, err := c.SignIn(t.Context(), "valid", "the-verifier", "nonce-1")
	if err != nil {
		t.Fatalf("carol: %v", err)
	}
	if carol.Email != "carol@example.com" || carol.Role != perm.Admin {
		t.Errorf("carol: %+v", carol)
	}
	dave, err := c.SignIn(t.Context(), "developer", "v", "nonce-1")
	if err != nil || dave.Role != perm.Developer {
		t.Errorf("dave: %+v %v", dave, err)
	}
	form := idp.Forms()[0]
	if !strings.Contains(form, "grant_type=authorization_code") || !strings.Contains(form, "code_verifier=the-verifier") {
		t.Errorf("form: %s", form)
	}
}

func TestAnythingElseIsRefused(t *testing.T) {
	c := client(t, &ssotest.FakeIdp{})
	cases := []struct {
		code, nonce string
		want        coresso.Denied
	}{
		{"valid", "nonce-2", coresso.DeniedNonce},
		{"unverified", "nonce-1", coresso.DeniedUnverified},
		{"outsider", "nonce-1", coresso.DeniedDomain},
		{"no_role", "nonce-1", coresso.DeniedNoRole},
		{"wrong_aud", "nonce-1", coresso.DeniedAudience},
	}
	for _, tc := range cases {
		var denied coresso.Denied
		if _, err := c.SignIn(t.Context(), tc.code, "v", tc.nonce); !errors.As(err, &denied) || denied != tc.want {
			t.Errorf("%s: %v, want %v", tc.code, err, tc.want)
		}
	}
	var refused sso.CodeRefusedError
	if _, err := c.SignIn(t.Context(), "unknown", "v", "nonce-1"); !errors.As(err, &refused) {
		t.Errorf("an unknown code: %v", err)
	}
}

func TestAProviderClaimingAnotherIssuerIsNotUsed(t *testing.T) {
	c := client(t, &ssotest.FakeIdp{Issuer: opt.Some("https://evil.example.com")})
	var cfgErr sso.ConfigError
	if _, err := c.SignIn(t.Context(), "valid", "v", "nonce-1"); !errors.As(err, &cfgErr) {
		t.Errorf("another issuer: %v", err)
	}
}

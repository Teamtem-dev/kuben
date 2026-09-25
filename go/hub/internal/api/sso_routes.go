package api

// Browser sign-in with the identity provider (auth/sso.rs, M4.3).
//
// `start` remembers the sign-in (state hash, nonce, PKCE verifier) and
// binds it to this browser with a short-lived cookie; `callback` accepts
// only a state that matches both, once, then opens an ordinary session.
// Every failure ends on the sign-in page with the same message; the reason
// goes to the log and the audit record.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/sso"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	coresso "github.com/Teamtem-dev/kuben/go/hub/internal/core/sso"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

const (
	// ssoStateCookie binds a started sign-in to the browser.
	ssoStateCookie = "kuben_sso"
	ssoCookiePath  = "/api/v1/auth/sso"
	// ssoFailed is where a failed sign-in lands.
	ssoFailed = "/login?error=sso"
)

// ssoState is the cookie carrying the state of a started sign-in:
// scoped to the SSO routes, sent on the provider's top-level navigation
// back (Lax), alive for maxAgeSecs.
func ssoState(cfg config.Config, value string, maxAgeSecs int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure only over HTTPS, as the session cookie
		Name: ssoStateCookie, Value: value, Path: ssoCookiePath, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAgeSecs, Secure: cfg.CookieSecure(),
	}
}

// ssoStateRemoval clears the state cookie (cookie crate's make_removal).
func ssoStateRemoval(cfg config.Config) *http.Cookie {
	c := ssoState(cfg, "", -1) //nolint:gosec // a removal: Secure as the cookie it removes
	c.Expires = time.Unix(0, 0)
	return c
}

// ssoClient is the single sign-on client, 404 when it is not configured.
func (s *Server) ssoClient() (*sso.Client, error) {
	c, ok := s.deps.SSO.Get()
	if !ok {
		return nil, kerr.New(kerr.NotFound, "single sign-on is not configured")
	}
	return c, nil
}

// GetSsoInfo says whether single sign-on is offered, for the sign-in page.
func (s *Server) GetSsoInfo(context.Context) (*gen.SsoInfo, error) {
	c, ok := s.deps.SSO.Get()
	if !ok {
		var name, start gen.OptNilString
		name.SetToNull()
		start.SetToNull()
		return &gen.SsoInfo{Enabled: false, DisplayName: name, StartUrl: start}, nil
	}
	return &gen.SsoInfo{
		Enabled:     true,
		DisplayName: gen.NewOptNilString(c.DisplayName()),
		StartUrl:    gen.NewOptNilString(ssoCookiePath + "/start"),
	}, nil
}

// StartSso starts signing in: redirects to the identity provider.
func (s *Server) StartSso(ctx context.Context, params gen.StartSsoParams) (gen.StartSsoRes, error) {
	c, err := s.ssoClient()
	if err != nil {
		return nil, err
	}
	state, nonce, verifier := sso.RandomValue(), sso.RandomValue(), sso.RandomValue()
	url, err := c.AuthorizeURL(ctx, state, nonce, verifier)
	if err != nil {
		return nil, kerr.New(kerr.Unavailable, "%s", err.Error())
	}
	pending := store.PendingSSO{Nonce: nonce, Verifier: verifier, ReturnTo: coresso.SafeReturnTo(params.ReturnTo.Or(""))}
	expires := s.deps.Clock.NowMs() + coresso.LoginWindowSecs*1000
	if err := s.deps.Store.BeginSSO(ctx, auth.SHA256([]byte(state)), pending, expires); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	httpx.SetCookie(ctx, ssoState(s.deps.Config, state, int(coresso.LoginWindowSecs)))
	httpx.SetHeader(ctx, "Location", url)
	return &gen.StartSsoSeeOther{}, nil
}

// FinishSso is the identity provider's answer: signs the person in and
// redirects, to where they started or, on any failure, to the sign-in
// page.
func (s *Server) FinishSso(ctx context.Context, params gen.FinishSsoParams) error {
	cookie, bound := httpx.RequestFrom(ctx).Cookie(ssoStateCookie)
	if bound {
		httpx.SetCookie(ctx, ssoStateRemoval(s.deps.Config))
	}
	to, err := s.finishSSO(ctx, cookie, bound, params)
	if err != nil {
		s.deps.Logger.Warn("a single sign-on was refused", "reason", err.Error())
		s.auditLogin(ctx, opt.None[model.User](), false, "sso", "failure")
		to = ssoFailed
	}
	httpx.SetHeader(ctx, "Location", to)
	return nil
}

// finishSSO signs the person of the callback in and opens their session;
// where to go next, or why not. bound is whether the browser sent the
// state cookie.
func (s *Server) finishSSO(ctx context.Context, cookie string, bound bool, params gen.FinishSsoParams) (string, error) {
	c, ok := s.deps.SSO.Get()
	if !ok {
		return "", errors.New("not configured")
	}
	if refusal, refused := params.Error.Get(); refused {
		return "", fmt.Errorf("the provider answered %s", refusal)
	}
	code, hasCode := params.Code.Get()
	returned, hasState := params.State.Get()
	if !hasCode || !hasState {
		return "", errors.New("no code or state")
	}
	if !bound || subtle.ConstantTimeCompare([]byte(cookie), []byte(returned)) != 1 {
		return "", errors.New("the state does not belong to this browser")
	}
	pending, found, err := s.deps.Store.TakeSSO(ctx, auth.SHA256([]byte(returned)))
	if err != nil {
		return "", err //nolint:wrapcheck // logged as it is
	}
	if !found {
		return "", errors.New("the sign-in is unknown, used or expired")
	}
	person, err := c.SignIn(ctx, code, pending.Verifier, pending.Nonce)
	if err != nil {
		return "", err //nolint:wrapcheck // logged as it is
	}
	org, found, err := s.deps.Store.FindOrgBySlug(ctx, c.OrgSlug())
	if err != nil {
		return "", err //nolint:wrapcheck // logged as it is
	}
	if !found {
		return "", fmt.Errorf("organization `%s` does not exist", c.OrgSlug())
	}
	outcome, err := s.deps.Store.SSOSignIn(ctx, org.ID, c.Issuer(), person)
	if err != nil {
		return "", err //nolint:wrapcheck // logged as it is
	}
	var user model.User
	switch o := outcome.(type) {
	case store.SSOSignedIn:
		user = o.User
	case store.SSOInactive:
		return "", fmt.Errorf("%s is deactivated", person.Email)
	}
	s.auditLogin(ctx, opt.Some(user), true, person.Email, "success")
	if _, err := s.startSession(ctx, user); err != nil {
		return "", err
	}
	return pending.ReturnTo, nil
}

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/httpapi/stream"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// userDto is the user as the console sees it (auth/mod.rs UserDto).
func userDto(c httpx.CurrentUser) *gen.UserDto {
	return &gen.UserDto{
		ID:                 c.User.ID.String(),
		Email:              c.User.Email,
		DisplayName:        optNilString(c.User.DisplayName),
		Via:                string(c.Via),
		MustChangePassword: c.User.MustChangePassword,
	}
}

func optNilString(v opt.Val[string]) gen.OptNilString {
	if s, ok := v.Get(); ok {
		return gen.NewOptNilString(s)
	}
	var n gen.OptNilString
	n.SetToNull()
	return n
}

// withPermit bounds concurrent password hashing (security.login_concurrency).
func (s *Server) withPermit(ctx context.Context, f func()) error {
	select {
	case s.loginPermits <- struct{}{}:
		defer func() { <-s.loginPermits }()
		f()
		return nil
	case <-ctx.Done():
		return kerrors.Wrap(ctx.Err(), "waiting to hash a password")
	}
}

// Login signs in with email and password, sets an HttpOnly session cookie
// and throttles repeated failures (429 with Retry-After).
func (s *Server) Login(ctx context.Context, req *gen.LoginRequest) (gen.LoginRes, error) {
	email := ascii.Lower(strings.TrimSpace(req.Email))
	ip := httpx.RequestFrom(ctx).IP
	if wait := s.throttle.check(ctx, email, ip); wait > 0 {
		s.auditLogin(ctx, opt.None[model.User](), false, email, "throttled")
		return nil, kerrors.TooMany(wait)
	}
	creds, found, err := s.deps.Store.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	// Always verify a hash so the timing does not tell whether the account
	// exists.
	hash := s.deps.Hasher.DummyHash()
	if found {
		hash = creds.PasswordHash.Or(hash)
	}
	ok := false
	if err := s.withPermit(ctx, func() { ok = s.deps.Hasher.Verify(req.Password, hash) }); err != nil {
		return nil, err
	}
	account := opt.None[model.User]()
	if found {
		account = opt.Some(creds.User)
	}
	// With `sso.disable_password_for_linked`, an account linked to the
	// identity provider signs in there only.
	ssoOnly := false
	if found && ok && s.deps.SSO.IsSome() && s.deps.Config.SSO.DisablePasswordForLinked {
		if ssoOnly, err = s.deps.Store.HasIdentity(ctx, creds.User.ID); err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	if !found || !ok || ssoOnly || !creds.User.IsActive || creds.PasswordHash.IsNone() {
		s.throttle.recordFailure(ctx, email, ip)
		s.auditLogin(ctx, account, false, email, "failure")
		return nil, kerrors.ErrUnauthorized
	}
	s.throttle.recordSuccess(ctx, email, ip)
	s.auditLogin(ctx, account, true, email, "success")
	return s.startSession(ctx, creds.User)
}

// startSession opens a session for user and sets its cookie; login and the
// first-run setup share it.
func (s *Server) startSession(ctx context.Context, user model.User) (*gen.UserDto, error) {
	raw, idHash := auth.NewSessionID()
	req := httpx.RequestFrom(ctx)
	ua := opt.None[[]byte]()
	if req.UserAgent != "" {
		ua = opt.Some(auth.SHA256([]byte(req.UserAgent)))
	}
	err := s.deps.Store.CreateSession(ctx, store.NewSession{
		IDHash:    idHash,
		UserID:    user.ID,
		ExpiresAt: clock.PlusHours(s.deps.Clock.NowMs(), s.deps.Config.Security.SessionTTLHours),
		IP:        req.IP,
		UAHash:    ua,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	httpx.SetCookie(ctx, auth.SessionCookie(s.deps.Config, raw))
	return userDto(httpx.CurrentUser{User: user, Via: httpx.ViaSession}), nil
}

// auditLogin records a login attempt; the audit middleware skips `login`
// because only the handler knows the attempted email.
func (s *Server) auditLogin(ctx context.Context, account opt.Val[model.User], authenticated bool, email, outcome string) {
	org := opt.None[ids.OrgID]()
	actorID := opt.None[string]()
	if u, ok := account.Get(); ok {
		if bindings, err := s.deps.Store.BindingsForUser(ctx, u.ID); err == nil && len(bindings) > 0 {
			org = opt.Some(bindings[0].OrgID)
		}
		if authenticated {
			actorID = opt.Some(u.ID.String())
		}
	}
	kind := "anonymous"
	if authenticated {
		kind = "user"
	}
	req := httpx.RequestFrom(ctx)
	_, err := s.deps.Store.AppendAudit(ctx, store.NewAudit{
		OrgID:      org,
		ActorKind:  kind,
		ActorID:    actorID,
		Action:     "login",
		Outcome:    outcome,
		TargetKind: opt.Some("user"),
		TargetRef:  opt.Some(email),
		IP:         req.IP,
	})
	if err != nil {
		s.deps.Logger.Error("audit write failed", "error", err)
	}
}

// Logout revokes the current session and clears the cookie.
func (s *Server) Logout(ctx context.Context) error {
	if raw, ok := httpx.RequestFrom(ctx).Cookie(auth.CookieName(s.deps.Config)); ok {
		idHash := auth.SHA256([]byte(raw))
		if err := s.deps.Store.RevokeSession(ctx, idHash); err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		s.sessionCache.Remove(string(idHash))
	}
	httpx.SetCookie(ctx, auth.RemovalCookie(s.deps.Config))
	return nil
}

// GetMe is the authenticated user.
func (s *Server) GetMe(ctx context.Context) (gen.GetMeRes, error) {
	c, ok := httpx.UserFrom(ctx)
	if !ok {
		return nil, kerrors.ErrUnauthorized
	}
	return userDto(c), nil
}

// ChangePassword replaces the caller's password and signs every other
// session out. Tokens cannot change passwords.
func (s *Server) ChangePassword(ctx context.Context, req *gen.ChangePassword) (gen.ChangePasswordRes, error) {
	c, ok := httpx.UserFrom(ctx)
	if !ok {
		return nil, kerrors.ErrUnauthorized
	}
	if c.Token.IsSome() {
		return nil, kerrors.ErrForbidden
	}
	minLen := s.deps.Config.Security.PasswordMinLength
	if uint64(utf8.RuneCountInString(req.NewPassword)) < minLen { //nolint:gosec // a count is never negative
		return nil, kerrors.New(kerrors.Validation, "the new password needs at least %d characters", minLen)
	}
	if req.NewPassword == req.CurrentPassword {
		return nil, kerrors.New(kerrors.Validation, "the new password must differ from the current one")
	}
	stored := s.deps.Hasher.DummyHash()
	if creds, found, err := s.deps.Store.FindUserByEmail(ctx, c.User.Email); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	} else if found {
		stored = creds.PasswordHash.Or(stored)
	}
	var verified bool
	var hash string
	var hashErr error
	if err := s.withPermit(ctx, func() {
		if verified = s.deps.Hasher.Verify(req.CurrentPassword, stored); verified {
			hash, hashErr = s.deps.Hasher.Hash(req.NewPassword)
		}
	}); err != nil {
		return nil, err
	}
	if !verified {
		return nil, kerrors.New(kerrors.Validation, "the current password is incorrect")
	}
	if hashErr != nil {
		return nil, kerrors.Wrap(hashErr, "hash the new password")
	}
	if err := s.deps.Store.SetPasswordHash(ctx, c.User.ID, hash); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	keep := []byte{}
	if raw, ok := httpx.RequestFrom(ctx).Cookie(auth.CookieName(s.deps.Config)); ok {
		keep = auth.SHA256([]byte(raw))
	}
	if _, err := s.deps.Store.RevokeOtherSessions(ctx, c.User.ID, keep); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	s.sessionCache.Purge()
	return &gen.ChangePasswordNoContent{}, nil
}

// serveStream is the event stream, filtered to the caller's organizations.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	a, err := s.access(r.Context())
	if err != nil {
		s.writeError(r.Context(), w, r, err)
		return
	}
	orgs := make([]string, 0, len(a.OrgIDs()))
	for _, o := range a.OrgIDs() {
		orgs = append(orgs, o.String())
	}
	if err := stream.Serve(w, r, projection.NewSource(s.deps.Projections, s.deps.Health.Metrics()), orgs); err != nil {
		s.deps.Logger.Debug("event stream ended", "error", err)
	}
}

package httpapi

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// touchTokenEvery: a token's last_used_at is written at most this often.
const touchTokenEvery = 60_000

// session resolves the session cookie or bearer token into the request's
// principal (auth/mod.rs session_middleware). It never refuses by itself:
// handlers decide whether they need a principal.
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.resolveUser(r); ok {
			r = r.WithContext(httpx.WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) resolveUser(r *http.Request) (httpx.CurrentUser, bool) {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.userFromToken(r.Context(), strings.TrimSpace(bearer))
	}
	c, err := r.Cookie(auth.CookieName(s.deps.Config))
	if err != nil {
		return httpx.CurrentUser{}, false
	}
	return s.userFromSession(r.Context(), c.Value)
}

func (s *Server) userFromSession(ctx context.Context, raw string) (httpx.CurrentUser, bool) {
	idHash := auth.SHA256([]byte(raw))
	key := string(idHash)
	userID, cached := s.sessionCache.Get(key)
	if !cached {
		session, found, err := s.deps.Store.FindSession(ctx, idHash)
		if err != nil || !found || !session.IsValidAt(s.deps.Clock.NowMs()) {
			return httpx.CurrentUser{}, false
		}
		_ = s.deps.Store.TouchSession(ctx, idHash) //nolint:errcheck // best effort, as in Rust
		s.sessionCache.Add(key, session.UserID)
		userID = session.UserID
	}
	user, found, err := s.deps.Store.FindUserByID(ctx, userID)
	if err != nil || !found || !user.IsActive {
		return httpx.CurrentUser{}, false
	}
	return httpx.CurrentUser{User: user, Via: httpx.ViaSession}, true
}

func (s *Server) userFromToken(ctx context.Context, token string) (httpx.CurrentUser, bool) {
	id, secret, ok := auth.ParseAPIToken(token)
	if !ok {
		return httpx.CurrentUser{}, false
	}
	record, found, err := s.deps.Store.FindToken(ctx, id)
	if err != nil || !found || !auth.SecretMatches(secret, record.SecretHash) {
		return httpx.CurrentUser{}, false
	}
	now := s.deps.Clock.NowMs()
	owner, hasOwner := record.Owner.Get()
	if !record.IsUsableAt(now) || !hasOwner {
		return httpx.CurrentUser{}, false
	}
	user, found, err := s.deps.Store.FindUserByID(ctx, owner)
	if err != nil || !found || !user.IsActive {
		return httpx.CurrentUser{}, false
	}
	if last, ok := record.LastUsedAt.Get(); !ok || now-last > touchTokenEvery {
		_ = s.deps.Store.TouchToken(ctx, id) //nolint:errcheck // best effort, as in Rust
	}
	return httpx.CurrentUser{
		User:  user,
		Via:   httpx.ViaToken,
		Token: opt.Some(httpx.TokenGrant{ID: id, Org: record.OrgID, Scope: record.Scope}),
	}, true
}

// loginThrottle is auth/throttle.rs: fixed windows over three buckets —
// (email, ip), ip, and email — kept in the database so every replica
// counts against the same budget; bucket keys are hashed before storage.
type loginThrottle struct {
	store                   *store.Store
	clock                   clock.Clock
	logger                  *slog.Logger
	windowMs                int64
	perPair, perIP, perAcct uint32
}

func newLoginThrottle(cfg config.SecurityCfg, st *store.Store, c clock.Clock, logger *slog.Logger) *loginThrottle {
	secs := max(cfg.LoginWindowSecs, 1)
	windowMs := int64(1<<63 - 1)
	if secs < uint64(windowMs/1000) {
		windowMs = int64(secs) * 1000 //nolint:gosec // bounded above
	}
	return &loginThrottle{
		store: st, clock: c, logger: logger, windowMs: windowMs,
		perPair: max(cfg.LoginMaxFailures, 1),
		perIP:   max(cfg.LoginMaxFailuresPerIP, 1),
		perAcct: max(cfg.LoginMaxFailuresPerAccount, 1),
	}
}

func bucketKey(key string) string {
	return base64.RawURLEncoding.EncodeToString(auth.SHA256([]byte(key)))
}

type bucket struct {
	key   string
	limit uint32
}

func (t *loginThrottle) buckets(email string, ip opt.Val[string]) [3]bucket {
	addr := ip.Or("unknown")
	return [3]bucket{
		{bucketKey("pair:" + email + "|" + addr), t.perPair},
		{bucketKey("ip:" + addr), t.perIP},
		{bucketKey("acct:" + email), t.perAcct},
	}
}

// check is the seconds to wait when a bucket is exhausted, 0 otherwise. A
// database error lets the attempt through: the login needs the same
// database and fails right after.
func (t *loginThrottle) check(ctx context.Context, email string, ip opt.Val[string]) uint64 {
	now := t.clock.NowMs()
	var retryAfter uint64
	for _, b := range t.buckets(email, ip) {
		w, found, err := t.store.ThrottleWindow(ctx, b.key)
		if err != nil {
			t.logger.Error("login throttle unavailable", "error", err)
			continue
		}
		if !found {
			continue
		}
		elapsed := now - w.StartedAt
		if elapsed < t.windowMs && w.Failures >= int64(b.limit) {
			remaining := uint64(max(t.windowMs-elapsed, 0)) //nolint:gosec // not negative
			retryAfter = max(retryAfter, (remaining+999)/1000)
		}
	}
	return retryAfter
}

func (t *loginThrottle) recordFailure(ctx context.Context, email string, ip opt.Val[string]) {
	now := t.clock.NowMs()
	windowStart := clock.SaturatingSub(now, t.windowMs)
	for _, b := range t.buckets(email, ip) {
		if err := t.store.ThrottleRecordFailure(ctx, b.key, now, windowStart); err != nil {
			t.logger.Error("failed to record a login failure", "error", err)
		}
	}
	// Expired windows are dead weight; drop them while we are here.
	if _, err := t.store.ThrottlePurge(ctx, windowStart); err != nil {
		t.logger.Warn("failed to purge expired login windows", "error", err)
	}
}

// recordSuccess clears only the (email, ip) bucket: a success must not
// reset what an attacker accumulated from other places.
func (t *loginThrottle) recordSuccess(ctx context.Context, email string, ip opt.Val[string]) {
	if err := t.store.ThrottleClear(ctx, t.buckets(email, ip)[0].key); err != nil {
		t.logger.Warn("failed to clear a login window", "error", err)
	}
}

package api

import (
	"net/http"
	"slices"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/httpapi/problem"
)

// Operations whose Rust handler has no authentication extractor: anyone
// may call them (the list is derived from crates/kuben-api and pinned by
// a test).
var publicOperations = []string{
	"finishSso", "getPublicStatus", "getSsoInfo", "login", "logout", "setup", "setupStatus", "startSso",
}

// Operations that need a signed-in user but not the authorization
// extractor, so they stay open to a user who must still replace a
// temporary password.
var userOperations = []string{"changePassword", "getHealthDetails", "getMe"}

// gate refuses a request before its body or parameters are read, in the
// order axum ran the extractors of the Rust handlers: no principal is
// 401, a principal who must replace a temporary password is 403 (except
// on the user operations), and only then is the request decoded — so an
// anonymous client learns nothing from validation errors.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, found := s.routes.FindRoute(r.Method, r.URL.Path)
		if !found || slices.Contains(publicOperations, route.OperationID()) {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := httpx.UserFrom(r.Context())
		switch {
		case !ok:
			problem.Write(w, s.deps.Logger, kerrors.ErrUnauthorized)
		case u.User.MustChangePassword && !slices.Contains(userOperations, route.OperationID()):
			problem.Write(w, s.deps.Logger, kerrors.ErrForbidden)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

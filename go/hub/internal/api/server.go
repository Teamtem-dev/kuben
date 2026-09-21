package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/problem"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/stream"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/web"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// Deps are what the API needs from the rest of the server.
type Deps struct {
	Config config.Config
	Store  *store.Store
	Hasher *auth.Hasher
	Health *health.Health
	// Stream feeds /api/v1/stream; stream.Empty{} without a cluster.
	Stream stream.Source
	// Console serves every non-API path; web.New() in production.
	Console http.Handler
	Clock   clock.Clock
	Logger  *slog.Logger
	// InCluster is config.InCluster(), injectable for tests.
	InCluster bool
}

// Server implements the generated handler interface. Operations not ported
// yet answer 501 through the embedded UnimplementedHandler.
type Server struct {
	gen.UnimplementedHandler

	deps         Deps
	policy       authz.Policy
	throttle     *loginThrottle
	sessionCache *expirable.LRU[string, ids.UserID]
	loginPermits chan struct{} // bounds concurrent password hashing
	setupMu      sync.Mutex    // one first-run setup at a time
	routes       *gen.Server
}

// New builds the API on deps.
func New(deps Deps) (*Server, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Stream == nil {
		deps.Stream = stream.Empty{}
	}
	if deps.Console == nil {
		deps.Console = web.New()
	}
	sec := deps.Config.Security
	s := &Server{
		deps:         deps,
		policy:       authz.RolePolicy{},
		throttle:     newLoginThrottle(sec, deps.Store, deps.Clock, deps.Logger),
		sessionCache: expirable.NewLRU[string, ids.UserID](10_000, nil, time.Duration(sec.SessionCacheTTLSecs)*time.Second),
		loginPermits: make(chan struct{}, max(sec.LoginConcurrency, 1)),
	}
	routes, err := gen.NewServer(s,
		gen.WithErrorHandler(s.writeError),
		gen.WithNotFound(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, s.deps.Logger, kerr.New(kerr.NotFound, "no route for %s", r.URL.Path))
		}),
		gen.WithMethodNotAllowed(func(w http.ResponseWriter, _ *http.Request, allowed string) {
			w.Header().Set("Allow", allowed)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}),
	)
	if err != nil {
		return nil, err //nolint:wrapcheck // ogen's own error
	}
	s.routes = routes
	return s, nil
}

// writeError answers a failed operation as a problem. Requests the
// generated decoder refuses (malformed JSON, missing or invalid members)
// are validation failures.
func (s *Server) writeError(ctx context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	var decode *ogenerrors.DecodeRequestError
	var params *ogenerrors.DecodeParamsError
	switch {
	case errors.As(err, &decode), errors.As(err, &params):
		err = kerr.New(kerr.Validation, "%s", err.Error())
	case store.IsUniqueViolation(err):
		err = kerr.New(kerr.Conflict, "it already exists")
	case errors.Is(err, ht.ErrNotImplemented):
		problem.WriteProblem(w, problem.Problem{
			Code: "not_implemented", Title: http.StatusText(http.StatusNotImplemented),
			Status: http.StatusNotImplemented, Detail: "this operation is not part of this build yet",
		})
		return
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return // the client went away
	}
	problem.Write(w, s.deps.Logger, err)
}

// Handler is the whole HTTP surface: the API behind its middleware, the
// event stream, the probes and the console.
func (s *Server) Handler() http.Handler {
	sec := s.deps.Config.Security
	forbidden := func(w http.ResponseWriter) { problem.Write(w, s.deps.Logger, kerr.ErrForbidden) }

	var rest http.Handler = s.routes
	rest = s.gate(rest)
	rest = s.audit(rest)
	rest = http.TimeoutHandler(rest, time.Duration(s.deps.Config.Server.RequestTimeoutSecs)*time.Second,
		`{"code":"timeout","title":"Request Timeout","status":408}`)
	rest = limitBody(int64(min(s.deps.Config.Server.MaxBodyBytes, 1<<40)), rest) //nolint:gosec // bounded

	api := http.NewServeMux()
	api.Handle("GET /api/v1/stream", http.HandlerFunc(s.serveStream))
	api.Handle("/api/", rest)
	guarded := s.session(httpx.CSRFGuard(forbidden, api))

	mux := http.NewServeMux()
	mux.Handle("/api/", guarded)
	mux.HandleFunc("GET /livez", s.livez)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("/", s.deps.Console)
	return httpx.Wrap(sec, recoverPanics(s.deps.Logger, mux))
}

// limitBody caps request bodies (tower-http's RequestBodyLimitLayer).
func limitBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a panic in a handler into a logged 500: one bad
// request must not take the server down.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // a sentinel re-panicked on purpose
					panic(v)
				}
				logger.Error("handler panicked", "panic", v, "path", r.URL.Path)
				problem.Write(w, nil, kerr.New(kerr.Internal, "panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

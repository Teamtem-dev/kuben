package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/problem"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/web"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// DNSResolver resolves hostnames: net.DefaultResolver in production, mocked in tests.
type DNSResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

type systemResolver struct{}

func (systemResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, host)
}

// Deps are what the API needs from the rest of the server.
type Deps struct {
	Config config.Config
	Store  *store.Store
	Hasher *auth.Hasher
	Health *health.Health
	// Cluster is the registry of clusters, absent when Kuben runs without
	// one (setup and diagnosis only).
	Cluster opt.Val[*registry.Registry]
	// Projections are the read models of the cluster; they feed
	// /api/v1/stream and the readiness of projects, environments and apps.
	// Empty without a cluster; a fresh set when nil.
	Projections *projection.Projections
	// Images resolves image tags to digests: the images' registries
	// (oci.Registry{}) when nil; tests give fixed answers (oci.Fixed).
	Images oci.Resolver
	// Console serves every non-API path; web.New() in production.
	Console http.Handler
	Clock   clock.Clock
	Logger  *slog.Logger
	// InCluster is config.InCluster(), injectable for tests.
	InCluster bool
	// Resolver checks DNS records for apps/domains.rs; systemResolver when nil.
	Resolver DNSResolver
}

// Server implements the generated handler interface. Operations not ported
// yet answer 501 through the embedded UnimplementedHandler.
type Server struct {
	gen.UnimplementedHandler

	deps         Deps
	policy       authz.Policy
	throttle     *loginThrottle
	sessionCache *expirable.LRU[string, ids.UserID]
	statusCache  *expirable.LRU[string, *gen.PublicStatus]
	loginPermits chan struct{} // bounds concurrent password hashing
	setupMu      sync.Mutex    // one first-run setup at a time
	logStreams   *logStreams
	routes       *gen.Server
}

// statusCacheTTL is how long a public status answer is reused (routes/status.rs CACHE_TTL).
const statusCacheTTL = 15 * time.Second

// statusCacheCapacity is how many public status responses are cached in memory.
const statusCacheCapacity = 1_000

// New builds the API on deps.
func New(deps Deps) (*Server, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Projections == nil {
		deps.Projections = projection.New()
	}
	if deps.Images == nil {
		deps.Images = oci.Registry{}
	}
	if deps.Console == nil {
		deps.Console = web.New()
	}
	if deps.Resolver == nil {
		deps.Resolver = systemResolver{}
	}
	sec := deps.Config.Security
	s := &Server{
		deps:         deps,
		policy:       authz.RolePolicy{},
		throttle:     newLoginThrottle(sec, deps.Store, deps.Clock, deps.Logger),
		sessionCache: expirable.NewLRU[string, ids.UserID](10_000, nil, clock.Seconds(sec.SessionCacheTTLSecs)),
		statusCache:  expirable.NewLRU[string, *gen.PublicStatus](statusCacheCapacity, nil, statusCacheTTL),
		loginPermits: make(chan struct{}, max(sec.LoginConcurrency, 1)),
		logStreams:   newLogStreams(),
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
	if errors.Is(err, errStreamHandled) {
		return
	}
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
	timed := http.TimeoutHandler(rest, clock.Seconds(s.deps.Config.Server.RequestTimeoutSecs),
		`{"code":"timeout","title":"Request Timeout","status":408}`)
	unwrapped := rest
	rest = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") == "true" && strings.HasSuffix(r.URL.Path, "/logs") {
			unwrapped.ServeHTTP(w, r)
			return
		}
		timed.ServeHTTP(w, r)
	})
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
					panic(v) //nolint:forbidigo // net/http's way to abort a response
				}
				logger.Error("handler panicked", "panic", v, "path", r.URL.Path)
				problem.Write(w, nil, kerr.New(kerr.Internal, "panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

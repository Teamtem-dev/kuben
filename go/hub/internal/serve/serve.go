// Package serve runs the server (crates/kuben/src/serve.rs): configuration
// to listening socket, the shared state, background work, and the ordered
// shutdown. Wired so far: the database, health, the API and the console,
// and with a cluster the informers, the readiness gate, capability
// discovery and (once reconcilers exist) the controller Lease.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
)

// Logger is the process logger the configuration asks for (JSON by
// default, text for `pretty`).
func Logger(cfg config.TelemetryCfg) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "pretty" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// Run serves until ctx ends (a signal), then shuts down in order.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	h := health.New(clock.System{})
	watchdog(ctx, h)

	st, err := store.ConnectUnmigrated(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	st = st.WithLogger(logger)
	defer st.Close()
	migrated, err := st.MigrateJournaled(ctx, version.Version)
	if err != nil {
		return fmt.Errorf("database migrations: %w", err)
	}
	databaseReady(ctx, cfg, st, migrated, logger)
	cluster, err := registry.FromConfig(cfg.Kube, logger)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	elect, err := election(cfg)
	if err != nil {
		return err
	}

	inCluster := config.InCluster()
	announceSetup(ctx, cfg, st, inCluster, logger)

	projections := projection.New()
	var subsystems []<-chan struct{}
	if r, ok := cluster.Get(); ok {
		subsystems = clusterWork{
			cfg: cfg, registry: r, projections: projections, store: st, health: h, logger: logger,
			election: elect, facts: &discovery.Watch{},
		}.start(ctx)
	} else {
		// Nothing to sync without a cluster: serve setup and diagnostics now.
		h.Degrade("cluster", "no kubernetes cluster configured")
		h.SetReady(true)
	}

	var serveErr error
	if cfg.HasRole(config.RoleAPI) {
		server, err := api.New(api.Deps{
			Config:      cfg,
			Store:       st,
			Hasher:      auth.HasherFromConfig(cfg.Security),
			Health:      h,
			Cluster:     cluster,
			Projections: projections,
			Logger:      logger,
			InCluster:   inCluster,
		})
		if err != nil {
			return fmt.Errorf("api: %w", err)
		}
		serveErr = listen(ctx, cfg, server.Handler(), h, logger)
	} else {
		<-ctx.Done()
		logger.Info("shutting down")
		h.SetReady(false)
	}
	// The subsystems run on ctx, which has ended: give each the same ten
	// seconds Rust gave its tasks.
	waitAll(subsystems, 10*time.Second, logger)
	if serveErr == nil {
		logger.Info("bye")
	}
	return serveErr
}

// waitAll waits for every channel to close, at most timeout in total.
func waitAll(done []<-chan struct{}, timeout time.Duration, logger *slog.Logger) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, d := range done {
		select {
		case <-d:
		case <-deadline.C:
			logger.Warn("background work did not stop in time")
			return
		}
	}
}

func listen(ctx context.Context, cfg config.Config, handler http.Handler, h *health.Health, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.Server.Bind)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", cfg.Server.Bind, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	logger.Info("kuben listening", "bind", cfg.Server.Bind, "roles", cfg.Server.Roles, "version", version.Version)
	if warning, ok := cfg.InsecureCookieWarning(config.InCluster()); ok {
		logger.Warn(warning)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }() //nolint:forbidigo // owned: shut down below
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	// Ordered shutdown (Invariant I-15): not ready first, so load balancers
	// stop sending, then drain.
	logger.Info("shutting down")
	h.SetReady(false)
	time.Sleep(time.Second)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil {
		logger.Warn("connections did not drain", "error", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// watchdog beats every second; /livez fails when the beats stop.
func watchdog(ctx context.Context, h *health.Health) {
	go func() { //nolint:forbidigo // owned: ends with ctx
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				h.Heartbeat()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func databaseReady(ctx context.Context, cfg config.Config, st *store.Store, m store.Migrated, logger *slog.Logger) {
	logger.Info("database ready", "backend", st.Backend(), "url", redactCredentials(cfg.Database.URL.Expose()),
		"from_schema", m.FromSchema, "to_schema", m.ToSchema)
	if role, bypasses, err := st.RoleBypassingRowSecurity(ctx); err == nil && bypasses {
		logger.Warn("the database role is a superuser or has BYPASSRLS: row-level security does not apply; connect as an ordinary role",
			"role", role)
	}
}

// redactCredentials hides the password of a database URL for logs.
func redactCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return strings.Replace(u.String(), "%2A%2A%2A", "***", 1)
}

// announceSetup tells the operator where to create the first admin.
func announceSetup(ctx context.Context, cfg config.Config, st *store.Store, inCluster bool, logger *slog.Logger) {
	if !cfg.SetupWizard(inCluster) {
		logger.Warn("bootstrap.admin_password is not handled by this build yet; create the admin from a Rust 1.2 server or finish the setup wizard")
		return
	}
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return
	}
	token := opt.None[string]()
	if api.SetupTokenRequired(cfg) {
		t, err := api.CurrentOrNewSetupToken(cfg, time.Now())
		if err != nil {
			logger.Error("cannot write the setup token", "error", err, "file", api.SetupTokenPath(cfg))
			return
		}
		token = opt.Some(t)
	}
	host := AdvertiseIP().Or("localhost")
	logger.Warn("no admin account yet: finish the setup in the browser",
		"url", api.SetupURLAt(cfg.ConsoleURLWithHost(host), token))
}

// AdvertiseIP is this machine's address as other machines reach it (the
// source address of a route to the internet; nothing is sent), for links
// in logs and the setup banner (host.rs advertise_ip).
func AdvertiseIP() opt.Val[string] {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return opt.None[string]()
	}
	defer conn.Close() //nolint:errcheck // nothing was sent
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return opt.None[string]()
	}
	return opt.Some(addr.IP.String())
}

// Package serve runs the server (crates/kuben/src/serve.rs): configuration
// to listening socket, the shared state, background work, and the ordered
// shutdown. Wired so far: the database, health, the API and the console,
// and with a cluster the informers, the readiness gate, capability
// discovery, the materializer and, behind the controller Lease, the
// reconcilers and the drift watch; on controller replicas AgentLink, the
// endpoint cluster agents dial, and with builds enabled the build worker
// and the rescans.
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

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/firstrun"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/host"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/internal/integrations/sso"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/maintenance/upgrade"
	"github.com/Teamtem-dev/kuben/internal/metrics"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/usage"
	"github.com/Teamtem-dev/kuben/internal/version"
)

// Logger is the process logger the configuration asks for (telemetry.rs
// init): JSON by default, text for `pretty`. The level is RUST_LOG's when
// set, else `telemetry.log_level`; of an EnvFilter directive list
// (`debug,hyper=info`) the global level counts, per-target levels do not.
func Logger(cfg config.TelemetryCfg) *slog.Logger {
	directives := cfg.LogLevel
	if env := os.Getenv("RUST_LOG"); env != "" {
		directives = env
	}
	opts := &slog.HandlerOptions{Level: LogLevel(directives)}
	if cfg.LogFormat == "pretty" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// LogLevel is the global level of an EnvFilter directive list: its last
// directive without a target; info when there is none. `trace` is debug.
func LogLevel(directives string) slog.Level {
	level := slog.LevelInfo
	for d := range strings.SplitSeq(directives, ",") {
		d = strings.TrimSpace(d)
		if d == "" || strings.ContainsAny(d, "=[{:") {
			continue
		}
		switch strings.ToLower(d) {
		case "trace", "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error", "off":
			level = slog.LevelError
		}
	}
	return level
}

// Run serves until ctx ends (a signal), then shuts down in order.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	// controller-runtime's sources and caches log through its process-wide
	// logger; unset, it warns on stderr and drops their logs. Set once, by
	// the process owner.
	ctrllog.SetLogger(logr.FromSlogHandler(logger.Handler()))
	h := health.New(clock.System{})
	watchdog(ctx, h)
	// Owned by ctx: the exporter shuts down with the server.
	_ = metrics.Serve(ctx, h.Metrics(), cfg.Server.MetricsBind, logger)

	st, err := store.ConnectUnmigrated(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	st = st.WithLogger(logger)
	defer st.Close()
	migrated, err := upgrade.Migrate(ctx, cfg, st, logger)
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
	if err := firstAdmin(ctx, cfg, st, cluster, inCluster, logger); err != nil {
		return err
	}
	keyring, err := secretKeyring(ctx, cfg, st, logger)
	if err != nil {
		return err
	}
	link, err := agentLink(ctx, cfg, st, cluster, logger)
	if err != nil {
		return err
	}
	app, err := githubApp(cfg, logger)
	if err != nil {
		return err
	}

	projections := projection.New()
	var subsystems []<-chan struct{}
	if r, ok := cluster.Get(); ok {
		subsystems = clusterWork{
			cfg: cfg, registry: r, projections: projections, store: st, health: h, logger: logger,
			election: elect, facts: &discovery.Watch{}, keyring: keyring, agents: agentDispatch(link),
		}.start(ctx)
	} else {
		// Nothing to sync without a cluster: serve setup and diagnostics now.
		h.Degrade("cluster", "no kubernetes cluster configured")
		h.SetReady(true)
	}
	subsystems = append(subsystems, startBackground(ctx, cfg, st, keyring, app, h, logger)...)
	subsystems = append(subsystems, startMaintenance(ctx, cfg, st, h, logger)...)
	warnActivator(cfg, logger)
	subsystems = append(subsystems, startAgentLink(ctx, cfg, st, cluster, link, h, logger)...)
	builds, err := startBuilds(ctx, cfg, st, cluster, app, h, logger)
	if err != nil {
		return err
	}
	subsystems = append(subsystems, builds...)

	var serveErr error
	if cfg.HasRole(config.RoleAPI) {
		serveErr = serveAPI(ctx, cfg, cluster, st, h, app, projections, keyring, inCluster, &subsystems, logger)
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

func serveAPI(ctx context.Context, cfg config.Config, cluster opt.Val[*registry.Registry], st *store.Store, h *health.Health, app opt.Val[*github.App], projections *projection.Projections, keyring *keyring.Keyring, inCluster bool, subsystems *[]<-chan struct{}, logger *slog.Logger) error {
	live := opt.None[*usage.Buffer]()
	if r, ok := cluster.Get(); ok {
		buffer, done := startUsage(ctx, r.Primary(), st, h, logger)
		live = opt.Some(buffer)
		*subsystems = append(*subsystems, done)
	}
	single, err := ssoClient(cfg, logger)
	if err != nil {
		return err
	}
	server, err := api.New(api.Deps{
		SSO:         single,
		GitHub:      app,
		Usage:       live,
		Config:      cfg,
		Store:       st,
		Hasher:      auth.HasherFromConfig(cfg.Security),
		Health:      h,
		Cluster:     cluster,
		Projections: projections,
		Logger:      logger,
		InCluster:   inCluster,
		Keyring:     opt.Some(keyring),
	})
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	return listen(ctx, cfg, server.Handler(), h, logger)
}

func listen(ctx context.Context, cfg config.Config, handler http.Handler, h *health.Health, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.Bind)
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

// firstAdmin creates the first admin, or says how to (serve.rs): with the
// setup wizard the account is created from the console and nothing is
// seeded; otherwise the configured or a generated password is used, and a
// generated one is handed over exactly once.
func firstAdmin(ctx context.Context, cfg config.Config, st *store.Store, cluster opt.Val[*registry.Registry], inCluster bool, logger *slog.Logger) error {
	stderr := firstrun.Stderr{
		Write:      func(s string) { fmt.Fprint(os.Stderr, s) }, //nolint:errcheck // the operator's terminal
		IsTerminal: stderrIsTerminal(),
	}
	if cfg.SetupWizard(inCluster) {
		n, err := st.CountUsers(ctx)
		if err != nil {
			return fmt.Errorf("count users: %w", err)
		}
		if n == 0 {
			firstrun.AnnounceSetup(cfg, stderr, host.AdvertiseIP(ctx), time.UnixMilli(clock.System{}.NowMs()), logger)
		}
		return nil
	}
	password, err := firstrun.EnsureAdmin(ctx, cfg, st, auth.HasherFromConfig(cfg.Security), logger)
	if err != nil {
		return fmt.Errorf("bootstrap the admin: %w", err)
	}
	if p, ok := password.Get(); ok {
		firstrun.HandOverPassword(ctx, cfg, cluster, p, stderr, logger)
	}
	return nil
}

// stderrIsTerminal says whether standard error is a character device (a
// terminal), as Rust's IsTerminal did.
func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	if err != nil || info == nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ssoClient is single sign-on (M4.3), when enabled. A broken configuration
// stops the server instead of silently offering password sign-in only.
func ssoClient(cfg config.Config, logger *slog.Logger) (opt.Val[*sso.Client], error) {
	if !cfg.SSO.Enabled {
		return opt.None[*sso.Client](), nil
	}
	c, err := sso.FromConfig(cfg.SSO, cfg.Server.PublicURL, cfg.Bootstrap.OrgSlug)
	if err != nil {
		return opt.None[*sso.Client](), fmt.Errorf("single sign-on: %w", err)
	}
	logger.Info("single sign-on enabled", "issuer", c.Issuer(), "org", c.OrgSlug())
	return opt.Some(c), nil
}

// Package doctor is `kuben doctor`, the preflight checks, the port of
// crates/kuben/src/cli/doctor.rs. Each check prints OK / WARN / FAIL and
// the command fails on any FAIL. Every line passes through
// [registry.RedactCredentials]: URLs in errors may carry passwords.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/compat"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/maintenance/backup"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// ErrFailures is the error of a run that found a failure.
var ErrFailures = errors.New("doctor found failures") //nolint:gochecknoglobals // sentinel

// Level is the verdict of one line.
type Level int

// The levels of a line.
const (
	LevelOK Level = iota
	LevelWarn
	LevelFail
)

// Report prints the lines of a run and remembers whether one failed.
type Report struct {
	w      io.Writer
	failed bool
	// err is the first write that failed.
	err error
}

// NewReport is a report printing to w.
func NewReport(w io.Writer) *Report { return &Report{w: w} }

// Failed reports whether a line failed.
func (r *Report) Failed() bool { return r.failed }

// Line prints `[TAG] name: detail`, detail redacted.
func (r *Report) Line(level Level, name, detail string) {
	var tag string
	switch level {
	case LevelOK:
		tag = "OK  "
	case LevelWarn:
		tag = "WARN"
	case LevelFail:
		r.failed = true
		tag = "FAIL"
	}
	if _, err := fmt.Fprintf(r.w, "[%s] %s: %s\n", tag, name, registry.RedactCredentials(detail)); err != nil && r.err == nil {
		r.err = err
	}
}

// Options are the options of `kuben doctor` (DoctorOpts).
type Options struct {
	// Cluster checks only the cluster (before installing Kuben into it,
	// BYOK): what each feature needs, what is there, and the permissions
	// Kuben needs. No database is needed.
	Cluster bool
}

// Run is `kuben doctor`; build is the version line it starts with.
func Run(ctx context.Context, cfg config.Config, opts Options, build string, stdout io.Writer, logger *slog.Logger) error {
	r := NewReport(stdout)
	if _, err := fmt.Fprintln(stdout, build); err != nil {
		return err //nolint:wrapcheck // stdout
	}
	if opts.Cluster {
		clusterChecks(ctx, r, cfg, false, logger)
		return r.result()
	}
	st, err := store.Connect(ctx, cfg.Database)
	if err == nil {
		r.Line(LevelOK, "database", fmt.Sprintf("%s reachable, migrations applied (%s)", st.Backend(), cfg.Database.URL.Expose()))
		roleCheck(ctx, r, st)
		databaseChecks(ctx, r, cfg, st)
		st.Close()
	} else {
		// The URL goes through RedactCredentials like every line.
		r.Line(LevelFail, "database", fmt.Sprintf("%v (url: %s; set KUBEN_DATABASE__URL to change it)", err, cfg.Database.URL.Expose()))
	}
	clusterChecks(ctx, r, cfg, true, logger)
	cookieCheck(r, cfg)
	return r.result()
}

// result is the error a finished run returns.
func (r *Report) result() error {
	if r.err != nil {
		return r.err
	}
	if r.failed {
		return ErrFailures
	}
	return nil
}

func roleCheck(ctx context.Context, r *Report, st *store.Store) {
	role, bypasses, err := st.RoleBypassingRowSecurity(ctx)
	switch {
	case err != nil:
		r.Line(LevelWarn, "database role", fmt.Sprintf("cannot read the role: %v", err))
	case bypasses:
		r.Line(LevelWarn, "database role", fmt.Sprintf("`%s` is a superuser or has BYPASSRLS: row-level security, the second wall "+
			"between organizations, does not apply. Connect as an ordinary role that owns "+
			"the database (the Helm chart and `kuben setup` make one)", role))
	default:
		r.Line(LevelOK, "database role", "row-level security applies")
	}
}

func cookieCheck(r *Report, cfg config.Config) {
	_, auto := cfg.Security.CookieSecure.(config.CookieAuto)
	warning, hasWarn := cfg.InsecureCookieWarning(config.InCluster())
	switch {
	case hasWarn:
		r.Line(LevelWarn, "cookies", warning)
	case cfg.CookieSecure():
		r.Line(LevelOK, "cookies", "Secure + HttpOnly (__Host- prefix)")
	case auto:
		r.Line(LevelOK, "cookies", "HttpOnly, not Secure: the console is served over plain http (auto; becomes Secure once "+
			"KUBEN_SERVER__PUBLIC_URL is https)")
	default:
		r.Line(LevelWarn, "cookies", "Secure flag disabled — development only")
	}
}

// databaseChecks is the database server, point-in-time recovery and
// backups (M4.7).
func databaseChecks(ctx context.Context, r *Report, cfg config.Config, st *store.Store) {
	if facts, err := st.DatabaseFacts(ctx); err != nil {
		r.Line(LevelWarn, "database server", fmt.Sprintf("cannot read its facts: %v", err))
	} else {
		serverLines(r, cfg, facts)
	}
	last, found, err := st.LastBackup(ctx)
	if err != nil {
		r.Line(LevelWarn, "backup", fmt.Sprintf("cannot read the backups: %v", err))
		return
	}
	at := opt.None[int64]()
	if found {
		at = opt.Some(last.FinishedAt)
	}
	if msg, err := backup.Freshness(at, clock.System{}.NowMs(), cfg.Backup.MaxAgeHours); err != nil {
		r.Line(LevelWarn, "backup", err.Error())
	} else {
		r.Line(LevelOK, "backup", msg)
	}
}

// serverLines judges the database server's version, encryption and WAL
// archiving.
func serverLines(r *Report, cfg config.Config, facts store.DatabaseFacts) {
	url := cfg.Database.URL.Expose()
	local := strings.Contains(url, "@localhost") || strings.Contains(url, "@127.0.0.1") || strings.Contains(url, "host=/")
	major := uint32(max(facts.Major(), 0)) //nolint:gosec // not negative
	fit, fits := compat.PostgreSQL().Describe(compat.Minor{Major: major, Minor: 0})
	level := LevelOK
	if fit != compat.Supported || facts.InRecovery || (!facts.TLS && !local) {
		level = LevelWarn
	}
	transport := ", NOT encrypted: add sslmode=verify-full"
	switch {
	case facts.TLS:
		transport = ", TLS"
	case local:
		transport = ", local"
	}
	standby := ""
	if facts.InRecovery {
		standby = ", a read-only standby"
	}
	r.Line(level, "database server", fmt.Sprintf("PostgreSQL %d%s%s", facts.Major(), transport, standby))
	if fit != compat.Supported {
		r.Line(LevelWarn, "support envelope", fits)
	}
	if facts.ArchivesWAL() {
		r.Line(LevelOK, "point-in-time recovery", "WAL is archived (wal_level and archive_mode)")
	} else {
		r.Line(LevelWarn, "point-in-time recovery",
			"WAL is not archived: recovery goes back to the newest backup only; archive WAL "+
				"(pgBackRest, WAL-G or your provider's PITR) for a smaller recovery point")
	}
}

// clusterChecks is the cluster: reachable, and what each feature needs of
// it.
func clusterChecks(ctx context.Context, r *Report, cfg config.Config, installed bool, logger *slog.Logger) {
	if u, ok := UnreadableKubeconfig(cfg.Kube); ok {
		r.Line(LevelFail, "kubernetes", fmt.Sprintf(
			"cannot read the kubeconfig %s: %s. k3s writes it for root only; give this user a copy: "+
				"sudo install -D -m 600 -o \"$USER\" %s ~/.kube/config && export KUBECONFIG=~/.kube/config",
			u.Path, u.Reason, u.Path))
		return
	}
	checkCluster(ctx, r, cfg, installed, logger)
}

// Unreadable is a kubeconfig that exists but cannot be read.
type Unreadable struct {
	Path string
	// Reason is the system's, e.g. `permission denied`.
	Reason string
}

// UnreadableKubeconfig is a kubeconfig that is configured and exists but
// cannot be read, like the root-only `/etc/rancher/k3s/k3s.yaml` from a
// non-root shell. Without this check it would read as "no cluster found".
func UnreadableKubeconfig(kube config.KubeCfg) (Unreadable, bool) {
	var paths []string
	if path, ok := kube.Kubeconfig.Get(); ok {
		paths = []string{path}
	} else if list, ok := os.LookupEnv("KUBECONFIG"); ok {
		paths = filepath.SplitList(list)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path) //nolint:gosec // the configured kubeconfig
		if err != nil {
			reason := err.Error()
			if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
				reason = pe.Err.Error()
			}
			return Unreadable{Path: path, Reason: reason}, true
		}
		_ = f.Close() //nolint:errcheck // only opening it mattered
	}
	return Unreadable{}, false
}

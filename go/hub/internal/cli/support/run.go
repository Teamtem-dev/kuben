package support

// `kuben support-bundle` itself: the sections, the preview, the file and
// its audit record.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	coresupport "github.com/Teamtem-dev/kuben/go/hub/internal/core/support"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// Options are the options of `kuben support-bundle` (SupportOpts).
type Options struct {
	// Out is the directory the bundle goes to; default `support` in the
	// state dir.
	Out opt.Val[string]
	// Preview shows what the bundle would hold, and writes nothing.
	Preview bool
	// Logs adds the newest lines of Kuben's own logs.
	Logs bool
	// LogLines is the log lines per container with Logs (1 to
	// [MaxLogLines]).
	LogLines uint32
	// Keep is the bundles kept in the directory; older ones are removed.
	Keep uint32
}

// Env is what the command gets from the command line around it.
type Env struct {
	// ConfigPath is `--config`, passed on to the `kuben doctor` it runs.
	ConfigPath opt.Val[string]
	// Build is the version line (`kuben <version> (<os> <arch>)`).
	Build  string
	Stdout io.Writer
	Stderr io.Writer
}

// Run is `kuben support-bundle`.
func Run(ctx context.Context, cfg config.Config, opts Options, env Env) error {
	c := clock.System{}
	sections := map[string]any{
		"about": map[string]any{
			"kuben":            version.Version,
			"build":            env.Build,
			"generated_at":     c.NowMs(),
			"in_cluster":       config.InCluster(),
			"format":           1,
			"support_envelope": coresupport.Envelope(),
		},
		"config": AllowedConfig(cfg),
		"doctor": doctorSection(ctx, env.ConfigPath),
	}
	st, storeErr := store.ConnectUnmigrated(ctx, cfg.Database)
	if storeErr == nil && st != nil {
		defer st.Close()
		sections["database"] = databaseSection(ctx, st)
	} else {
		sections["database"] = map[string]any{"error": "the database is not reachable"}
	}
	clusterSections(ctx, cfg, opts, sections)
	for name, value := range sections {
		normal, err := generic(value)
		if err != nil {
			return err
		}
		sections[name] = Redacted(normal)
	}
	data, err := Encode(sections, MaxBundleBytes)
	if err != nil {
		return err
	}
	if opts.Preview {
		return printPreview(env.Stdout, data)
	}
	dir, ok := opts.Out.Get()
	if !ok {
		dir = joinPath(cfg.StateDir(), "support")
	}
	path, sum, err := writeBundle(dir, data, time.UnixMilli(c.NowMs()))
	if err != nil {
		return err
	}
	removed, err := Prune(dir, opts.Keep)
	if err != nil {
		return err
	}
	if storeErr == nil && st != nil {
		audit(ctx, cfg, st, sum, len(data), opts.Logs, env.Stderr)
	}
	suffix := ""
	if removed > 0 {
		suffix = fmt.Sprintf("; %d older bundle(s) removed", removed)
	}
	if _, err := fmt.Fprintf(env.Stdout, "support bundle written to %s (%d KiB, sha256 %s)%s\n",
		path, len(data)>>10, sum, suffix); err != nil {
		return err //nolint:wrapcheck // stdout
	}
	_, err = fmt.Fprintln(env.Stdout, "nothing was uploaded; read the file before you share it")
	return err //nolint:wrapcheck // stdout
}

// clusterSections adds the cluster and, when asked, the logs.
func clusterSections(ctx context.Context, cfg config.Config, opts Options, sections map[string]any) {
	reg, err := registry.Connect(cfg.Kube)
	namespace := registry.OwnNamespace(cfg.Kube.Namespace).Or("kuben-system")
	if err != nil {
		sections["cluster"] = map[string]any{"error": registry.RedactCredentials(err.Error())}
	} else {
		sections["cluster"] = clusterSection(ctx, reg.Primary(), namespace)
	}
	if !opts.Logs {
		return
	}
	if err == nil && config.InCluster() {
		sections["logs"] = podLogs(ctx, reg.Primary(), namespace, opts.LogLines)
	} else {
		sections["logs"] = hostLogs(ctx, opts.LogLines)
	}
}

// printPreview says what the bundle data would hold, section by section.
func printPreview(w io.Writer, data []byte) error {
	value, err := wire.DecodeAny(data)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	sections, ok := value.(map[string]any)
	if !ok {
		return errors.New("the bundle is not a JSON object")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the bundle would hold %d KiB:\n", len(data)>>10)
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		size := 0
		if text, err := wire.CanonicalValue(sections[name]); err == nil {
			size = len(text)
		}
		fmt.Fprintf(&b, "  %-10s %7d bytes\n", name, size)
	}
	if cfg, ok := sections["config"].(map[string]any); ok {
		if omitted, ok := cfg["omitted"].([]any); ok {
			names := make([]string, 0, len(omitted))
			for _, n := range omitted {
				if s, ok := n.(string); ok {
					names = append(names, s)
				}
			}
			fmt.Fprintf(&b, "configuration left out: %s\n", strings.Join(names, ", "))
		}
	}
	b.WriteString("nothing was written (drop --preview to write it)\n")
	_, err = io.WriteString(w, b.String())
	return err //nolint:wrapcheck // stdout
}

// doctorSection is the output of `kuben doctor` with the same
// configuration.
func doctorSection(ctx context.Context, configPath opt.Val[string]) map[string]any {
	exe, err := os.Executable()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	var args []string
	if path, ok := configPath.Get(); ok {
		args = append(args, "--config", path)
	}
	args = append(args, "doctor")
	out, err := exec.CommandContext(ctx, exe, args...).Output() //nolint:gosec // this binary, arguments without a shell
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return map[string]any{"error": "doctor timed out"}
	case err != nil && !errors.As(err, &exit):
		return map[string]any{"error": err.Error()}
	}
	lines := rustLines(strings.ToValidUTF8(string(out), string(utf8.RuneError)))
	return map[string]any{"passed": err == nil, "lines": lines[:min(len(lines), 500)]}
}

// databaseSection is what the database says about itself, counts and
// codes only.
func databaseSection(ctx context.Context, st *store.Store) map[string]any {
	failed := func(err error) any { return map[string]any{"error": err.Error()} }
	var schema, server, last, versions, agents, summary any
	if s, err := st.SchemaState(ctx); err != nil {
		schema = failed(err)
	} else {
		schema = map[string]any{"applied": s.Applied, "dirty": s.Dirty}
	}
	if f, err := st.DatabaseFacts(ctx); err != nil {
		server = failed(err)
	} else {
		server = map[string]any{
			"major": f.Major(), "tls": f.TLS, "wal_level": f.WALLevel,
			"archive_mode": f.ArchiveMode, "in_recovery": f.InRecovery,
		}
	}
	if b, found, err := st.LastBackup(ctx); err != nil {
		last = failed(err)
	} else if found {
		last = map[string]any{"finished_at": b.FinishedAt, "bytes": b.Bytes}
	}
	if v, err := st.ServerVersions(ctx); err != nil {
		versions = failed(err)
	} else {
		versions = v
	}
	if a, err := st.AgentProtocols(ctx); err != nil {
		agents = failed(err)
	} else {
		pairs := make([]any, 0, len(a))
		for _, p := range a {
			pairs = append(pairs, []any{p.Cluster, p.Protocol})
		}
		agents = pairs
	}
	if s, err := st.SupportSummary(ctx); err != nil {
		summary = failed(err)
	} else {
		summary = s
	}
	return map[string]any{
		"schema":                 schema,
		"latest_known_migration": store.LatestMigration(),
		"server":                 server,
		"last_backup":            last,
		"server_versions":        versions,
		"agent_protocols":        agents,
		"summary":                summary,
	}
}

// audit records that a bundle was made.
func audit(ctx context.Context, cfg config.Config, st *store.Store, sum string, size int, logs bool, stderr io.Writer) {
	if st == nil {
		return
	}
	org := opt.None[ids.OrgID]()
	if id, ok, err := st.InstallationOrg(ctx, cfg.Bootstrap.OrgSlug); err == nil && ok {
		org = opt.Some(id)
	}
	actor, ok := os.LookupEnv("SUDO_USER")
	if !ok {
		if actor, ok = os.LookupEnv("USER"); !ok {
			actor = "unknown"
		}
	}
	record := store.NewAudit{
		OrgID: org, ActorKind: "host", ActorID: opt.Some(actor), Action: "support.bundle.created",
		TargetKind: opt.Some("installation"), Outcome: "success",
		Data: opt.Some[any](map[string]any{"sha256": sum, "bytes": size, "logs": logs}),
	}
	if _, err := st.AppendAudit(ctx, record); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: the bundle was not audited: %v\n", err) //nolint:errcheck // the terminal
	}
}

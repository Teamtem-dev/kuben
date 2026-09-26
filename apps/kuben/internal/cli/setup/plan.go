package setup

// `kuben setup --plan` (plan.rs): what a run would do, from the same
// read-only checks the steps make, and nothing changed (plan §11.2: the
// plan comes before any mutation). Resources a run would create are
// Kuben's; what exists already keeps its recorded owner, or stays someone
// else's.

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/bundlelock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/install/journal"
)

// planLine is one line of the plan: what, and what a run would do about it.
type planLine struct {
	what   string
	action string
}

func (m *machine) showPlan(ctx context.Context, opts Opts) {
	if runtime.GOOS != "linux" {
		m.ui.Note("`kuben setup` sets up a Linux server with systemd; nothing to plan on this machine.")
		return
	}
	j, ok := journal.Peek(filepath.Join(StateDir, journal.File))
	if !ok {
		j = journal.Journal{}
	}
	m.ui.Heading("kuben setup would:")
	for _, l := range m.planLines(ctx, opts, &j) {
		m.ui.Done(l.what, l.action)
	}
	if !isRoot() {
		m.ui.Note("Run as root for the checks that need it (PostgreSQL roles, firewall).")
	}
	if len(j.Resources) > 0 {
		m.ui.Heading("Recorded owners (kuben uninstall --purge removes only Kuben's):")
		for _, r := range j.Resources {
			m.ui.Done(r.Kind.DebugName()+" "+r.Name, r.Owner.DebugName())
		}
	}
	m.ui.Note("Nothing was changed. Run `kuben setup` without --plan to apply it.")
}

func (m *machine) planLines(ctx context.Context, opts Opts, j *journal.Journal) []planLine {
	version := "v" + m.version
	b, err := bundlelock.Get()
	if err != nil {
		b = bundlelock.Bundle{}
	}
	binary := fmt.Sprintf("install %s to %s", version, Bin)
	if exists(Bin) {
		binary = fmt.Sprintf("replace %s with %s when it differs", Bin, version)
	}
	user := fmt.Sprintf("create the system user %s (home %s)", User, StateDir)
	if m.userIDs(ctx, User).IsSome() {
		user = "use the existing user " + User
	}
	plan := []planLine{
		{"Binary", binary},
		{"System user", user},
		m.planCluster(opts, b),
	}
	configured := readText(ConfigFile)
	plan = append(plan, m.planDatabase(ctx, configured))
	port := opts.Port.Or(configuredPort(configured).Or(DefaultPort))
	configuration := fmt.Sprintf("write %s (PostgreSQL on this server, the kubeconfig copy)", ConfigFile)
	if configured.IsSome() {
		configuration = "keep " + ConfigFile
	}
	plan = append(plan, planLine{"Configuration", configuration})
	portAction := fmt.Sprintf("%d is in use: the next free port, or ask", port)
	if m.serviceActive(ctx) || m.portFree(port) {
		portAction = fmt.Sprintf("%d", port)
	}
	plan = append(plan, planLine{"Port", portAction})
	service := "install and start kuben.service"
	if unit, ok := readText(UnitFile).Get(); ok {
		if writtenBySetup(unit) {
			service = "restart kuben.service only if its binary, unit or configuration changes"
		} else {
			service = fmt.Sprintf("stop: %s was not written by kuben setup", UnitFile)
		}
	}
	plan = append(plan, planLine{"Service", service})
	fw := "nothing (no host firewall is active)"
	if f, ok := m.activeFirewall(ctx).Get(); ok {
		o := opening{port: port}
		if m.isOpen(ctx, f, o) {
			fw = f.rule(o) + " already open"
		} else {
			fw = "open " + f.rule(o)
		}
	}
	plan = append(plan, planLine{"Firewall", fw}, planPlatform(opts, b), planConsole(opts))
	if _, unfinished := j.Unfinished(); unfinished {
		plan = append(plan, planLine{"Last run", "did not finish: every step is checked again"})
	}
	return plan
}

func planPlatform(opts Opts, b bundlelock.Bundle) planLine {
	if opts.Kubeconfig.IsSome() {
		return planLine{"Platform", "nothing: a cluster brought with --kubeconfig is left as it is (kuben doctor says what it lacks)"}
	}
	issuer := "no issuer: apps are served over plain HTTP until --acme-email is given"
	if opts.AcmeEmail.IsSome() {
		issuer = "a Let's Encrypt ClusterIssuer (HTTP-01 through Kuben's Gateway)"
	}
	domain := ""
	if d, ok := opts.Domain.Get(); ok {
		domain = " and base domain " + d
	}
	return planLine{"Platform", fmt.Sprintf("Traefik as the Gateway provider, Gateway API %s CRDs and cert-manager %s when missing "+
		"(never upgraded), %s; KubenConfig with gateway class %s%s",
		b.GatewayAPI.Version, b.CertManager.Version, issuer, GatewayClass, domain)}
}

func planConsole(opts Opts) planLine {
	host, ok := opts.consoleHost().Get()
	switch {
	case ok && opts.Kubeconfig.IsNone():
		return planLine{"Console", fmt.Sprintf("https://%s through Kuben's Gateway; the first admin over HTTPS or an SSH tunnel", host)}
	case opts.AllowHTTPSetup:
		return planLine{"Console", "plain HTTP on the port, the first admin included (--allow-http-setup)"}
	default:
		return planLine{"Console", "plain HTTP on the port; the first admin over an SSH tunnel (or --domain with --acme-email)"}
	}
}

func (m *machine) planCluster(opts Opts, b bundlelock.Bundle) planLine {
	var action string
	path, given := opts.Kubeconfig.Get()
	existing := m.existingKubeconfig()
	switch {
	case given:
		action = "use the cluster in " + path
	case exists(K3sKubeconfig) && exists(K3sMarker):
		action = "use k3s (installed by kuben setup)"
	case exists(K3sKubeconfig):
		action = "use the k3s already installed (never removed by uninstall)"
	case existing.IsSome():
		action = "use the cluster in " + existing.Or("")
	case opts.NoK3s:
		action = "stop: no cluster found and --no-k3s given"
	default:
		action = fmt.Sprintf("install k3s %s (verified installer, %s datastore, reserved CPU and memory)",
			b.K3s.Version, opts.Datastore.debugName())
	}
	return planLine{"Cluster", action}
}

func (m *machine) planDatabase(ctx context.Context, configured opt.Val[string]) planLine {
	var action string
	switch home := dataHomeOf(configured); {
	case home == dataExternal:
		action = "use the database in " + ConfigFile
	case home == dataSqlite:
		action = fmt.Sprintf("stop: %s keeps Kuben 1.x data in SQLite", ConfigFile)
	case !m.postgresInstalled(ctx):
		if pk, ok := packagesOf(readText("/etc/os-release").Or("")); ok {
			action = fmt.Sprintf("install PostgreSQL (%s), then create the role and database kuben", pk.describe())
		} else {
			action = "stop: install PostgreSQL 14 or newer first"
		}
	case !isRoot():
		action = "start PostgreSQL; create the role and database kuben if missing"
	default:
		present := func(sql string) bool {
			out, err := m.psqlAsPostgres(ctx, sql)
			return err == nil && strings.TrimSpace(out) == "1"
		}
		role := present("SELECT 1 FROM pg_roles WHERE rolname = 'kuben'")
		db := present("SELECT 1 FROM pg_database WHERE datname = 'kuben'")
		switch {
		case role && db:
			action = "use the role and database kuben (kept by uninstall unless setup made them)"
		case !role && !db:
			action = "create the role and database kuben"
		case role:
			action = "create the database kuben for the existing role"
		default:
			action = "create the role kuben for the existing database"
		}
	}
	return planLine{"PostgreSQL", action}
}

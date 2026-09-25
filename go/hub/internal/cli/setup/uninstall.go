package setup

// `kuben status` without an app, and `kuben uninstall`.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/setup/journal"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// Status is `kuben status` without an app: the service, the admin
// account, the cluster and the console address of this server.
func Status(ctx context.Context, stdout, stderr io.Writer, version string) error {
	return newMachine(stdout, stderr, version).status(ctx)
}

func (m *machine) status(ctx context.Context) error {
	if !exists(UnitFile) {
		m.ui.Done("kuben.service", "not installed on this machine (kuben setup installs it)")
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	port := cfg.BindPort()
	if m.serviceActive(ctx) {
		m.ui.Done("kuben.service", "active, v"+m.version)
	} else {
		m.ui.Fail("kuben.service", "not running (journalctl -u kuben)")
	}
	code, body, err := m.httpGet(ctx, port, "/api/v1/setup")
	switch {
	case err != nil:
		m.ui.Fail("API", fmt.Sprintf("no answer on port %d: %s", port, err))
	case code == 200 && strings.Contains(body, `"needed":true`):
		m.ui.Warn("Admin account", "not created yet: kuben setup-token prints the link")
	case code == 200:
		m.ui.Done("Admin account", "created")
	default:
		m.ui.Warn("API", fmt.Sprintf("HTTP %d on /api/v1/setup", code))
	}
	if kubeconfig, ok := cfg.Kube.Kubeconfig.Get(); ok {
		nodes, err := m.waitForNodes(ctx, kubeconfig, 5*time.Second)
		if err != nil {
			m.ui.Fail("Cluster", err.Error())
		} else {
			m.ui.Done("Cluster", nodes)
		}
	} else {
		m.ui.Warn("Cluster", "no kubeconfig configured")
	}
	m.ui.Done("Console", m.consoleURL(ctx, cfg))
	return nil
}

// Uninstall is `kuben uninstall`.
func Uninstall(ctx context.Context, opts UninstallOpts, stdout, stderr io.Writer, version string) error {
	return newMachine(stdout, stderr, version).uninstall(ctx, opts)
}

func (m *machine) uninstall(ctx context.Context, opts UninstallOpts) error {
	if !isRoot() {
		return errors.New("run it as root: sudo kuben uninstall")
	}
	book, err := journal.OpenBook(StateDir, opt.None[uint32](), m.clock, m.logger)
	if err != nil {
		return err
	}
	mine := ownedBy(book.Journal(), readText(ConfigFile))
	// The apps run on that cluster: it stays.
	mine.k3s = mine.k3s && !opts.KeepApps
	if !opts.Yes {
		if err := m.confirmUninstall(opts, mine); err != nil {
			return err
		}
	}
	st := m.ui.Step("Stopping kuben.service")
	defer st.Close()
	if err := m.removeBackupTimer(ctx, book); err != nil {
		return err
	}
	switch {
	case exists(UnitFile) && mine.unit:
		_, _ = m.run(ctx, "systemctl", "disable", "--now", "--quiet", "kuben") //nolint:errcheck // stopped already is fine
		if err := os.Remove(UnitFile); err != nil {
			return err //nolint:wrapcheck // names the path
		}
		if _, err := m.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := book.Release(journal.KindSystemdUnit, unitName); err != nil {
			return err
		}
		st.Done("removed")
	case exists(UnitFile):
		st.Warn("not written by kuben setup; left in place")
	default:
		st.Done("not installed")
	}
	if !opts.Purge {
		m.ui.Note(fmt.Sprintf("Kept: the PostgreSQL database kuben, %s, %s, %s. `kuben uninstall --purge` "+
			"removes what kuben setup created (PostgreSQL itself stays).", StateDir, ConfigFile, Bin))
		if mine.k3s {
			m.ui.Note("k3s stays as well; --purge removes it too.")
		}
		m.ui.Note("The apps keep running; nobody updates them until kuben.service runs again.")
		return nil
	}
	kubeconfig := filepath.Join(StateDir, "kubeconfig")
	if !mine.k3s && exists(kubeconfig) {
		m.purgeCluster(ctx, kubeconfig, book.Journal(), opts.KeepApps)
	}
	m.purge(ctx, mine)
	return nil
}

func (m *machine) confirmUninstall(opts UninstallOpts, o owned) error {
	what := "Stop and remove kuben.service? (data and configuration stay)"
	if opts.Purge {
		extra := ""
		switch {
		case o.k3s:
			extra = " and the k3s it installed"
		case opts.KeepApps:
			extra = " (the apps keep running)"
		}
		what = fmt.Sprintf("Remove Kuben, its data in %s, %s%s?", StateDir, ConfigDir, extra)
	}
	yes, asked := m.ui.Confirm(what)
	switch {
	case !asked:
		return errors.New("no terminal to confirm on; pass --yes")
	case !yes:
		return errAborted
	}
	return nil
}

func (m *machine) purgeCluster(ctx context.Context, kubeconfig string, j *journal.Journal, keepApps bool) {
	if kept := m.purgeObjects(ctx, kubeconfig, j, keepApps); len(kept) > 0 {
		m.ui.Note(fmt.Sprintf("Kept in the cluster, other workloads may use them: %s.", strings.Join(kept, ", ")))
	}
	if !keepApps {
		return
	}
	inventory, err := m.retainedInventory(ctx, kubeconfig)
	switch {
	case err != nil:
		m.ui.Warn("Listing what stays", err.Error())
	case len(inventory) == 0:
		m.ui.Note("No apps are left in the cluster.")
	default:
		m.ui.Note("Left for the apps:\n  " + strings.Join(inventory, "\n  "))
	}
}

// owned is what `kuben uninstall --purge` may remove: what the journal
// says setup created. A server set up before the journal existed keeps the
// old rule: everything but a database of the operator's own, and k3s only
// with its marker.
type owned struct {
	unit     bool
	state    bool
	config   bool
	database bool
	role     bool
	k3s      bool
	binary   bool
	user     bool
	firewall []string
}

// ownedBy reads j; config is the configuration file's content.
func ownedBy(j *journal.Journal, config opt.Val[string]) owned {
	local := dataHomeOf(config) == dataLocal
	if len(j.Resources) == 0 {
		return owned{
			unit: true, state: true, config: true,
			database: local, role: local,
			k3s:    exists(K3sMarker),
			binary: true,
		}
	}
	rules := []string{}
	for _, r := range j.Resources {
		if r.Kind == journal.KindFirewallRule && r.Owner == journal.OwnerKuben {
			rules = append(rules, r.Name)
		}
	}
	return owned{
		unit:   j.Owns(journal.KindSystemdUnit, unitName),
		state:  j.Owns(journal.KindDirectory, StateDir),
		config: j.Owns(journal.KindFile, ConfigFile),
		// Never an external database, whatever the journal says.
		database: local && j.Owns(journal.KindPostgresDatabase, User),
		role:     local && j.Owns(journal.KindPostgresRole, User),
		k3s:      j.Owns(journal.KindCluster, "k3s"),
		binary:   j.Owns(journal.KindFile, Bin),
		user:     j.Owns(journal.KindSystemUser, User),
		firewall: rules,
	}
}

func (m *machine) purge(ctx context.Context, o owned) {
	st := m.ui.Step("Deleting data and configuration")
	removed, kept := []string{}, []string{}
	for _, p := range []struct {
		path string
		ours bool
	}{{ConfigFile, o.config}, {StateDir, o.state}} {
		gone := false
		if p.ours {
			if isDir(p.path) {
				gone = os.RemoveAll(p.path) == nil
			} else {
				gone = os.Remove(p.path) == nil
			}
		}
		if gone {
			removed = append(removed, p.path)
		} else if exists(p.path) {
			kept = append(kept, p.path)
		}
	}
	// The configuration directory goes only when setup made it and nothing
	// else lives there.
	_ = os.Remove(ConfigDir) //nolint:errcheck // stays when not empty
	if len(kept) == 0 {
		st.Done(strings.Join(removed, ", "))
	} else {
		st.Done(fmt.Sprintf("%s; kept (not created by kuben setup): %s", strings.Join(removed, ", "), strings.Join(kept, ", ")))
	}
	if (o.database || o.role) && m.which("psql").IsSome() {
		m.dropPostgres(ctx, o)
	}
	for _, rule := range o.firewall {
		f, opening, ok := parseRule(rule)
		if !ok {
			continue
		}
		if err := m.change(ctx, f, opening, false); err != nil {
			m.ui.Warn("Closing the firewall port", fmt.Sprintf("%s: %s", rule, err))
		} else {
			m.ui.Done("Closing the firewall port", rule)
		}
	}
	if o.k3s && exists(K3sUninstall) {
		m.uninstallK3s(ctx)
	}
	if o.user && m.userIDs(ctx, User).IsSome() {
		st := m.ui.Step("Removing the kuben system user")
		if _, err := m.run(ctx, "userdel", User); err != nil {
			st.Warn(err.Error())
		} else {
			st.Done(User)
		}
	}
	if o.binary {
		st := m.ui.Step("Removing the binary")
		_ = os.Remove(Bin) //nolint:errcheck // gone already is fine
		st.Done(Bin)
	}
}

func (m *machine) dropPostgres(ctx context.Context, o owned) {
	st := m.ui.Step("Dropping what kuben setup created in PostgreSQL")
	statements := []string{}
	if o.database {
		statements = append(statements, "DROP DATABASE IF EXISTS kuben WITH (FORCE)")
	}
	if o.role {
		statements = append(statements, "DROP ROLE IF EXISTS kuben")
	}
	for _, sql := range statements {
		if _, err := m.psqlAsPostgres(ctx, sql); err != nil {
			st.Warn(err.Error())
			return
		}
	}
	st.Done("PostgreSQL itself stays installed")
}

func (m *machine) uninstallK3s(ctx context.Context) {
	st := m.ui.Step("Uninstalling k3s")
	st.Command(K3sUninstall)
	out, err := m.runner.output(ctx, nil, K3sUninstall)
	switch {
	case err != nil:
		st.Warn(err.Error())
	case out.ok:
		st.Done("")
	default:
		st.Warn("k3s-uninstall.sh failed")
		m.ui.Note(tail(out.stdout, out.stderr, 8))
	}
}

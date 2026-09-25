package setup

// The configuration file, kuben.service with its backup timer, the host
// firewall, and the closing announcement.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/internal/cli/ui"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/install/journal"
)

// loadConfig reads the configuration as every kuben command does, without
// `--config` (Config::load).
func loadConfig() (config.Config, error) { return config.Load() } //nolint:wrapcheck // the caller says what

// writeConfig is true when the file was written or changed now. An
// existing file is kept, except for its port when port differs from it,
// and the sections a configuration setup wrote gains later.
func (m *machine) writeConfig(ctx context.Context, opts Opts, kubeconfig string, existing opt.Val[string], port uint16,
	wants configWants, book *journal.Book,
) (bool, error) {
	st := m.ui.Step("Writing " + ConfigFile)
	defer st.Close()
	if text, ok := existing.Get(); ok {
		return m.updateConfig(st, text, port, wants, book)
	}
	host := "localhost"
	if !opts.BindLocal {
		host = m.advertise(ctx).Or("localhost")
	}
	bindHost := "0.0.0.0"
	if opts.BindLocal {
		bindHost = "127.0.0.1"
	}
	publicURL := fmt.Sprintf("http://%s:%d", host, port)
	if c, ok := wants.console.Get(); ok {
		publicURL = "https://" + c
	}
	content := configTemplate(bindHost, port, publicURL, kubeconfig)
	if hub, ok := wants.hub.Get(); ok {
		content += agentSection(hub)
	}
	content += securitySection(wants)
	if err := os.WriteFile(ConfigFile, []byte(content), 0o644); err != nil { //nolint:gosec // readable by the service user
		return false, err //nolint:wrapcheck // names the path
	}
	if err := setMode(ConfigFile, 0o644); err != nil {
		return false, err
	}
	if _, err := book.Claim(journal.KindFile, ConfigFile, true); err != nil {
		return false, err
	}
	st.Done(fmt.Sprintf("port %d", port))
	return true, book.Done(true, fmt.Sprintf("port %d", port))
}

func (m *machine) updateConfig(st *ui.Step, text string, port uint16, wants configWants, book *journal.Book) (bool, error) {
	if _, err := book.Claim(journal.KindFile, ConfigFile, false); err != nil {
		return false, err
	}
	updated := updatedConfig(text, port, wants, book.Journal().Owns(journal.KindFile, ConfigFile))
	changed := updated.text != text
	if changed {
		if err := os.WriteFile(ConfigFile, []byte(updated.text), 0o644); err != nil { //nolint:gosec // readable by the service user
			return false, err //nolint:wrapcheck // names the path
		}
		suffix := ""
		if updated.agent {
			suffix = ", the cluster agent added"
		}
		st.Done(fmt.Sprintf("port %d%s, everything else kept", port, suffix))
		if wants.console.IsSome() && !hasSection(text, "security") {
			m.ui.Note("Set server.public_url to the console's https:// address in the configuration.")
		}
	} else {
		st.Done("kept; edit it to change the public URL")
	}
	return changed, book.Done(changed, fmt.Sprintf("port %d", port))
}

// configUpdate is an existing configuration as setup leaves it.
type configUpdate struct {
	text string
	// agent: the `[agent]` section was added.
	agent bool
}

// updatedConfig is text with port, and — when setup wrote it (owned) — the
// sections it gained since: `[agent]` for the local agent, `[security]`,
// or `insecure_setup` under an existing `[security]`. A configuration
// someone else wrote is theirs.
func updatedConfig(text string, port uint16, wants configWants, owned bool) configUpdate {
	updated := text
	if old, ok := configPort(text); ok && old != port {
		updated = withPort(text, old, port)
	}
	agent := false
	if hub, ok := wants.hub.Get(); ok && owned && !hasSection(text, "agent") {
		updated += agentSection(hub)
		agent = true
	}
	switch {
	case owned && !hasSection(text, "security"):
		updated += securitySection(wants)
	case owned && wants.insecureSetup && !strings.Contains(text, "insecure_setup"):
		if pos := strings.Index(updated, "[security]"); pos >= 0 {
			insertAt := len(updated)
			if i := strings.IndexByte(updated[pos:], '\n'); i >= 0 {
				insertAt = pos + i + 1
			}
			updated = updated[:insertAt] + "insecure_setup = true\n" + updated[insertAt:]
		}
	}
	return configUpdate{text: updated, agent: agent}
}

// installBackupTimer installs and starts the daily backup timer. Unit
// files someone else wrote are left alone.
func (m *machine) installBackupTimer(ctx context.Context, book *journal.Book) error {
	changed := false
	for _, u := range []struct{ name, desired string }{
		{backupService, backupServiceTemplate()},
		{backupTimer, backupTimerTemplate()},
	} {
		path := filepath.Join(systemdDir, u.name)
		current, has := readText(path).Get()
		if has && !strings.HasPrefix(current, unitMarker) && !book.Journal().Owns(journal.KindSystemdUnit, u.name) {
			return fmt.Errorf("%s exists and was not written by kuben setup; move it aside", path)
		}
		if _, err := book.Claim(journal.KindSystemdUnit, u.name, true); err != nil {
			return err
		}
		if !has || current != u.desired {
			if err := os.WriteFile(path, []byte(u.desired), 0o644); err != nil { //nolint:gosec // a unit file
				return err //nolint:wrapcheck // names the path
			}
			changed = true
		}
	}
	if changed {
		if _, err := m.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, "systemctl", "enable", "--now", "--quiet", backupTimer)
	return err
}

// removeBackupTimer stops and removes the backup timer setup installed.
func (m *machine) removeBackupTimer(ctx context.Context, book *journal.Book) error {
	if !book.Journal().Owns(journal.KindSystemdUnit, backupTimer) {
		return nil
	}
	_, _ = m.run(ctx, "systemctl", "disable", "--now", "--quiet", backupTimer) //nolint:errcheck // gone already is fine
	for _, name := range []string{backupTimer, backupService} {
		path := filepath.Join(systemdDir, name)
		if exists(path) {
			if err := os.Remove(path); err != nil {
				return err //nolint:wrapcheck // names the path
			}
		}
		if err := book.Release(journal.KindSystemdUnit, name); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, "systemctl", "daemon-reload")
	return err
}

// startService installs the unit and (re)starts the service: only when its
// binary, unit or configuration changed, or it is not running. A unit file
// someone else wrote is never replaced.
func (m *machine) startService(ctx context.Context, port uint16, changed bool, book *journal.Book) error {
	st := m.ui.Step("Starting kuben.service")
	defer st.Close()
	current, has := readText(UnitFile).Get()
	if has && !writtenBySetup(current) && !book.Journal().Owns(journal.KindSystemdUnit, unitName) {
		st.Fail("not written by kuben setup")
		return fmt.Errorf("%s exists and was not written by kuben setup; move it aside, then run kuben setup again", UnitFile)
	}
	if _, err := book.Claim(journal.KindSystemdUnit, unitName, true); err != nil {
		return err
	}
	desired := unitTemplate()
	unitChanged := !has || current != desired
	if unitChanged {
		if err := os.WriteFile(UnitFile, []byte(desired), 0o644); err != nil { //nolint:gosec // a unit file
			return err //nolint:wrapcheck // names the path
		}
		if _, err := m.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, "systemctl", "enable", "--quiet", "kuben"); err != nil {
		return err
	}
	if err := m.installBackupTimer(ctx, book); err != nil {
		return err
	}
	if !changed && !unitChanged && m.serviceActive(ctx) {
		st.Done("running, unchanged")
		return book.Done(false, "running, unchanged")
	}
	if _, err := m.run(ctx, "systemctl", "restart", "kuben"); err != nil {
		return err
	}
	if err := m.waitForHTTP(ctx, port, "/livez", time.Minute); err != nil {
		st.Fail("not answering")
		m.ui.Note(m.serviceLog(ctx, 15))
		return fmt.Errorf("kuben.service did not come up: %w; the lines above are its last log (journalctl -u kuben has more)", err)
	}
	if m.waitForHTTP(ctx, port, "/readyz", 90*time.Second) == nil {
		st.Done("running")
	} else {
		st.Warn("running, not ready yet: still syncing with the cluster")
		m.ui.Note(m.serviceProblems(ctx, 8))
	}
	return book.Done(true, "restarted")
}

func (m *machine) activeFirewall(ctx context.Context) opt.Val[firewall] {
	if strings.HasPrefix(m.stdoutOf(ctx, "ufw", "status"), "Status: active") {
		return opt.Some(firewallUfw)
	}
	if strings.TrimSpace(m.stdoutOf(ctx, "firewall-cmd", "--state")) == "running" {
		return opt.Some(firewallFirewalld)
	}
	return opt.None[firewall]()
}

func (m *machine) isOpen(ctx context.Context, f firewall, o opening) bool {
	port := fmt.Sprintf("%d/tcp", o.port)
	switch f {
	case firewallUfw:
		out, err := m.runner.output(ctx, nil, "ufw", "status")
		if err != nil {
			return false
		}
		for _, l := range lines(string(out.stdout)) {
			fields := strings.Fields(l)
			source, hasSource := o.source.Get()
			if len(fields) > 0 && fields[0] == port && (!hasSource || strings.Contains(l, source)) {
				return true
			}
		}
		return false
	case firewallFirewalld:
		if o.source.IsNone() {
			return m.runner.status(ctx, "firewall-cmd", "--query-port="+port)
		}
		return m.runner.status(ctx, "firewall-cmd", "--query-rich-rule="+richRule(o))
	}
	return false
}

// change opens (or closes) o.
func (m *machine) change(ctx context.Context, f firewall, o opening, open bool) error {
	port := fmt.Sprintf("%d", o.port)
	switch f {
	case firewallUfw:
		source, hasSource := o.source.Get()
		if !hasSource {
			rule := port + "/tcp"
			if open {
				_, err := m.run(ctx, "ufw", "allow", rule)
				return err
			}
			_, err := m.run(ctx, "ufw", "delete", "allow", rule)
			return err
		}
		args := []string{"ufw"}
		if !open {
			args = append(args, "delete")
		}
		args = append(args, "allow", "from", source, "to", "any", "port", port, "proto", "tcp")
		_, err := m.run(ctx, args...)
		return err
	case firewallFirewalld:
		var arg string
		switch hasSource := o.source.IsSome(); {
		case !hasSource && open:
			arg = "--add-port=" + port + "/tcp"
		case !hasSource:
			arg = "--remove-port=" + port + "/tcp"
		case open:
			arg = "--add-rich-rule=" + richRule(o)
		default:
			arg = "--remove-rich-rule=" + richRule(o)
		}
		if _, err := m.run(ctx, "firewall-cmd", "--permanent", arg); err != nil {
			return err
		}
		_, err := m.run(ctx, "firewall-cmd", "--reload")
		return err
	}
	return nil
}

// openFirewall opens the console's port, and with the local agent its port
// for the pod network only. Each rule setup adds is recorded; one that was
// open is not.
func (m *machine) openFirewall(ctx context.Context, port uint16, agent bool, book *journal.Book) error {
	label := fmt.Sprintf("Opening port %d in the firewall", port)
	f, active := m.activeFirewall(ctx).Get()
	if !active {
		m.ui.Done(label, "no host firewall is active")
		return book.Done(false, "no host firewall")
	}
	openings := []opening{{port: port}}
	if agent {
		openings = append(openings, opening{port: AgentPort, source: opt.Some(PodNetwork)})
	}
	st := m.ui.Step(label)
	defer st.Close()
	changed, notes := false, []string{}
	for _, o := range openings {
		rule := f.rule(o)
		if m.isOpen(ctx, f, o) {
			if _, err := book.Claim(journal.KindFirewallRule, rule, false); err != nil {
				return err
			}
			notes = append(notes, rule+" already open")
			continue
		}
		// A firewall problem never stops the install; it is reported.
		if err := m.change(ctx, f, o, true); err != nil {
			notes = append(notes, fmt.Sprintf("%s failed: %s", rule, err))
			continue
		}
		if _, err := book.Claim(journal.KindFirewallRule, rule, true); err != nil {
			return err
		}
		changed = true
		notes = append(notes, rule)
	}
	detail := strings.Join(notes, ", ")
	if strings.Contains(detail, "failed") {
		st.Warn(detail)
	} else {
		st.Done(detail)
	}
	return book.Done(changed, detail)
}

// consoleURL is the console's address for links (host::console_url).
func (m *machine) consoleURL(ctx context.Context, cfg config.Config) string {
	return cfg.ConsoleURLWithHost(m.advertise(ctx).Or("localhost"))
}

func (m *machine) announce(ctx context.Context, cfg config.Config, owner ids, freshConfig bool, host hostFacts,
	managed opt.Val[Wanted],
) error {
	port := cfg.BindPort()
	needed := false
	if m.waitForHTTP(ctx, port, "/api/v1/setup", 10*time.Second) == nil {
		if _, body, err := m.httpGet(ctx, port, "/api/v1/setup"); err == nil {
			needed = strings.Contains(body, `"needed":true`)
		}
	}
	m.ui.Blank()
	var url string
	if needed {
		// `serve` wrote the token as the service user; issue one if it did not.
		token := opt.None[string]()
		if api.SetupTokenRequired(cfg) {
			t, err := api.CurrentOrNewSetupToken(cfg, time.UnixMilli(m.clock.NowMs()))
			if err != nil {
				return err //nolint:wrapcheck // names the file
			}
			if err := chown(api.SetupTokenPath(cfg), owner); err != nil {
				return err
			}
			token = opt.Some(t)
		}
		link, notes := api.SetupGuide(cfg, token, m.advertise(ctx))
		url = link
		m.ui.Heading("Kuben is running. Finish the setup in your browser:")
		m.eprintln("\n    " + url + "\n")
		for _, n := range notes {
			m.ui.Note(n)
		}
		if token.IsSome() {
			m.ui.Note("The link is valid for 30 minutes; print a new one with `kuben setup-token`.")
		}
	} else {
		url = m.consoleURL(ctx, cfg)
		if freshConfig {
			m.ui.Heading("Kuben is running:")
		} else {
			m.ui.Heading("Kuben is up to date and running:")
		}
		m.eprintln("\n    " + url + "\n")
	}
	if !cfg.BindIsLoopback() {
		m.ui.Note("Behind a cloud firewall (Hetzner, AWS, GCP, …)? Allow the port there too.")
	}
	m.ui.Note(appsNote(managed))
	m.ui.Note(fmt.Sprintf("%s · %s / %s", Docs, host.os, host.arch))
	m.println(url)
	return nil
}

// appsNote says how apps get their public addresses.
func appsNote(managed opt.Val[Wanted]) string {
	w, ok := managed.Get()
	if !ok {
		return "Apps get public HTTPS addresses once a Gateway and a base domain are configured; see the docs."
	}
	domain, hasDomain := w.Domain.Get()
	switch {
	case hasDomain && w.AcmeEmail.IsSome():
		return fmt.Sprintf("Apps get HTTPS addresses under %[1]s; point a wildcard DNS record (*.%[1]s) at this server.", domain)
	case hasDomain:
		return fmt.Sprintf("Apps get plain-HTTP addresses under %s; add --acme-email you@example.com for HTTPS.", domain)
	default:
		return "Apps get public addresses once a base domain is set: kuben setup --domain apps.example.com --acme-email you@example.com"
	}
}

var errAborted = errors.New("aborted") //nolint:gochecknoglobals // sentinel

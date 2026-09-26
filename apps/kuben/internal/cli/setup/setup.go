// Package setup is `kuben setup`: this machine becomes a Kuben server.
// `kuben status` (without an app) and `kuben uninstall` share its pieces
// (crates/kuben/src/cli/setup: mod.rs, plan.rs, platform.rs, journal.rs).
//
// Every step checks before it acts, so the same command installs, upgrades,
// repairs and resumes an interrupted run: an existing k3s or kubeconfig is
// used instead of installing k3s, an existing config file is kept, and the
// service restarts only when its binary, unit or configuration changed; a
// repeated run changes nothing. Linux with systemd, as root; everything
// else gets a clear message and no changes. `kuben setup --plan` shows what
// a run would do without doing it.
//
// Every run is recorded in the install journal (package journal), with the owner
// of each resource: the binary at /usr/local/bin/kuben, the system user
// `kuben`, /etc/kuben/config.toml, /var/lib/kuben (kubeconfig copy, setup
// token, journal), the PostgreSQL role and database `kuben` (PostgreSQL from
// the distribution's packages when it was missing), kuben.service, a
// firewall rule, and k3s. `kuben uninstall` removes only what setup created;
// what was there before stays (I12).
//
// Commands, ports, local HTTP and the cluster go through the machine's
// runner, portFree, httpGet and connect, so the tests run without root,
// systemd, k3s or a network, as the Rust tests did.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/cli/ui"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/host"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/install/journal"
)

// Where setup puts things.
const (
	Bin      = "/usr/local/bin/kuben"
	User     = "kuben"
	StateDir = "/var/lib/kuben"
	// LocalDatabaseURL is the server's own PostgreSQL, reached over its Unix
	// socket with peer authentication as User, so no password is stored
	// (ADR-025).
	LocalDatabaseURL = "postgres:///kuben?host=/run/postgresql&user=kuben"
	ConfigDir        = "/etc/kuben"
	ConfigFile       = "/etc/kuben/config.toml"
	UnitFile         = "/etc/systemd/system/kuben.service"
	K3sKubeconfig    = "/etc/rancher/k3s/k3s.yaml"
	K3sUninstall     = "/usr/local/bin/k3s-uninstall.sh"
	// K3sMarker was written by setup before the install journal existed,
	// when it installed k3s itself; still honoured for such installs.
	K3sMarker = "/var/lib/kuben/.k3s-installed-by-kuben"
	Docs      = "https://kuben.teamtem.com/docs/getting-started/binary/"
	// DefaultPort is the port Kuben listens on when neither --port nor an
	// install says otherwise.
	DefaultPort uint16 = 3000
	// AgentPort is the port AgentLink listens on for the agent in this
	// server's k3s.
	AgentPort uint16 = 7443
	// PodNetwork is k3s's default pod network: agent pods reach the hub on
	// this server from there.
	PodNetwork = "10.42.0.0/16"

	unitName = "kuben.service"
	// The daily backup (M4.7): a oneshot service and its timer.
	backupService = "kuben-backup.service"
	backupTimer   = "kuben-backup.timer"
	systemdDir    = "/etc/systemd/system"
	// unitMarker is the first line of the unit files setup writes.
	unitMarker = "# Written by `kuben setup`"
)

// Opts are the options of `kuben setup`.
type Opts struct {
	// Port of the console and API; 3000, or the next free port, by default.
	Port opt.Val[uint16]
	// Kubeconfig is a cluster to manage instead of installing k3s.
	Kubeconfig opt.Val[string]
	// NoK3s: never install k3s; fail when no cluster is found.
	NoK3s bool
	// BindLocal: listen on 127.0.0.1 only.
	BindLocal bool
	// Yes goes ahead on warnings without asking.
	Yes bool
	// Plan shows what setup would do, and changes nothing.
	Plan bool
	// Domain is the base domain for app hostnames.
	Domain opt.Val[string]
	// AcmeEmail is the Let's Encrypt account: apps get HTTPS certificates.
	AcmeEmail opt.Val[string]
	// AcmeStaging uses Let's Encrypt's staging server.
	AcmeStaging bool
	// Datastore is how an installed k3s keeps its state.
	Datastore Datastore
	// AllowHTTPSetup lets the first admin be created over plain HTTP.
	AllowHTTPSetup bool
}

// consoleHost is the console's own HTTPS host, when setup gives it one: a
// domain for apps and an ACME account (the console needs a trusted
// certificate).
func (o Opts) consoleHost() opt.Val[string] {
	domain, hasDomain := o.Domain.Get()
	if hasDomain && o.AcmeEmail.IsSome() && !o.BindLocal {
		return opt.Some("kuben." + domain)
	}
	return opt.None[string]()
}

func (o Opts) wanted() Wanted {
	return Wanted{Domain: o.Domain, AcmeEmail: o.AcmeEmail, AcmeStaging: o.AcmeStaging}
}

// UninstallOpts are the options of `kuben uninstall`.
type UninstallOpts struct {
	// Purge also deletes the data, the configuration, the binary, and k3s
	// when setup installed it.
	Purge bool
	// KeepApps (with Purge) keeps the apps running without Kuben.
	KeepApps bool
	// Yes does not ask for confirmation.
	Yes bool
}

// machine is this server as setup sees it, and how setup talks to the user.
type machine struct {
	ui ui.UI
	// stdout gets the final link; stderr the lines ui does not draw.
	stdout  io.Writer
	stderr  io.Writer
	runner  runner
	clock   clock.Clock
	sleep   func(context.Context, time.Duration) error
	connect func(kubeconfig string) (cluster, error)
	// portFree reports whether nothing listens on the port.
	portFree func(port uint16) bool
	httpGet  func(ctx context.Context, port uint16, path string) (int, string, error)
	// advertise is this machine's address as other machines reach it.
	advertise func(context.Context) opt.Val[string]
	getenv    func(string) string
	lookupEnv func(string) (string, bool)
	version   string
	// logger receives what Rust sent to tracing, which setup never
	// initialised: nothing is shown.
	logger *slog.Logger
}

func newMachine(stdout, stderr io.Writer, version string) *machine {
	return &machine{
		ui:        consoleUI(stderr),
		stdout:    stdout,
		stderr:    stderr,
		runner:    execRunner{},
		clock:     clock.System{},
		sleep:     sleepCtx,
		connect:   connectKube,
		portFree:  portFreeOn,
		httpGet:   httpGet,
		advertise: host.AdvertiseIP,
		getenv:    os.Getenv,
		lookupEnv: os.LookupEnv,
		version:   version,
		logger:    slog.New(slog.DiscardHandler),
	}
}

// consoleUI draws on stderr: the terminal's own when it is the process's
// stderr, else plain lines.
func consoleUI(stderr io.Writer) ui.UI {
	if f, ok := stderr.(*os.File); ok && f == os.Stderr {
		return ui.New()
	}
	return ui.With(stderr, false)
}

// eprintln writes a line to stderr outside a step (Rust's eprintln!);
// best effort, as there.
func (m *machine) eprintln(text string) {
	_, _ = io.WriteString(m.stderr, text+"\n") //nolint:errcheck // stderr
}

// println writes a line to stdout (the final link).
func (m *machine) println(text string) {
	_, _ = io.WriteString(m.stdout, text+"\n") //nolint:errcheck // stdout
}

// Setup is `kuben setup`; version is this binary's.
func Setup(ctx context.Context, opts Opts, stdout, stderr io.Writer, version string) error {
	return newMachine(stdout, stderr, version).setup(ctx, opts)
}

func (m *machine) setup(ctx context.Context, opts Opts) error {
	if _, printed := m.lookupEnv("KUBEN_BANNER_PRINTED"); !printed {
		m.ui.Banner(m.version)
	}
	if opts.Plan {
		m.showPlan(ctx, opts)
		return nil
	}
	m.reportDownload()
	host, err := m.preflight(opts)
	if err != nil {
		return err
	}
	// The journal lives in the state directory: whether that directory is
	// new is known only before the journal is first saved.
	freshState := !exists(StateDir)
	group := opt.None[uint32]()
	if owner, ok := m.userIDs(ctx, User).Get(); ok {
		group = opt.Some(owner.gid)
	}
	book, err := journal.OpenBook(StateDir, group, m.clock, m.logger)
	if err != nil {
		return err
	}
	if last, ok := book.Journal().Unfinished(); ok {
		stopped := "was interrupted"
		for i := len(last.Steps) - 1; i >= 0; i-- {
			if last.Steps[i].Result == journal.StepFailed {
				stopped = "stopped at " + last.Steps[i].ID
				break
			}
		}
		m.ui.Note("The last run " + stopped + "; this run checks every step again and carries on.")
	}
	if err := book.Begin(m.version); err != nil {
		return err
	}
	result := m.install(ctx, opts, host, freshState, book)
	if err := book.Finish(result); err != nil {
		return err
	}
	if result != nil {
		return result
	}
	if !book.Journal().LastRunChanged() {
		m.ui.Note("Nothing changed: this server was already set up this way.")
	}
	return nil
}

type baseInstall struct {
	owner         ids
	kubeconfig    string
	configured    opt.Val[string]
	binaryChanged bool
}

func (m *machine) installBase(ctx context.Context, opts Opts, freshState bool, book *journal.Book) (baseInstall, error) {
	if err := m.adoptEarlierInstall(book); err != nil {
		return baseInstall{}, err
	}
	book.Start("binary")
	binaryChanged, err := m.installBinary(book)
	if err != nil {
		return baseInstall{}, err
	}
	book.Start("user")
	owner, err := m.ensureUser(ctx, freshState, book)
	if err != nil {
		return baseInstall{}, err
	}
	book.SetGroup(owner.gid)
	book.Start("cluster")
	kubeconfig, err := m.ensureCluster(ctx, opts, owner, book)
	if err != nil {
		return baseInstall{}, err
	}
	configured := readText(ConfigFile)
	book.Start("database")
	configReset, err := m.ensureDatabase(ctx, opts, configured, book)
	if err != nil {
		return baseInstall{}, err
	}
	if configReset {
		configured = opt.None[string]()
	}
	return baseInstall{
		owner:         owner,
		kubeconfig:    kubeconfig,
		configured:    configured,
		binaryChanged: binaryChanged,
	}, nil
}

func (m *machine) installPostService(ctx context.Context, kubeconfig string, wanted Wanted, managed bool, console, hub opt.Val[string], port uint16, book *journal.Book) error {
	if managed {
		book.Start("kubenconfig")
		if err := m.ensureKubenConfig(ctx, kubeconfig, wanted, book); err != nil {
			return err
		}
	}
	consoleHost, hasConsole := console.Get()
	hubAddr, hasHub := hub.Get()
	if hasConsole && hasHub {
		book.Start("console")
		if err := m.ensureConsole(ctx, kubeconfig, consoleHost, hubAddr, port, book); err != nil {
			return err
		}
	}
	book.Start("firewall")
	return m.openFirewall(ctx, port, hub.IsSome(), book)
}

func (m *machine) install(ctx context.Context, opts Opts, host hostFacts, freshState bool, book *journal.Book) error {
	base, err := m.installBase(ctx, opts, freshState, book)
	if err != nil {
		return err
	}
	book.Start("config")
	port, err := m.choosePort(ctx, opts.Port, configuredPort(base.configured), opts.Yes)
	if err != nil {
		return err
	}
	// The managed path: this server's k3s gets what exposes apps (ADR-031)
	// and its own agent (M2.8). A cluster brought with --kubeconfig is left
	// as it is.
	managed := opts.Kubeconfig.IsNone() && exists(K3sKubeconfig)
	hub := opt.None[string]()
	if managed {
		hub = m.advertise(ctx)
	}
	console := opt.None[string]()
	if hub.IsSome() {
		console = opts.consoleHost()
	}
	wants := configWants{hub: hub, console: console, insecureSetup: m.allowHTTPSetup(opts)}
	configChanged, err := m.writeConfig(ctx, opts, base.kubeconfig, base.configured, port, wants, book)
	if err != nil {
		return err
	}
	wanted := opts.wanted()
	if managed {
		book.Start("platform")
		if err := m.ensurePlatform(ctx, base.kubeconfig, wanted, hub.IsSome(), book); err != nil {
			return err
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("reading /etc/kuben/config.toml: %w", err)
	}
	port = cfg.BindPort()
	book.Start("service")
	if err := m.startService(ctx, port, base.binaryChanged || configChanged, book); err != nil {
		return err
	}
	if err := m.installPostService(ctx, base.kubeconfig, wanted, managed, console, hub, port, book); err != nil {
		return err
	}
	m.warnWebPorts(ctx)
	managedWanted := opt.None[Wanted]()
	if managed {
		managedWanted = opt.Some(wanted)
	}
	return m.announce(ctx, cfg, base.owner, base.configured.IsNone(), host, managedWanted)
}

// allowHTTPSetup is `--allow-http-setup`, or, without a domain and on every
// address, yes with --yes, else the operator's answer (Enter says yes;
// without a terminal to ask, no).
func (m *machine) allowHTTPSetup(opts Opts) bool {
	if opts.AllowHTTPSetup {
		return true
	}
	if opts.Domain.IsSome() || opts.BindLocal {
		return false
	}
	if opts.Yes {
		return true
	}
	yes, asked := m.ui.ConfirmDefaultYes("Allow web setup directly over HTTP (http://<ip>:<port> without SSH tunnel)?")
	return asked && yes
}

func configuredPort(configured opt.Val[string]) opt.Val[uint16] {
	text, ok := configured.Get()
	if !ok {
		return opt.None[uint16]()
	}
	if port, ok := configPort(text); ok {
		return opt.Some(port)
	}
	return opt.None[uint16]()
}

// adoptEarlierInstall: a server set up before the journal existed has a
// unit file (or k3s marker) that shows setup made it, and it made the rest
// too — the binary, the user, the directories, the configuration and this
// server's own database. Recorded as Kuben's once, so `uninstall --purge`
// still removes them.
func (m *machine) adoptEarlierInstall(book *journal.Book) error {
	unit, hasUnit := readText(UnitFile).Get()
	earlier := len(book.Journal().Resources) == 0 && ((hasUnit && writtenBySetup(unit)) || exists(K3sMarker))
	if !earlier {
		return nil
	}
	type resource struct {
		kind journal.Kind
		name string
	}
	ours := []resource{
		{journal.KindFile, Bin},
		{journal.KindSystemUser, User},
		{journal.KindDirectory, StateDir},
		{journal.KindDirectory, ConfigDir},
		{journal.KindFile, ConfigFile},
		{journal.KindSystemdUnit, unitName},
	}
	if dataHomeOf(readText(ConfigFile)) == dataLocal {
		ours = append(ours, resource{journal.KindPostgresRole, User}, resource{journal.KindPostgresDatabase, User})
	}
	for _, r := range ours {
		if _, err := book.Claim(r.kind, r.name, true); err != nil {
			return err
		}
	}
	return nil
}

// choosePort is the port to use. A running install keeps its own. An
// explicit --port, or the port of a service that already ran, must be free:
// another process holding it is named. Otherwise 3000; when something else
// has it (the Dokploy console, a Node app…) the operator is asked, with the
// next free port as the answer Enter gives, and without a terminal (or with
// --yes) that port is taken, so a first install never stops on it.
func (m *machine) choosePort(ctx context.Context, requested, configured opt.Val[uint16], yes bool) (uint16, error) {
	if m.serviceActive(ctx) && (requested.IsNone() || requested == configured) {
		if port, ok := configured.Get(); ok {
			return port, nil // the port is ours
		}
	}
	// A service that was started once keeps its port; a first run that
	// stopped halfway (config written, never started) may still move.
	settled := configured.IsSome() && exists(UnitFile)
	fallback := opt.None[uint16]()
	if settled {
		fallback = configured
	}
	wanted := requested.Or(fallback.Or(DefaultPort))
	st := m.ui.Step(fmt.Sprintf("Checking port %d", wanted))
	defer st.Close()
	if m.portFree(wanted) {
		st.Done("free")
		return wanted, nil
	}
	owner := m.portOwnerOr(ctx, wanted)
	suggestion := opt.None[uint16]()
	if requested.IsNone() && !settled {
		suggestion = m.nextFreePort(wanted)
	}
	next, ok := suggestion.Get()
	if !ok {
		st.Fail("used by " + owner)
		return 0, fmt.Errorf("port %d is already in use by %s; pick another with `kuben setup --port <port>`", wanted, owner)
	}
	st.Warn("used by " + owner)
	chosen := func(port uint16, why string) (uint16, error) {
		m.ui.Done("Port", fmt.Sprintf("%d%s", port, why))
		return port, nil
	}
	if yes {
		return chosen(next, ", the next free one")
	}
	for range 3 {
		answer, asked := m.ui.Ask("Which port should Kuben use?", strconv.Itoa(int(next)))
		if !asked {
			return chosen(next, ", the next free one")
		}
		port, valid := parsePort(answer)
		switch {
		case valid && m.portFree(port):
			return chosen(port, "")
		case valid:
			m.ui.Warn(fmt.Sprintf("Port %d", port), "used by "+m.portOwnerOr(ctx, port))
		default:
			m.ui.Warn("Port", fmt.Sprintf("%q is not a port number (1–65535)", answer))
		}
	}
	return 0, errors.New("no usable port chosen; run `kuben setup --port <port>`")
}

// nextFreePort is the first free port of the 99 after after.
func (m *machine) nextFreePort(after uint16) opt.Val[uint16] {
	start := min(uint32(after)+1, 65535)
	end := min(uint32(after)+100, 65535)
	for port := start; port < end; port++ {
		p := uint16(port) //nolint:gosec // port is bounded by min(..., 65535)
		if m.portFree(p) {
			return opt.Some(p)
		}
	}
	return opt.None[uint16]()
}

// reportDownload reports the installer's download here so that the banner
// comes first: install.sh passes
// `KUBEN_INSTALLED="<version> <target> <path> <sha256>"` when it hands over
// to `kuben setup`.
func (m *machine) reportDownload() {
	if _, printed := m.lookupEnv("KUBEN_BANNER_PRINTED"); printed {
		return
	}
	info, ok := m.lookupEnv("KUBEN_INSTALLED")
	if !ok {
		return
	}
	if fields := strings.Fields(info); len(fields) == 4 {
		m.ui.Done(fmt.Sprintf("Installed kuben %s (%s) to %s", fields[0], fields[1], fields[2]), "sha256 "+fields[3])
	}
}

// warnWebPorts: apps get public addresses through the cluster's ingress on
// 80 and 443 (Traefik on k3s). Another web server on the host (Dokploy's
// Traefik, nginx, Caddy) keeps them, so say so before anyone wonders why an
// app has no public address. The console does not depend on them.
func (m *machine) warnWebPorts(ctx context.Context) {
	taken := []string{}
	for _, port := range []uint16{80, 443} {
		if !m.portFree(port) {
			taken = append(taken, fmt.Sprintf("%d is used by %s", port, m.portOwnerOr(ctx, port)))
		}
	}
	if len(taken) > 0 {
		m.ui.Warn("Ports 80 and 443", strings.Join(taken, "; "))
		m.ui.Note("Apps get their public addresses through the cluster's Traefik on these ports; until that\n" +
			"server moves, the console works but app routes are not reachable from outside.")
	}
}

// hostFacts is what preflight learnt about this machine.
type hostFacts struct {
	os    string
	arch  string
	memGB opt.Val[float64]
}

func (m *machine) preflight(opts Opts) (hostFacts, error) {
	st := m.ui.Step("Preflight checks")
	defer st.Close()
	if runtime.GOOS != "linux" {
		st.Fail("Linux only")
		return hostFacts{}, fmt.Errorf("`kuben setup` sets up a Linux server; on this machine run `kuben serve` yourself (see %s)", Docs)
	}
	if !isRoot() {
		st.Fail("not root")
		return hostFacts{}, errors.New("run it as root: sudo kuben setup")
	}
	if !isDir("/run/systemd/system") {
		st.Fail("no systemd")
		return hostFacts{}, fmt.Errorf("this system does not run systemd; run `kuben serve` under your own supervisor (see %s)", Docs)
	}
	host := hostFacts{os: osName().Or("Linux"), arch: archName(runtime.GOARCH), memGB: memTotalGB()}
	detail := fmt.Sprintf("%s, %s", host.os, host.arch)
	gb, knownMem := host.memGB.Get()
	if knownMem {
		detail = fmt.Sprintf("%s, %s, %.0f GB RAM", host.os, host.arch, gb)
	}
	if m.inContainer() && opts.Kubeconfig.IsNone() {
		st.Fail(detail)
		return hostFacts{}, errors.New("this looks like a container; k3s cannot run here. Pass --kubeconfig for a cluster that exists, or run kuben setup on a VM or bare metal")
	}
	if knownMem && gb < 1.9 {
		st.Warn(detail)
		m.ui.Note("Less than 2 GB of RAM: k3s and Kuben will run, but slowly. 2 GB or more is recommended.")
		// Without a terminal (or with --yes) a warning does not stop the install.
		if !opts.Yes {
			if yes, asked := m.ui.Confirm("Continue anyway?"); asked && !yes {
				return hostFacts{}, errAborted
			}
		}
	} else {
		st.Done(detail)
	}
	return host, nil
}

// archName is the architecture as Rust named it (std::env::consts::ARCH).
func archName(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x86"
	default:
		return goarch
	}
}

// installBinary is true when the binary at Bin changed.
func (m *machine) installBinary(book *journal.Book) (bool, error) {
	me, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locating the running binary: %w", err)
	}
	existed := exists(Bin)
	version := "v" + m.version
	// The installer put it there (install.sh records that download itself),
	// or it is this very binary: nothing to copy.
	_, installedNow := m.lookupEnv("KUBEN_INSTALLED")
	if sameFile(me, Bin) {
		if _, err := book.Claim(journal.KindFile, Bin, installedNow); err != nil {
			return false, err
		}
		return installedNow, book.Done(installedNow, version+" at "+Bin)
	}
	if existed && sameContent(me, Bin) {
		if _, err := book.Claim(journal.KindFile, Bin, false); err != nil {
			return false, err
		}
		return false, book.Done(false, version+" at "+Bin+", up to date")
	}
	st := m.ui.Step("Installing the kuben binary")
	defer st.Close()
	if err := os.MkdirAll(filepath.Dir(Bin), 0o755); err != nil { //nolint:gosec // system bin directory
		return false, err //nolint:wrapcheck // names the path
	}
	// A running service keeps its old inode open; replace, do not overwrite.
	staged := withExtension(Bin, "new")
	if err := copyFile(me, staged); err != nil {
		return false, fmt.Errorf("copying %s to %s: %w", me, staged, err)
	}
	if err := setMode(staged, 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(staged, Bin); err != nil {
		return false, err //nolint:wrapcheck // names the paths
	}
	if _, err := book.Claim(journal.KindFile, Bin, !existed); err != nil {
		return false, err
	}
	st.Done(version + " → " + Bin)
	return true, book.Done(true, version+" → "+Bin)
}

// sameContent compares two files as Rust did: both unreadable counts as
// equal.
func sameContent(a, b string) bool {
	da, errA := os.ReadFile(a) //nolint:gosec // our own binary
	db, errB := os.ReadFile(b) //nolint:gosec // the installed binary
	if (errA == nil) != (errB == nil) {
		return false
	}
	return bytes.Equal(da, db)
}

func copyFile(from, to string) error {
	src, err := os.Open(from) //nolint:gosec // our own binary
	if err != nil {
		return err //nolint:wrapcheck // the caller names both
	}
	defer src.Close()                                                      //nolint:errcheck // read only
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // an executable, by design
	if err != nil {
		return err //nolint:wrapcheck // the caller names both
	}
	if _, err := io.Copy(dst, src); err != nil {
		return errors.Join(err, dst.Close())
	}
	return dst.Close() //nolint:wrapcheck // the caller names both
}

func (m *machine) ensureUser(ctx context.Context, freshState bool, book *journal.Book) (ids, error) {
	st := m.ui.Step("Creating the kuben system user")
	defer st.Close()
	existed := m.userIDs(ctx, User)
	owner, found := existed.Get()
	if found {
		st.Done("exists")
	} else {
		if _, err := m.run(ctx, "useradd", "--system", "--home-dir", StateDir, "--shell", "/usr/sbin/nologin",
			"--user-group", User); err != nil {
			return ids{}, err
		}
		created, ok := m.userIDs(ctx, User).Get()
		if !ok {
			return ids{}, fmt.Errorf("user %s not found after useradd", User)
		}
		owner = created
		st.Done(fmt.Sprintf("%s (uid %d)", User, owner.uid))
	}
	if _, err := book.Claim(journal.KindSystemUser, User, !found); err != nil {
		return ids{}, err
	}
	changed := !found
	for _, dir := range []string{StateDir, ConfigDir} {
		created := !isDir(dir)
		if dir == StateDir {
			created = freshState
		}
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // system directory
			return ids{}, err //nolint:wrapcheck // names the path
		}
		if _, err := book.Claim(journal.KindDirectory, dir, created); err != nil {
			return ids{}, err
		}
		changed = changed || created
	}
	if err := setMode(StateDir, 0o750); err != nil {
		return ids{}, err
	}
	if err := chown(StateDir, owner); err != nil {
		return ids{}, err
	}
	return owner, book.Done(changed, fmt.Sprintf("%s (uid %d)", User, owner.uid))
}

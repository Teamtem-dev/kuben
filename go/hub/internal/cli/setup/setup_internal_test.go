package setup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/ui"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/install/journal"
)

// failingRunner is a machine where no program can be started: no root,
// systemd, PostgreSQL or firewall.
type failingRunner struct{}

func (failingRunner) output(context.Context, []string, ...string) (commandOutput, error) {
	return commandOutput{}, errors.New("not in tests")
}

func (failingRunner) status(context.Context, ...string) bool { return false }

// testMachine talks to buffers and touches neither processes, ports nor
// the network.
func testMachine(t *testing.T) (*machine, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return &machine{
		ui:       ui.With(&stderr, false),
		stdout:   &stdout,
		stderr:   &stderr,
		runner:   failingRunner{},
		clock:    clock.Fixed(1_700_000_000_000),
		sleep:    func(context.Context, time.Duration) error { return errors.New("no waiting in tests") },
		connect:  func(string) (cluster, error) { return nil, errors.New("no cluster in tests") },
		portFree: func(uint16) bool { return true },
		httpGet: func(context.Context, uint16, string) (int, string, error) {
			return 0, "", errors.New("no server in tests")
		},
		advertise: func(context.Context) opt.Val[string] { return opt.None[string]() },
		getenv:    func(string) string { return "" },
		lookupEnv: func(string) (string, bool) { return "", false },
		version:   "2.0.0",
		logger:    slog.New(slog.DiscardHandler),
	}, &stdout, &stderr
}

// loadTOML reads text as a Kuben configuration over the defaults, with no
// environment.
func loadTOML(t *testing.T, text string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Source{Files: []string{path}, Environ: func() []string { return nil }}.Load()
	if err != nil {
		t.Fatalf("not a valid Kuben config: %v\n%s", err, text)
	}
	return cfg
}

func TestParsesTheHostFacts(t *testing.T) {
	if got := parseOSRelease("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n"); got != opt.Some("Ubuntu 24.04.4 LTS") {
		t.Errorf("os %v", got)
	}
	gb, ok := parseMemTotalGB("MemTotal:        4015036 kB\nMemFree: 1 kB\n").Get()
	if !ok || math.Abs(gb-3.83) >= 0.01 {
		t.Errorf("mem %v %v", gb, ok)
	}
	if got := parsePasswdIDs("kuben:x:998:997::/var/lib/kuben:/usr/sbin/nologin\n"); got != opt.Some(ids{uid: 998, gid: 997}) {
		t.Errorf("ids %v", got)
	}
	if got := parsePasswdIDs(""); got.IsSome() {
		t.Errorf("empty passwd %v", got)
	}
}

func TestConfigAndUnitTemplatesAreValidTOMLAndINI(t *testing.T) {
	cfg := loadTOML(t, configTemplate("0.0.0.0", 3000, "http://203.0.113.7:3000", "/var/lib/kuben/kubeconfig"))
	if cfg.Server.Bind != "0.0.0.0:3000" {
		t.Errorf("bind %s", cfg.Server.Bind)
	}
	if got := cfg.Server.PublicURL; got != opt.Some("http://203.0.113.7:3000") {
		t.Errorf("public url %v", got)
	}
	if got := cfg.Kube.Kubeconfig; got != opt.Some("/var/lib/kuben/kubeconfig") {
		t.Errorf("kubeconfig %v", got)
	}
	if got := cfg.Database.URL.Expose(); got != LocalDatabaseURL {
		t.Errorf("database %s", got)
	}
	if got := cfg.StateDir(); got != StateDir {
		t.Errorf("state dir %s", got)
	}
	if cfg.CookieSecure() {
		t.Error("plain http until public_url is https")
	}
	unit := unitTemplate()
	for _, want := range []string{
		"ExecStart=/usr/local/bin/kuben serve",
		"User=kuben",
		"StateDirectory=kuben",
		"After=network-online.target k3s.service postgresql.service",
		"Wants=network-online.target postgresql.service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q", want)
		}
	}
	backup := backupServiceTemplate()
	if !strings.HasPrefix(backup, unitMarker) || !strings.Contains(backup, "ExecStart=/usr/local/bin/kuben backup --scheduled") ||
		!strings.Contains(backup, "Type=oneshot") || !strings.Contains(backup, "User=kuben") {
		t.Errorf("backup service:\n%s", backup)
	}
	timer := backupTimerTemplate()
	if !strings.HasPrefix(timer, unitMarker) || !strings.Contains(timer, "Persistent=true") ||
		!strings.Contains(timer, "WantedBy=timers.target") {
		t.Errorf("backup timer:\n%s", timer)
	}
}

func TestKnowsWhereTheDataLivesAndHowToInstallPostgreSQL(t *testing.T) {
	written := configTemplate("0.0.0.0", 3000, "http://localhost:3000", "/var/lib/kuben/kubeconfig")
	homes := []struct {
		config opt.Val[string]
		want   dataHome
	}{
		{opt.None[string](), dataLocal},
		{opt.Some(written), dataLocal},
		{opt.Some("[database]\nurl = \"sqlite:///var/lib/kuben/kuben.db\"\n"), dataSqlite},
		{opt.Some("[server]\npublic_url = \"http://x:3000\"\n[database]\nurl = \"postgres://kuben:x@db/kuben\"\n"), dataExternal},
	}
	for _, h := range homes {
		if got := dataHomeOf(h.config); got != h.want {
			t.Errorf("%v: %d, want %d", h.config, got, h.want)
		}
	}
	type found struct {
		p  packages
		ok bool
	}
	for release, want := range map[string]found{
		"ID=ubuntu\nID_LIKE=debian\n":                    {packagesApt, true},
		"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n": {packagesDnf, true},
		"ID=fedora\n": {packagesDnf, true},
		"ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n": {packagesZypper, true},
		"ID=arch\n": {0, false},
	} {
		p, ok := packagesOf(release)
		if (found{p, ok}) != want {
			t.Errorf("%q: %v %v", release, p, ok)
		}
	}
}

func TestParsesPortAnswers(t *testing.T) {
	tests := []struct {
		answer string
		want   opt.Val[uint16]
	}{
		{" 3001 ", opt.Some[uint16](3001)},
		{"0", opt.None[uint16]()},
		{"70000", opt.None[uint16]()},
		{"three", opt.None[uint16]()},
	}
	for _, tc := range tests {
		port, ok := parsePort(tc.answer)
		got := opt.None[uint16]()
		if ok {
			got = opt.Some(port)
		}
		if got != tc.want {
			t.Errorf("%q: %v", tc.answer, got)
		}
	}
}

func TestReadsAndChangesTheConfiguredPort(t *testing.T) {
	text := configTemplate("0.0.0.0", 3000, "http://203.0.113.7:3000", "/var/lib/kuben/kubeconfig")
	if port, ok := configPort(text); !ok || port != 3000 {
		t.Errorf("bind, not metrics_bind: %d %v", port, ok)
	}
	moved := withPort(text, 3000, 3001)
	if port, ok := configPort(moved); !ok || port != 3001 {
		t.Errorf("moved: %d %v", port, ok)
	}
	for _, want := range []string{
		"public_url = \"http://203.0.113.7:3001\"",
		"metrics_bind = \"127.0.0.1:9090\"", // other ports stay
		"# Written by `kuben setup`",        // comments stay
	} {
		if !strings.Contains(moved, want) {
			t.Errorf("lacks %q:\n%s", want, moved)
		}
	}
	if !strings.HasSuffix(moved, "\n") {
		t.Error("the final newline is lost")
	}
	if port, ok := configPort("[server]\nbind=\"127.0.0.1:8080\"\n"); !ok || port != 8080 {
		t.Errorf("compact bind: %d %v", port, ok)
	}
	if _, ok := configPort("[server]\n"); ok {
		t.Error("a port without bind")
	}
}

func TestParsesHTTPResponses(t *testing.T) {
	code, body, ok := parseHTTPResponse("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"needed\":true}")
	if !ok || code != 200 || body != "{\"needed\":true}" {
		t.Errorf("got %d %q %v", code, body, ok)
	}
	if _, _, ok := parseHTTPResponse("garbage"); ok {
		t.Error("garbage parsed")
	}
}

func TestTailKeepsTheLastNonEmptyLines(t *testing.T) {
	if got := tail([]byte("a\n\nb\nc\n"), []byte("d\n"), 2); got != "c\nd" {
		t.Errorf("got %q", got)
	}
}

func TestTheAgentSectionLeavesTheServerPortAlone(t *testing.T) {
	config := configTemplate("0.0.0.0", 3000, "http://203.0.113.7:3000", "/var/lib/kuben/kubeconfig")
	if hasSection(config, "agent") {
		t.Fatal("an agent section already")
	}
	config += agentSection("203.0.113.7")
	if !hasSection(config, "agent") {
		t.Fatal("no agent section")
	}
	if !strings.Contains(config, "advertise = \"203.0.113.7:7443\"") {
		t.Errorf("advertise:\n%s", config)
	}
	if port, ok := configPort(config); !ok || port != 3000 {
		t.Errorf("the agent's bind is not the server's: %d", port)
	}
	moved := withPort(config, 3000, 7443)
	if port, ok := configPort(moved); !ok || port != 7443 {
		t.Errorf("moved %d", port)
	}
	if !strings.Contains(moved, "bind = \"0.0.0.0:7443\"\nlocal = true") {
		t.Error("the agent section is touched")
	}
	if back := withPort(moved, 7443, 3000); back != config {
		t.Errorf("only [server] lines moved:\n%s", cmp.Diff(config, back))
	}
}

func TestTheSecuritySectionTrustsOnlyTheGatewayAndOpensHTTPOnlyWhenAsked(t *testing.T) {
	read := func(wants configWants) config.Config {
		public := "http://203.0.113.7:3000"
		if c, ok := wants.console.Get(); ok {
			public = "https://" + c
		}
		return loadTOML(t, configTemplate("0.0.0.0", 3000, public, "/var/lib/kuben/kubeconfig")+securitySection(wants))
	}
	from := func(ip string) opt.Val[netip.Addr] { return opt.Some(netip.MustParseAddr(ip)) }
	if got := securitySection(configWants{}); got != "" {
		t.Errorf("empty wants: %q", got)
	}
	plain := read(configWants{})
	if plain.Security.InsecureSetup || plain.Security.TrustsForwarded(from("10.42.0.9")) {
		t.Error("plain trusts or opens")
	}

	gateway := read(configWants{console: opt.Some("kuben.apps.example.com")})
	if !gateway.CookieSecure() {
		t.Error("an https console gets Secure cookies")
	}
	if !gateway.Security.TrustsForwarded(from("10.42.3.4")) || gateway.Security.TrustsForwarded(from("203.0.113.9")) {
		t.Error("the gateway trusts the wrong peers")
	}
	if gateway.Security.InsecureSetup {
		t.Error("the gateway opens plain HTTP")
	}

	open := read(configWants{insecureSetup: true})
	if !open.Security.InsecureSetup || open.Security.TrustsForwarded(from("10.42.3.4")) {
		t.Error("allow-http-setup wrong")
	}
}

func TestFirewallRulesNameTheirSourceAndReadBack(t *testing.T) {
	open := opening{port: 3000}
	pods := opening{port: AgentPort, source: opt.Some(PodNetwork)}
	if got := firewallUfw.rule(open); got != "ufw:3000/tcp" {
		t.Errorf("got %s", got)
	}
	if got := firewallFirewalld.rule(pods); got != "firewalld:10.42.0.0/16:7443/tcp" {
		t.Errorf("got %s", got)
	}
	for _, c := range []struct {
		f firewall
		o opening
	}{{firewallUfw, open}, {firewallFirewalld, pods}} {
		f, o, ok := parseRule(c.f.rule(c.o))
		if !ok || f != c.f || o != c.o {
			t.Errorf("%s read back as %v %v %v", c.f.rule(c.o), f, o, ok)
		}
	}
	if _, _, ok := parseRule("iptables:22/tcp"); ok {
		t.Error("iptables parsed")
	}
	if !strings.Contains(richRule(pods), "source address=10.42.0.0/16 port port=7443") {
		t.Errorf("rich rule %s", richRule(pods))
	}
}

func TestAPlanNamesEveryStepAndChangesNothing(t *testing.T) {
	m, _, _ := testMachine(t)
	opts := Opts{
		Port:       opt.Some[uint16](3999),
		Kubeconfig: opt.Some("/nonexistent/kubeconfig"),
		Yes:        true,
		Plan:       true,
		Datastore:  DatastoreSqlite,
	}
	plan := m.planLines(context.Background(), opts, &journal.Journal{})
	whats := []string{}
	for _, l := range plan {
		whats = append(whats, l.what)
	}
	want := []string{"Binary", "System user", "Cluster", "PostgreSQL", "Configuration", "Port", "Service", "Firewall", "Platform", "Console"}
	if diff := cmp.Diff(want, whats); diff != "" {
		t.Fatalf("steps (-want +got):\n%s", diff)
	}
	if plan[2].action != "use the cluster in /nonexistent/kubeconfig" {
		t.Errorf("cluster: %s", plan[2].action)
	}
	if !strings.HasPrefix(plan[5].action, "3999") {
		t.Errorf("port: %s", plan[5].action)
	}
}

// The port asked for, another process holding 3000, and a terminal-less
// first install taking the next free port.
func TestAFirstInstallMovesOffATakenPortWithoutATerminal(t *testing.T) {
	m, _, stderr := testMachine(t)
	m.portFree = func(port uint16) bool { return port != 3000 }
	port, err := m.choosePort(context.Background(), opt.None[uint16](), opt.None[uint16](), true)
	if err != nil || port != 3001 {
		t.Fatalf("port %d, %v", port, err)
	}
	want := "… Checking port 3000\n⚠ Checking port 3000. used by another process\n✔ Port. 3001, the next free one\n"
	if diff := cmp.Diff(want, stderr.String()); diff != "" {
		t.Errorf("output (-want +got):\n%s", diff)
	}
	if _, err := m.choosePort(context.Background(), opt.Some[uint16](3000), opt.None[uint16](), true); err == nil ||
		err.Error() != "port 3000 is already in use by another process; pick another with `kuben setup --port <port>`" {
		t.Errorf("an explicit taken port: %v", err)
	}
}

func TestTheAppsNoteFollowsTheDomainAndTheIssuer(t *testing.T) {
	for _, c := range []struct {
		managed opt.Val[Wanted]
		want    string
	}{
		{opt.None[Wanted](), "Apps get public HTTPS addresses once a Gateway and a base domain are configured; see the docs."},
		{opt.Some(Wanted{}), "Apps get public addresses once a base domain is set: kuben setup --domain apps.example.com --acme-email you@example.com"},
		{opt.Some(Wanted{Domain: opt.Some("apps.example.com")}), "Apps get plain-HTTP addresses under apps.example.com; add --acme-email you@example.com for HTTPS."},
		{opt.Some(Wanted{Domain: opt.Some("apps.example.com"), AcmeEmail: opt.Some("a@b.c")}), "Apps get HTTPS addresses under apps.example.com; point a wildcard DNS record (*.apps.example.com) at this server."},
	} {
		if got := appsNote(c.managed); got != c.want {
			t.Errorf("got %s", got)
		}
	}
}

func TestAnOwnedConfigurationGainsItsLaterSectionsOnce(t *testing.T) {
	text := configTemplate("0.0.0.0", 3000, "http://203.0.113.7:3000", "/var/lib/kuben/kubeconfig")
	wants := configWants{hub: opt.Some("203.0.113.7"), insecureSetup: true}
	theirs := updatedConfig(text, 3000, wants, false)
	if theirs.text != text || theirs.agent {
		t.Error("someone else's configuration changed")
	}
	ours := updatedConfig(text, 3001, wants, true)
	if !ours.agent || !hasSection(ours.text, "agent") || !strings.Contains(ours.text, "insecure_setup = true") {
		t.Errorf("owned:\n%s", ours.text)
	}
	if again := updatedConfig(ours.text, 3001, wants, true); again.text != ours.text {
		t.Errorf("a second run changed it:\n%s", cmp.Diff(ours.text, again.text))
	}
	withSecurity := text + "\n[security]\ntrust_forwarded_for = false\n"
	added := updatedConfig(withSecurity, 3000, configWants{insecureSetup: true}, true)
	if !strings.Contains(added.text, "[security]\ninsecure_setup = true\ntrust_forwarded_for = false\n") {
		t.Errorf("insecure_setup not under [security]:\n%s", added.text)
	}
}

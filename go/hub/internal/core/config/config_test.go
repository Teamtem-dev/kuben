package config_test

import (
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

func TestForwardedHeadersAreBelievedOnlyFromTrustedProxies(t *testing.T) {
	ip := netip.MustParseAddr
	cases := []struct {
		cidr string
		ip   string
		want bool
	}{
		{"10.42.0.0/16", "10.42.3.7", true},
		{"10.42.0.0/16", "10.43.0.1", false},
		{"203.0.113.7", "203.0.113.7", true},
		{"203.0.113.7", "203.0.113.8", false},
		{"0.0.0.0/0", "198.51.100.1", true},
		{"10.42.0.0/99", "10.42.0.0", true},
		{"10.42.0.0/99", "10.42.0.1", false},
		{"10.42.0.0/x", "10.42.0.1", false},
		{" 10.42.0.0 /16", "10.42.9.9", true},
		{"fd00::/8", "fd12::1", true},
		{"fd00::/8", "10.42.0.1", false}, // families never mix
		{"::ffff:10.42.0.0/112", "10.42.0.1", false},
		{"not a network", "10.42.0.1", false},
	}
	for _, c := range cases {
		if got := config.CIDRContains(c.cidr, ip(c.ip)); got != c.want {
			t.Errorf("CIDRContains(%q, %s) = %v", c.cidr, c.ip, got)
		}
	}

	sec := config.DefaultSecurityCfg()
	if sec.TrustsForwarded(opt.Some(ip("10.42.0.5"))) {
		t.Error("off by default")
	}
	sec.TrustForwardedFor = true
	if !sec.TrustsForwarded(opt.None[netip.Addr]()) {
		t.Error("no list: every peer (the chart's proxy)")
	}
	sec.TrustedProxies = []string{"10.42.0.0/16"}
	if !sec.TrustsForwarded(opt.Some(ip("10.42.0.5"))) {
		t.Error("the proxy is trusted")
	}
	if sec.TrustsForwarded(opt.Some(ip("203.0.113.9"))) {
		t.Error("a direct client is not a proxy")
	}
	if sec.TrustsForwarded(opt.None[netip.Addr]()) {
		t.Error("an unknown peer is not a proxy")
	}
}

func TestDefaultsAreSane(t *testing.T) {
	cfg := config.Default()
	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("got %q", cfg.Server.Bind)
	}
	if !cfg.HasRole(config.RoleAPI) || !cfg.HasRole(config.RoleController) {
		t.Error("`all` enables every role")
	}
	if cfg.Security.CookieSecure != (config.CookieAuto{}) {
		t.Errorf("got %#v", cfg.Security.CookieSecure)
	}
	if cfg.Database.URL.IsSet() {
		t.Error("PostgreSQL has no default URL (ADR-025)")
	}
	if cfg.Agent.Bind.IsSome() {
		t.Error("AgentLink is off unless configured")
	}
	if cfg.Build.Enabled {
		t.Error("builds are off unless configured")
	}
	if cfg.Git.GithubEnabled() {
		t.Error("the GitHub App is off unless configured")
	}
	only := cfg
	only.Server.Roles = []config.Role{config.RoleAPI}
	if !only.HasRole(config.RoleAPI) || only.HasRole(config.RoleController) {
		t.Error("a named role enables itself only")
	}
}

func TestSSOMappingsBecomeAPolicy(t *testing.T) {
	s := config.DefaultSsoCfg()
	if !s.RequireVerifiedEmail || s.Enabled {
		t.Fatalf("got %+v", s)
	}
	s.Groups["admins"] = "admin"
	s.AllowedDomains = []string{" @Example.com"}
	policy, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Groups["admins"] != perm.Admin || policy.DefaultRole.IsSome() || !policy.RequireVerifiedEmail {
		t.Errorf("got %+v", policy)
	}
	if diff := cmp.Diff([]string{"example.com"}, policy.AllowedDomains); diff != "" {
		t.Error(diff)
	}
	s.DefaultRole = opt.Some("viewer")
	if policy, err = s.Policy(); err != nil || policy.DefaultRole != opt.Some(perm.Viewer) {
		t.Errorf("got %+v, %v", policy, err)
	}
	s.DefaultRole = opt.Some("root")
	if _, err := s.Policy(); kerr.CodeOf(err) != kerr.Validation {
		t.Errorf("got %v", err)
	}
	s.DefaultRole = opt.None[string]()
	s.Groups["x"] = "root"
	if _, err := s.Policy(); err == nil || err.Error() != "validation failed: unknown role `root`" {
		t.Errorf("got %v", err)
	}
}

func TestTheCIAudienceDefaultsToThePublicURL(t *testing.T) {
	cfg := config.Default()
	if _, ok := cfg.GithubOIDCAudience(); ok {
		t.Error("off by default")
	}
	cfg.CI.GithubActions = true
	if _, ok := cfg.GithubOIDCAudience(); ok {
		t.Error("no audience known")
	}
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com/")
	if got, ok := cfg.GithubOIDCAudience(); !ok || got != "https://kuben.example.com" {
		t.Errorf("got %q, %v", got, ok)
	}
	cfg.CI.GithubOIDCAudience = opt.Some("kuben-ci")
	if got, ok := cfg.GithubOIDCAudience(); !ok || got != "kuben-ci" {
		t.Errorf("got %q, %v", got, ok)
	}
	cfg.CI.GithubOIDCAudience = opt.Some(" / ")
	if _, ok := cfg.GithubOIDCAudience(); ok {
		t.Error("a blank audience is none, and does not fall back")
	}
}

func TestUnpinnedBuildImagesAreReported(t *testing.T) {
	build := config.DefaultBuildCfg()
	if got := build.UnpinnedImages(); len(got) != 3 {
		t.Errorf("the tag-pinned defaults: got %v", got)
	}
	build.BuildkitImage = "moby/buildkit@sha256:" + strings.Repeat("a", 64)
	build.FetchImage = "alpine/git@sha256:" + strings.Repeat("b", 64)
	build.ScannerImage = "aquasec/trivy@sha256:" + strings.Repeat("c", 64)
	if got := build.UnpinnedImages(); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	build.RailpackFrontend = opt.Some("ghcr.io/railwayapp/railpack-frontend")
	if diff := cmp.Diff([]string{"ghcr.io/railwayapp/railpack-frontend"}, build.UnpinnedImages()); diff != "" {
		t.Error(diff)
	}
}

func TestTheGithubAppNeedsAllThreeSettings(t *testing.T) {
	git := config.DefaultGitCfg()
	git.GithubAppID = opt.Some[uint64](1)
	git.GithubPrivateKeyFile = opt.Some("/etc/kuben/github.pem")
	if git.GithubEnabled() {
		t.Error("no webhook secret")
	}
	git.GithubWebhookSecret = "s3cret"
	if !git.GithubEnabled() {
		t.Error("all three are set")
	}
	git.GithubPrivateKeyFile = opt.Some("")
	if git.GithubEnabled() {
		t.Error("an empty file name is none")
	}
}

func TestOrganizationQuotasAreQuantities(t *testing.T) {
	var quota config.QuotaCfg
	if limits, err := quota.OrgLimits(); err != nil || !limits.IsUnlimited() {
		t.Fatalf("got %+v, %v", limits, err)
	}
	quota.OrgCPU, quota.OrgMemory, quota.OrgPods = opt.Some("16"), opt.Some("32Gi"), opt.Some[uint64](100)
	limits, err := quota.OrgLimits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.CPUMillis != opt.Some[uint64](16_000) || limits.MemoryBytes != opt.Some[uint64](32<<30) || limits.Pods != opt.Some[uint64](100) {
		t.Errorf("got %+v", limits)
	}
	quota.OrgMemory = opt.Some("lots")
	_, err = quota.OrgLimits()
	if invalid, ok := kerrOf(err); !ok || invalid.Code != kerr.Validation ||
		invalid.Detail != "quota.org_memory `lots` is not a quantity" {
		t.Errorf("got %v", err)
	}
	quota.OrgCPU = opt.Some("many")
	_, err = quota.OrgLimits()
	if invalid, ok := kerrOf(err); !ok || invalid.Detail != "quota.org_cpu `many` is not a quantity" {
		t.Errorf("got %v", err)
	}
}

func TestTheKeyringLivesInTheStateDirUnlessNamed(t *testing.T) {
	cfg := config.Default()
	cfg.Server.StateDir = opt.Some("/var/lib/kuben")
	if got := cfg.SecretKeyringFile(); got != filepath.FromSlash("/var/lib/kuben/secrets.keyring") {
		t.Errorf("got %q", got)
	}
	if got := cfg.BackupDir(); got != filepath.FromSlash("/var/lib/kuben/backups") {
		t.Errorf("got %q", got)
	}
	cfg.Secrets.KeyringFile = opt.Some("/etc/kuben/keyring")
	cfg.Backup.Dir = opt.Some("/mnt/backups")
	if got := cfg.SecretKeyringFile(); got != "/etc/kuben/keyring" {
		t.Errorf("got %q", got)
	}
	if got := cfg.BackupDir(); got != "/mnt/backups" {
		t.Errorf("got %q", got)
	}
}

func getenv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestStateDirKeepsAnExistingDataVolume(t *testing.T) {
	env := getenv(map[string]string{"STATE_DIRECTORY": "/var/lib/kuben", "HOME": "/home/u"})
	if got := config.DefaultDataDir(true, env); got != "/data" {
		t.Errorf("got %q", got)
	}
}

func TestStateDirNeedsNoRootOnAFreshServer(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"systemd", map[string]string{"STATE_DIRECTORY": "/var/lib/kuben:/var/lib/other", "HOME": "/root"}, "/var/lib/kuben"},
		{"systemd, first entry empty", map[string]string{"STATE_DIRECTORY": ":/var/lib/kuben"}, "/var/lib/kuben"},
		{"xdg", map[string]string{"XDG_STATE_HOME": "/xdg", "HOME": "/home/u"}, "/xdg/kuben"},
		{"home", map[string]string{"STATE_DIRECTORY": "", "HOME": "/home/u"}, "/home/u/.local/state/kuben"},
		{"windows", map[string]string{"LOCALAPPDATA": "/local"}, "/local/kuben"},
		{"nothing", nil, "."},
	}
	for _, c := range cases {
		if got := config.DefaultDataDir(false, getenv(c.env)); got != filepath.FromSlash(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWarnsWhenTheSecureCookieMeetsPlainHTTP(t *testing.T) {
	cfg := config.Default()
	warns := func(inCluster bool) bool {
		_, ok := cfg.InsecureCookieWarning(inCluster)
		return ok
	}
	if warns(false) {
		t.Error("auto over http is not Secure")
	}
	cfg.Security.CookieSecure = config.CookieFixed(true)
	cfg.Server.Bind = "0.0.0.0:3000"
	warning, ok := cfg.InsecureCookieWarning(false)
	want := "the session cookie is Secure and browsers drop it over plain http, so signing in at " +
		"http://<this server>:3000 loops back to the login page. Serve the console over " +
		"HTTPS (and set KUBEN_SERVER__PUBLIC_URL), open it through an SSH tunnel to " +
		"localhost, or set KUBEN_SECURITY__COOKIE_SECURE=auto"
	if !ok || warning != want {
		t.Errorf("forced Secure on 0.0.0.0 over http: got %q", warning)
	}
	if warns(true) {
		t.Error("in a pod")
	}
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com")
	if warns(false) {
		t.Error("https public URL")
	}
	cfg.Server.PublicURL = opt.None[string]()
	cfg.Server.Bind = "127.0.0.1:8080"
	if warns(false) {
		t.Error("loopback only")
	}
	cfg.Server.Bind = "0.0.0.0:8080"
	cfg.Security.CookieSecure = config.CookieFixed(false)
	if warns(false) {
		t.Error("cookie not Secure")
	}
}

func TestConsoleURLAndBindHelpers(t *testing.T) {
	cfg := config.Default()
	if cfg.BindPort() != 8080 || cfg.BindIsLoopback() {
		t.Errorf("got %d, %v", cfg.BindPort(), cfg.BindIsLoopback())
	}
	binds := []struct {
		bind     string
		port     uint16
		loopback bool
	}{
		{"127.0.0.1:3000", 3000, true},
		{"[::1]:3000", 3000, true},
		{"localhost:3000", 3000, true},
		{"[::ffff:127.0.0.1]:3000", 3000, false},
		{"0.0.0.0:99999", 8080, false},
		{"nonsense", 8080, false},
	}
	for _, b := range binds {
		cfg.Server.Bind = b.bind
		if cfg.BindPort() != b.port || cfg.BindIsLoopback() != b.loopback {
			t.Errorf("%s: got %d, %v", b.bind, cfg.BindPort(), cfg.BindIsLoopback())
		}
	}
	cfg.Server.Bind = "127.0.0.1:3000"
	if got := cfg.ConsoleURLWithHost("203.0.113.7"); got != "http://203.0.113.7:3000" {
		t.Errorf("got %q", got)
	}
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com/")
	if got := cfg.ConsoleURLWithHost("ignored"); got != "https://kuben.example.com" {
		t.Errorf("got %q", got)
	}
	cfg.Server.StateDir = opt.Some("/var/lib/kuben")
	if got := cfg.StateDir(); got != "/var/lib/kuben" {
		t.Errorf("got %q", got)
	}
}

func TestSetupWizardOnlyWithoutAConfiguredPassword(t *testing.T) {
	cfg := config.Default()
	if !cfg.SetupWizard(false) || cfg.SetupWizard(true) {
		t.Error("the wizard runs outside a cluster only")
	}
	cfg.Bootstrap.AdminPassword = ""
	if !cfg.SetupWizard(false) {
		t.Error("empty counts as unset")
	}
	cfg.Bootstrap.AdminPassword = "configured"
	if cfg.SetupWizard(false) {
		t.Error("a configured password needs no wizard")
	}
}

func TestCookieSecureAutoFollowsThePublicURL(t *testing.T) {
	cfg := config.Default()
	if cfg.CookieSecure() {
		t.Error("no public URL: plain http install")
	}
	cfg.Server.PublicURL = opt.Some("http://203.0.113.7:3000")
	if cfg.CookieSecure() {
		t.Error("an http public URL")
	}
	cfg.Server.PublicURL = opt.Some("https://kuben.example.com")
	if !cfg.CookieSecure() {
		t.Error("an https public URL")
	}
	var zero config.Config
	zero.Server.PublicURL = cfg.Server.PublicURL
	if !zero.CookieSecure() {
		t.Error("a configuration without a value is auto")
	}
	cfg.Security.CookieSecure = config.CookieFixed(false)
	if cfg.CookieSecure() {
		t.Error("an explicit value wins")
	}
}

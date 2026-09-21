package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

var optionals = cmp.AllowUnexported(opt.Val[string]{}, opt.Val[uint64]{})

// wantDefaults are the defaults of the Rust `Default` impls, written out so
// that a changed default is a failed test.
func wantDefaults() config.Config {
	return config.Config{
		Server: config.ServerCfg{
			Bind: "0.0.0.0:8080", MetricsBind: "0.0.0.0:9090", ActivatorBind: "0.0.0.0:8081",
			Roles: []config.Role{"all"}, RequestTimeoutSecs: 30, MaxBodyBytes: 1048576,
		},
		Database: config.DatabaseCfg{URL: "", MaxConnections: 4},
		Runtime:  config.RuntimeCfg{MaxBlockingThreads: 16},
		Kube:     config.KubeCfg{},
		Security: config.SecurityCfg{
			SessionTTLHours: 12, SessionCacheTTLSecs: 5, Argon2MKib: 19456, Argon2T: 2, Argon2P: 1,
			LoginConcurrency: 2, CookieSecure: config.CookieAuto{}, LoginMaxFailures: 5,
			LoginMaxFailuresPerIP: 30, LoginMaxFailuresPerAccount: 100, LoginWindowSecs: 900,
			TrustedProxies: []string{}, PasswordMinLength: 12,
		},
		Telemetry: config.TelemetryCfg{LogFormat: "json", LogLevel: "info"},
		Bootstrap: config.BootstrapCfg{OrgSlug: "default", OrgName: "Default", AdminEmail: "admin@kuben.local"},
		Agent:     config.AgentCfg{HubName: "hub.kuben.internal", CertificateHours: 24, HeartbeatSecs: 10},
		Git:       config.GitCfg{GithubAPIURL: "https://api.github.com", GithubCloneURL: "https://github.com"},
		Build: config.BuildCfg{
			BuildkitImage: "moby/buildkit:v0.33.0-rootless", FetchImage: "alpine/git:2.49.1",
			CPURequest: "500m", CPULimit: "2", Memory: "2Gi", EphemeralStorage: "10Gi",
			DeadlineSecs: 1800, MaxConcurrent: 2, MaxConcurrentPerOrg: 1,
			ScannerImage: "aquasec/trivy:0.74.0", ScannerMemory: "1Gi", RescanHours: 24,
		},
		CI: config.CiCfg{GithubOIDCIssuer: "https://token.actions.githubusercontent.com"},
		SSO: config.SsoCfg{
			DisplayName: "Single sign-on", Scopes: []string{"openid", "email", "profile"},
			GroupClaim: "groups", Groups: map[string]string{}, AllowedDomains: []string{},
			RequireVerifiedEmail: true,
		},
		Backup:    config.BackupCfg{Keep: 7, MaxAgeHours: 26, BeforeUpgrade: true},
		Retention: config.RetentionCfg{OutboxDays: 7, WebhookDeliveryDays: 30, ResolvedIncidentDays: 180, UsageDays: 7},
		Domains: config.DomainsCfg{
			DohURL: "https://cloudflare-dns.com/dns-query", CloudflareAPIURL: "https://api.cloudflare.com/client/v4",
		},
	}
}

func TestDefaultsArePinned(t *testing.T) {
	if diff := cmp.Diff(wantDefaults(), config.Default(), optionals); diff != "" {
		t.Errorf("Default() (-want +got):\n%s", diff)
	}
	loaded, err := jail(t, "", "", nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantDefaults(), loaded, optionals); diff != "" {
		t.Errorf("loading nothing (-want +got):\n%s", diff)
	}
}

func TestCookieSecureParsesBoolsAndAuto(t *testing.T) {
	cases := []struct {
		value string
		want  config.CookieSecure
	}{
		{"true", config.CookieFixed(true)},
		{"false", config.CookieFixed(false)},
		{" false ", config.CookieFixed(false)},
		{"auto", config.CookieAuto{}},
		{`"auto"`, config.CookieAuto{}},
	}
	for _, c := range cases {
		cfg, err := jail(t, "", "", map[string]string{"KUBEN_SECURITY__COOKIE_SECURE": c.value}).Load()
		if err != nil || cfg.Security.CookieSecure != c.want {
			t.Errorf("%s: got %#v, %v", c.value, cfg.Security.CookieSecure, err)
		}
		toml := "[security]\ncookie_secure = " + strings.TrimSpace(c.value) + "\n"
		if c.value == "auto" {
			continue // a bare word is not TOML
		}
		cfg, err = jail(t, "", toml, nil).Load()
		if err != nil || cfg.Security.CookieSecure != c.want {
			t.Errorf("toml %s: got %#v, %v", c.value, cfg.Security.CookieSecure, err)
		}
	}
	for _, bad := range []string{"sometimes", `"true"`, "1", "[true]"} {
		if _, err := jail(t, "", "", map[string]string{"KUBEN_SECURITY__COOKIE_SECURE": bad}).Load(); err == nil {
			t.Errorf("%s must be refused", bad)
		} else if !strings.Contains(err.Error(), `security.cookie_secure must be true, false or "auto"`) {
			t.Errorf("%s: got %v", bad, err)
		}
	}
}

func TestEnvOverridesNestedKeys(t *testing.T) {
	cfg, err := jail(t, "", "", map[string]string{
		"KUBEN_SERVER__BIND":                "127.0.0.1:1234",
		"KUBEN_SECURITY__SESSION_TTL_HOURS": "1",
		"kuben_server__Metrics_Bind":        "127.0.0.1:9", // names match in any case
		"KUBEN_SERVER__":                    "ignored",
		"KUBEN___BIND":                      "ignored",
		"KUBEN_":                            "ignored",
		"KUBEN_NOBODY__READS":               "ignored",
		"KUBENX_SERVER__BIND":               "ignored",
		"OTHER_SERVER__BIND":                "ignored",
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := wantDefaults()
	want.Server.Bind, want.Server.MetricsBind, want.Security.SessionTTLHours = "127.0.0.1:1234", "127.0.0.1:9", 1
	if diff := cmp.Diff(want, cfg, optionals); diff != "" {
		t.Error(diff)
	}
}

func TestEnvValuesParseLikeFigment(t *testing.T) {
	env := map[string]string{
		"KUBEN_SERVER__ROLES":               "[api, controller]",
		"KUBEN_SERVER__PUBLIC_URL":          `"https://kuben.example.com"`,
		"KUBEN_SERVER__STATE_DIR":           "  /var/lib/kuben  ",
		"KUBEN_SERVER__MAX_BODY_BYTES":      "+2097152",
		"KUBEN_RUNTIME__WORKER_THREADS":     "8",
		"KUBEN_KUBE__REQUIRED":              "true",
		"KUBEN_KUBE__CONTEXT":               "",
		"KUBEN_QUOTA__ORG_CPU":              "16",
		"KUBEN_QUOTA__ORG_MEMORY":           "64Gi",
		"KUBEN_QUOTA__ORG_PODS":             "500",
		"KUBEN_BUILD__CPU_LIMIT":            "1.50",
		"KUBEN_BUILD__NODE_POOL":            "pool=build",
		"KUBEN_GIT__GITHUB_APP_ID":          "123456",
		"KUBEN_GIT__GITHUB_WEBHOOK_SECRET":  `"  spaced\tsecreté "`,
		"KUBEN_BOOTSTRAP__ADMIN_PASSWORD":   "0123456789012",
		"KUBEN_BOOTSTRAP__ORG_NAME":         "true",
		"KUBEN_BOOTSTRAP__ORG_SLUG":         "a,b",
		"KUBEN_TELEMETRY__LOG_LEVEL":        "debug,hyper=info",
		"KUBEN_TELEMETRY__OTLP_ENDPOINT":    "[broken",
		"KUBEN_SSO__SCOPES":                 `[openid, "a b", 'c', 12, true]`,
		"KUBEN_SSO__GROUPS":                 `{admins=admin, "team.devs" = developer}`,
		"KUBEN_SSO__GROUPS__OPS":            "viewer",
		"KUBEN_SSO__ALLOWED_DOMAINS":        "[]",
		"KUBEN_SECURITY__TRUSTED_PROXIES":   "[10.42.0.0/16,fd00::/8,]",
		"KUBEN_DATABASE__URL":               "postgres://kuben:pw@db/kuben",
		"KUBEN_DATABASE__MAX_CONNECTIONS":   " 20 ",
		"KUBEN_NOTIFY__ALLOW_HTTP":          "false",
		"KUBEN_DOMAINS__CNAME_TARGET":       "trueish",
		"KUBEN_BACKUP__DIR":                 `"unterminated`,
		"KUBEN_SECRETS__KEYRING_FILE":       `"bad \x escape"`,
		"KUBEN_AGENT__ADVERTISE":            "'x'",
		"KUBEN_CI__GITHUB_OIDC_AUDIENCE":    "-7",
		"KUBEN_SSO__DISPLAY_NAME":           "99999999999999999999",
		"KUBEN_BUILD__SCANNER_IMAGE":        `""`,
		"KUBEN_RETENTION__OUTBOX_DAYS":      "4294967295",
		"KUBEN_SECURITY__LOGIN_WINDOW_SECS": "18446744073709551615",
	}
	cfg, err := jail(t, "", "", env).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := wantDefaults()
	want.Server.Roles = []config.Role{config.RoleAPI, config.RoleController}
	want.Server.PublicURL = opt.Some("https://kuben.example.com")
	want.Server.StateDir = opt.Some("/var/lib/kuben")
	want.Server.MaxBodyBytes = 2097152
	want.Runtime.WorkerThreads = opt.Some[uint64](8)
	want.Kube.Required, want.Kube.Context = true, opt.Some("")
	want.Quota.OrgCPU, want.Quota.OrgMemory, want.Quota.OrgPods = opt.Some("16"), opt.Some("64Gi"), opt.Some[uint64](500)
	want.Build.CPULimit, want.Build.NodePool, want.Build.ScannerImage = "1.50", opt.Some("pool=build"), ""
	want.Git.GithubAppID = opt.Some[uint64](123456)
	want.Git.GithubWebhookSecret = "  spaced\tsecreté "
	want.Bootstrap.AdminPassword, want.Bootstrap.OrgName, want.Bootstrap.OrgSlug = "0123456789012", "true", "a,b"
	want.Telemetry.LogLevel, want.Telemetry.OTLPEndpoint = "debug,hyper=info", opt.Some("[broken")
	want.SSO.Scopes = []string{"openid", "a b", "c", "12", "true"}
	want.SSO.Groups = map[string]string{"admins": "admin", "team.devs": "developer", "ops": "viewer"}
	want.SSO.DisplayName = "99999999999999999999"
	want.Security.TrustedProxies = []string{"10.42.0.0/16", "fd00::/8"}
	want.Security.LoginWindowSecs = 18446744073709551615
	want.Database.URL, want.Database.MaxConnections = "postgres://kuben:pw@db/kuben", 20
	want.Domains.CnameTarget = opt.Some("trueish")
	want.Backup.Dir = opt.Some(`"unterminated`)
	want.Secrets.KeyringFile = opt.Some(`"bad \x escape"`)
	want.Agent.Advertise = opt.Some("x")
	want.CI.GithubOIDCAudience = opt.Some("-7")
	want.Retention.OutboxDays = 4294967295
	if diff := cmp.Diff(want, cfg, optionals); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestValuesOfTheWrongTypeAreRefused(t *testing.T) {
	env := []struct{ name, value string }{
		{"KUBEN_SERVER__ROLES", "api"},                // one value is not a list
		{"KUBEN_SERVER__ROLES", "[api, root]"},        // not a role
		{"KUBEN_SERVER__ROLES", "[API]"},              // role names are lowercase
		{"KUBEN_SERVER", "everything"},                // a section is a table
		{"KUBEN_SERVER__BIND", "[a]"},                 // a list is not text
		{"KUBEN_SSO__SCOPES", "[openid, [x]]"},        // nor inside a list
		{"KUBEN_KUBE__REQUIRED", "yes"},               // not a boolean
		{"KUBEN_KUBE__REQUIRED", "1"},                 // nor is a number
		{"KUBEN_KUBE__REQUIRED", `"true"`},            // nor a quoted word
		{"KUBEN_QUOTA__ORG_PODS", "many"},             // not a number
		{"KUBEN_QUOTA__ORG_PODS", "1.5"},              // not a whole number
		{"KUBEN_QUOTA__ORG_PODS", "-1"},               // negative
		{"KUBEN_QUOTA__ORG_PODS", `"5"`},              // text is not a number
		{"KUBEN_QUOTA__ORG_PODS", "true"},             // nor is a boolean
		{"KUBEN_RETENTION__USAGE_DAYS", "4294967296"}, // beyond 32 bits
		{"KUBEN_SECURITY__LOGIN_WINDOW_SECS", "18446744073709551616"},
		{"KUBEN_SSO__GROUPS", "admins"},
	}
	for _, e := range env {
		if _, err := jail(t, "", "", map[string]string{e.name: e.value}).Load(); err == nil {
			t.Errorf("%s=%s must be refused", e.name, e.value)
		}
	}
	files := []string{
		"[server]\nbind = 8080\n",
		"[server]\nroles = \"api\"\n",
		"[server]\nroles = [\"root\"]\n",
		"server = \"x\"\n",
		"[build]\ncpu_limit = 2\n",
		"[kube]\nrequired = \"true\"\n",
		"[quota]\norg_pods = 1.0\n",
		"[quota]\norg_pods = -1\n",
		"[quota]\norg_pods = \"5\"\n",
		"[quota]\norg_cpu = 16\n",
		"[retention]\nusage_days = 4294967296\n",
		"[server\nbind = \"x\"\n",
	}
	for _, toml := range files {
		if _, err := jail(t, "", toml, nil).Load(); err == nil {
			t.Errorf("%q must be refused", toml)
		}
	}
}

func TestFilesLayerBelowTheEnvironment(t *testing.T) {
	system := `
[server]
bind = "10.0.0.1:80"
metrics_bind = "10.0.0.1:81"
activator_bind = "10.0.0.1:82"
roles = ["api"]

[sso]
scopes = ["openid"]

[sso.groups]
admins = "admin"
"platform.devs" = "developer"
`
	local := `
unknown_section = { anything = 1 }

[server]
metrics_bind = "10.0.0.2:81"
activator_bind = "10.0.0.2:82"
Bind = "keys are case-sensitive"
unknown_key = true

[sso.groups]
ops = "viewer"

[git]
github_app_id = 42
`
	cfg, err := jail(t, system, local, map[string]string{"KUBEN_SERVER__ACTIVATOR_BIND": "10.0.0.3:82"}).Load()
	if err != nil {
		t.Fatal(err)
	}
	want := wantDefaults()
	want.Server.Bind, want.Server.MetricsBind, want.Server.ActivatorBind = "10.0.0.1:80", "10.0.0.2:81", "10.0.0.3:82"
	want.Server.Roles = []config.Role{config.RoleAPI}
	want.SSO.Scopes = []string{"openid"}
	want.SSO.Groups = map[string]string{"admins": "admin", "platform.devs": "developer", "ops": "viewer"}
	want.Git.GithubAppID = opt.Some[uint64](42)
	if diff := cmp.Diff(want, cfg, optionals); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestTheCLILayersItsFileAboveTheOthers(t *testing.T) {
	src := jail(t, "", "[server]\nbind = \"local:1\"\nmetrics_bind = \"local:2\"\n", map[string]string{"KUBEN_SERVER__METRICS_BIND": "env:2"})
	extra := filepath.Join(src.Dir, "extra.toml")
	if err := os.WriteFile(extra, []byte("[server]\nbind = \"extra:1\"\nmetrics_bind = \"extra:2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src.Files = append(src.Files, extra)
	k, err := src.Koanf()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Set("telemetry.log_format", "pretty"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Extract(k)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Bind != "extra:1" || cfg.Server.MetricsBind != "env:2" || cfg.Telemetry.LogFormat != "pretty" {
		t.Errorf("got %+v, %+v", cfg.Server, cfg.Telemetry)
	}
}

func TestARelativeFileIsFoundInAParentDirectory(t *testing.T) {
	src := jail(t, "", "[server]\nbind = \"parent:1\"\n", nil)
	nested := filepath.Join(src.Dir, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	src.Dir = nested
	cfg, err := src.Load()
	if err != nil || cfg.Server.Bind != "parent:1" {
		t.Errorf("got %q, %v", cfg.Server.Bind, err)
	}
	if err := os.WriteFile(filepath.Join(nested, config.LocalFile), []byte("[server]\nbind = \"nearest:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = src.Load(); err != nil || cfg.Server.Bind != "nearest:1" {
		t.Errorf("got %q, %v", cfg.Server.Bind, err)
	}
	// A directory of that name is not a file.
	src = jail(t, "", "", nil)
	if err := os.Mkdir(filepath.Join(src.Dir, config.LocalFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(); err != nil {
		t.Error(err)
	}
}

func TestTheDefaultSourceIsTheDocumentedOne(t *testing.T) {
	src := config.DefaultSource()
	if diff := cmp.Diff([]string{"/etc/kuben/config.toml", "kuben.toml"}, src.Files); diff != "" || src.Dir != "" || src.Environ != nil {
		t.Errorf("got %+v", src)
	}
	if config.EnvPrefix != "KUBEN_" || config.EnvSeparator != "__" {
		t.Error("the environment contract changed")
	}
}

func TestSecretsNeverLeak(t *testing.T) {
	const value = "hunter2-hunter2"
	cfg, err := jail(t, "", "[bootstrap]\nadmin_password = \""+value+"\"\n", map[string]string{
		"KUBEN_DATABASE__URL":              "postgres://kuben:" + value + "@db/kuben",
		"KUBEN_SSO__CLIENT_SECRET":         value,
		"KUBEN_GIT__GITHUB_WEBHOOK_SECRET": value,
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	secrets := []config.Secret{cfg.Bootstrap.AdminPassword, cfg.SSO.ClientSecret, cfg.Git.GithubWebhookSecret}
	for _, s := range secrets {
		if s.Expose() != value || !s.IsSet() {
			t.Errorf("got %q", s.Expose())
		}
	}
	if cfg.Database.URL.Expose() != "postgres://kuben:"+value+"@db/kuben" {
		t.Errorf("got %q", cfg.Database.URL.Expose())
	}
	encoded, err := json.Marshal(map[string]any{"cfg": cfg, "list": secrets, "keys": map[config.Secret]int{secrets[0]: 1}})
	if err != nil {
		t.Fatal(err)
	}
	shown := []string{string(encoded)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		shown = append(shown, fmt.Sprintf(verb, cfg), fmt.Sprintf(verb, &cfg), fmt.Sprintf(verb, secrets), fmt.Sprintf(verb, secrets[0]))
	}
	for _, text := range shown {
		if strings.Contains(text, "hunter2") || strings.Contains(text, fmt.Sprintf("%x", value)) {
			t.Errorf("leaked: %s", text)
		}
	}
	if got := fmt.Sprintf("%v %+v %#v %s", secrets[0], secrets[0], secrets[0], secrets[0]); got != "[redacted] [redacted] [redacted] [redacted]" {
		t.Errorf("got %q", got)
	}
	if got, _ := json.Marshal(secrets[0]); string(got) != `"[redacted]"` {
		t.Errorf("got %s", got)
	}
	if got, _ := secrets[0].MarshalText(); string(got) != "[redacted]" {
		t.Errorf("got %s", got)
	}
}

func TestEnumForms(t *testing.T) {
	for _, name := range []string{"all", "api", "controller", "activator"} {
		role, err := config.ParseRole(name)
		encoded, _ := json.Marshal(role)
		var back config.Role
		if err != nil || string(role) != name || string(encoded) != `"`+name+`"` || json.Unmarshal(encoded, &back) != nil || back != role {
			t.Errorf("%s: got %q, %s, %v", name, role, encoded, err)
		}
	}
	var role config.Role
	for _, bad := range []string{`"All"`, `"root"`, `""`} {
		if err := json.Unmarshal([]byte(bad), &role); err == nil {
			t.Errorf("%s is not a role", bad)
		}
	}
	forms := []struct {
		value config.CookieSecure
		json  string
	}{{config.CookieFixed(true), "true"}, {config.CookieFixed(false), "false"}, {config.CookieAuto{}, `"auto"`}}
	for _, f := range forms {
		encoded, err := json.Marshal(f.value)
		if err != nil || string(encoded) != f.json {
			t.Errorf("got %s, %v", encoded, err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if back, err := config.DecodeCookieSecure(decoded); err != nil || back != f.value {
			t.Errorf("got %#v, %v", back, err)
		}
	}
	for _, bad := range []any{"true", "Auto", 1, nil, 1.5} {
		if _, err := config.DecodeCookieSecure(bad); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func FuzzEnvValue(f *testing.F) {
	for _, seed := range []string{
		"", "true", "false", "12", "-3", "1.5", `"quoted é"`, "'c'", "[a,b]", "[1,[2],3]",
		"{a=1,b=hi}", `{"a.b"=[x]}`, "[", "{", `"`, "'", "[[ -1], {a=[ b ]},  hi ]", "a,b", "\\", `"\U0010FFFF"`, `"\ud800"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsRune(value, 0) {
			t.Skip("not an environment value")
		}
		src := config.Source{Environ: func() []string {
			return []string{"KUBEN_SERVER__BIND=" + value, "KUBEN_SSO__GROUPS=" + value, "KUBEN_SSO__SCOPES=" + value}
		}}
		// Anything may be refused, nothing may panic, and a value that loads
		// as text is never longer than what was written.
		if cfg, err := src.Load(); err == nil && len(cfg.Server.Bind) > len(value) {
			t.Errorf("%q loaded as %q", value, cfg.Server.Bind)
		}
	})
}

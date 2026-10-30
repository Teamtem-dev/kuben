package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
)

func TestOnlyAPublicIPv4AddressGetsAnSslipDomain(t *testing.T) {
	for ip, want := range map[string]opt.Val[string]{
		"45.156.185.51": opt.Some("45-156-185-51.sslip.io"),
		"10.0.0.4":      opt.None[string](), // behind NAT: Let's Encrypt cannot reach it
		"192.168.1.20":  opt.None[string](),
		"100.64.3.9":    opt.None[string](), // carrier-grade NAT
		"127.0.0.1":     opt.None[string](),
		"2001:db8::1":   opt.None[string](), // sslip.io names here are IPv4
		"":              opt.None[string](),
	} {
		if got := sslipDomain(ip); got != want {
			t.Errorf("sslipDomain(%q) = %v, want %v", ip, got, want)
		}
	}
}

// A later run keeps the sslip.io domain an earlier one chose, without
// asking again (an upgrade through the installer has no terminal).
func TestAnEarlierSslipDomainIsReadFromThePublicURL(t *testing.T) {
	sslip := configTemplate("0.0.0.0", 3001, "https://kuben.45-156-185-51.sslip.io", "/k")
	if got := configuredSslip(opt.Some(sslip)); got != opt.Some("45-156-185-51.sslip.io") {
		t.Errorf("configuredSslip = %v", got)
	}
	for _, other := range []string{"http://45.156.185.51:3001", "https://kuben.apps.example.com"} {
		text := configTemplate("0.0.0.0", 3001, other, "/k")
		if got := configuredSslip(opt.Some(text)); got.IsSome() {
			t.Errorf("configuredSslip(%s) = %v, want none", other, got)
		}
	}
	if got := configuredSslip(opt.None[string]()); got.IsSome() {
		t.Errorf("configuredSslip(none) = %v", got)
	}
}

// An install that had no console gets one: its public_url turns https and
// its [security] trusts the Gateway's proxies; the rest stays.
func TestAConsoleAddedLaterUpdatesAnOwnedConfiguration(t *testing.T) {
	before := configTemplate("0.0.0.0", 3001, "http://45.156.185.51:3001", "/k") +
		securitySection(configWants{insecureSetup: true})
	got := updatedConfig(before, 3001, configWants{console: opt.Some("kuben.45-156-185-51.sslip.io"), insecureSetup: true}, true)
	cfg := loadTOML(t, got.text)
	if cfg.Server.PublicURL.Or("") != "https://kuben.45-156-185-51.sslip.io" {
		t.Errorf("public_url = %v", cfg.Server.PublicURL)
	}
	if !cfg.Security.TrustForwardedFor || !cfg.Security.InsecureSetup {
		t.Errorf("security = %+v", cfg.Security)
	}
	// Once is enough: a second run changes nothing.
	again := updatedConfig(got.text, 3001, configWants{console: opt.Some("kuben.45-156-185-51.sslip.io"), insecureSetup: true}, true)
	if diff := cmp.Diff(got.text, again.text); diff != "" {
		t.Errorf("second run (-first +second):\n%s", diff)
	}
	// Someone else's configuration stays theirs.
	theirs := updatedConfig(before, 3001, configWants{console: opt.Some("kuben.45-156-185-51.sslip.io")}, false)
	if theirs.text != before {
		t.Errorf("someone else's configuration changed:\n%s", theirs.text)
	}
}

func TestAnIssuerWithoutEmailHasNoEmailField(t *testing.T) {
	acme := func(email string) map[string]any {
		spec, _ := clusterIssuer(email, false)["spec"].(map[string]any)
		out, _ := spec["acme"].(map[string]any)
		return out
	}
	if _, has := acme("")["email"]; has {
		t.Error("an issuer without email names one")
	}
	if got := acme("ops@example.com")["email"]; got != "ops@example.com" {
		t.Errorf("email = %v", got)
	}
}

func TestAppsUnderSslipNeedNoDNSRecord(t *testing.T) {
	got := appsNote(opt.Some(Wanted{Domain: opt.Some("45-156-185-51.sslip.io"), AcmeEmail: opt.Some("")}))
	want := "Apps get HTTPS addresses under 45-156-185-51.sslip.io; no DNS record to make."
	if got != want {
		t.Errorf("appsNote = %q, want %q", got, want)
	}
}

// The cases decided without asking: an earlier choice, --sslip, --yes, an
// address Let's Encrypt cannot reach, a domain of one's own.
func TestWhenSetupPicksAnSslipDomainWithoutAsking(t *testing.T) {
	public, private := opt.Some("45.156.185.51"), opt.Some("10.0.0.4")
	earlier := opt.Some(configTemplate("0.0.0.0", 3001, "https://kuben.45-156-185-51.sslip.io", "/k"))
	none := opt.None[string]()
	sslip := Wanted{Domain: opt.Some("45-156-185-51.sslip.io"), AcmeEmail: opt.Some("")}
	for name, c := range map[string]struct {
		opts       Opts
		hub        opt.Val[string]
		configured opt.Val[string]
		want       Wanted
	}{
		"an earlier run chose it":      {Opts{Yes: true}, public, earlier, sslip},
		"--sslip":                      {Opts{Sslip: true}, public, none, sslip},
		"--sslip keeps --acme-email":   {Opts{Sslip: true, AcmeEmail: opt.Some("ops@example.com")}, public, none, Wanted{Domain: sslip.Domain, AcmeEmail: opt.Some("ops@example.com")}},
		"--yes does not opt in":        {Opts{Yes: true}, public, none, Wanted{}},
		"no public address":            {Opts{Sslip: true}, private, none, Wanted{}},
		"no address at all":            {Opts{Sslip: true}, none, none, Wanted{}},
		"a domain of one's own":        {Opts{Sslip: true, Domain: opt.Some("apps.example.com")}, public, earlier, Wanted{Domain: opt.Some("apps.example.com")}},
		"--bind-local keeps it closed": {Opts{Sslip: true, BindLocal: true}, public, none, Wanted{}},
	} {
		m, _, _ := testMachine(t)
		got := m.withSslip(c.opts, c.hub, c.configured).wanted()
		if diff := cmp.Diff(c.want, got, cmp.Comparer(func(a, b opt.Val[string]) bool { return a == b })); diff != "" {
			t.Errorf("%s (-want +got):\n%s", name, diff)
		}
	}
}

// certificates is a cluster that holds one Certificate of the console.
type certificates struct {
	cluster
	ready bool
	asked *[3]string
}

func (c certificates) get(_ context.Context, _, kind, namespace, name string) (*unstructured.Unstructured, bool, error) {
	*c.asked = [3]string{kind, namespace, name}
	status := "False"
	if c.ready {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": status}},
	}}}, true, nil
}

// The link is printed once it opens; without a certificate setup says what
// to look at and goes on, it does not fail an install that otherwise worked.
func TestSetupWaitsForTheConsolesCertificate(t *testing.T) {
	host := "kuben.45-156-185-51.sslip.io"
	for _, c := range []struct {
		ready bool
		want  string
	}{
		{true, "issued by Let's Encrypt"},
		{false, "Let's Encrypt must reach ports 80 and 443"},
	} {
		m, _, stderr := testMachine(t)
		var asked [3]string
		m.waitForCertificate(context.Background(), certificates{ready: c.ready, asked: &asked}, host)
		if !strings.Contains(stderr.String(), c.want) {
			t.Errorf("ready=%v: output %q does not say %q", c.ready, stderr.String(), c.want)
		}
		if want := [3]string{"Certificate", GatewayNamespace, render.HostSecretName(host)}; asked != want {
			t.Errorf("asked for %v, want %v", asked, want)
		}
	}
}

// backupRunner answers backup commands, and records them.
type backupRunner struct {
	failingRunner
	failed bool
	ran    *[][]string
}

func (r backupRunner) output(_ context.Context, _ []string, argv ...string) (commandOutput, error) {
	*r.ran = append(*r.ran, argv)
	if r.failed {
		return commandOutput{}, errors.New("pg_dump: not found")
	}
	return commandOutput{ok: true}, nil
}

// The backup runs as the service user; a failed one is a warning, because
// the new binary is already in place.
func TestAnUpgradeBacksUpFirstAndGoesOnWithoutABackup(t *testing.T) {
	for _, failed := range []bool{false, true} {
		m, _, stderr := testMachine(t)
		var ran [][]string
		m.runner = backupRunner{failed: failed, ran: &ran}
		m.backupBeforeUpgrade(context.Background())
		if diff := cmp.Diff([][]string{{"runuser", "-u", User, "--", Bin, "backup"}}, ran); diff != "" {
			t.Errorf("commands (-want +got):\n%s", diff)
		}
		if got := strings.Contains(stderr.String(), "no backup before the upgrade"); got != failed {
			t.Errorf("failed=%v: warned=%v in %q", failed, got, stderr.String())
		}
	}
}

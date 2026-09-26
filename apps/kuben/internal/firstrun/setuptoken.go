package firstrun

// The setup token and link (setup.rs): on an address other than loopback
// the first-run request must carry the setup token, 128 random bits the
// server writes to SetupTokenFile in the state directory (owner-only) and
// the installer prints in the setup link's fragment. The HTTP routes that
// check it are in internal/httpapi; the server, `kuben setup` and `kuben
// setup-token` issue and print it from here.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/host"
)

// SetupTokenFile holds the current setup token, in the state directory.
const SetupTokenFile = "setup-token"

// SetupTokenTTL is how long a setup token stays valid.
const SetupTokenTTL = 30 * time.Minute

// SetupTokenPath is where cfg keeps the setup token.
func SetupTokenPath(cfg config.Config) string { return filepath.Join(cfg.StateDir(), SetupTokenFile) }

// SetupTokenRequired reports whether first-run setup needs the installer
// token: always, unless the console listens on loopback only.
func SetupTokenRequired(cfg config.Config) bool { return !cfg.BindIsLoopback() }

// IssueSetupToken writes a fresh token and returns it.
func IssueSetupToken(cfg config.Config) (string, error) {
	var b [16]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails (Go ≥ 1.24)
	token := base64.RawURLEncoding.EncodeToString(b[:])
	path := SetupTokenPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("setup token: %w", err)
	}
	if err := host.WriteOwnerOnly(path, token+"\n"); err != nil {
		return "", fmt.Errorf("setup token: %w", err)
	}
	return token, nil
}

// CurrentOrNewSetupToken is the current token while it is valid, else a
// fresh one.
func CurrentOrNewSetupToken(cfg config.Config, now time.Time) (string, error) {
	if token, age, ok := readSetupToken(cfg, now); ok && age <= SetupTokenTTL {
		return token, nil
	}
	return IssueSetupToken(cfg)
}

func readSetupToken(cfg config.Config, now time.Time) (string, time.Duration, bool) {
	path := SetupTokenPath(cfg)
	data, err := os.ReadFile(path) //nolint:gosec // our own state file
	if err != nil {
		return "", 0, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, false
	}
	token := strings.TrimSpace(string(data))
	return token, max(now.Sub(info.ModTime()), 0), token != ""
}

// VerifySetupToken checks the token a first-run request presented: nothing
// to check when the console listens on loopback only, else the current,
// unexpired token (compared in constant time) or kerrors.ErrForbidden.
func VerifySetupToken(cfg config.Config, presented opt.Val[string], now time.Time) error {
	if !SetupTokenRequired(cfg) {
		return nil
	}
	given, ok := presented.Get()
	current, age, found := readSetupToken(cfg, now)
	if !ok || !found || age > SetupTokenTTL || subtle.ConstantTimeCompare([]byte(given), []byte(current)) != 1 {
		return kerrors.ErrForbidden
	}
	return nil
}

// SetupURL is the setup link on this server's console address
// (setup::setup_url): `server.public_url`, else the advertised address
// (host::console_url; localhost when unknown).
func SetupURL(cfg config.Config, token, advertise opt.Val[string]) string {
	return SetupURLAt(cfg.ConsoleURLWithHost(advertise.Or("localhost")), token)
}

// SetupGuide is the link to open and the notes that go with it
// (setup::setup_guide): the direct link on a loopback bind or with
// `security.insecure_setup`; else, over plain http, an SSH tunnel so the
// admin password never travels unencrypted, and over https the direct link
// with the tunnel as the way in until DNS and the certificate are ready.
func SetupGuide(cfg config.Config, token, advertise opt.Val[string]) (string, []string) {
	port := cfg.BindPort()
	direct := SetupURL(cfg, token, advertise)
	if cfg.BindIsLoopback() || cfg.Security.InsecureSetup {
		return direct, nil
	}
	server := advertise.Or("<this server>")
	ssh := fmt.Sprintf("ssh -L %[1]d:127.0.0.1:%[1]d <you>@%[2]s", port, server)
	tunnel := SetupURLAt(fmt.Sprintf("http://localhost:%d", port), token)
	if strings.HasPrefix(direct, "https://") {
		return direct, []string{fmt.Sprintf("Until DNS and the certificate are ready: run `%s` on your computer, then open %s", ssh, tunnel)}
	}
	return tunnel, []string{
		fmt.Sprintf("Run `%s` on your computer first; the admin password never travels over plain HTTP.", ssh),
		"For an HTTPS console: kuben setup --domain <domain> --acme-email <email>. On a network you " +
			"trust: kuben setup --allow-http-setup.",
	}
}

// SetupURLAt is the setup link on console, with the token in the fragment.
func SetupURLAt(console string, token opt.Val[string]) string {
	fragment := ""
	if t, ok := token.Get(); ok {
		fragment = "#token=" + t
	}
	return strings.TrimRight(console, "/") + "/setup" + fragment
}

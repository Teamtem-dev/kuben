package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// legacyDataDir is the volume of the container image and the Helm chart,
// and where binaries before 1.0.3 kept their data.
const legacyDataDir = "/data"

// InCluster reports whether this process runs in a Kubernetes pod.
func InCluster() bool {
	_, set := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	return set
}

// DefaultDataDir is the state directory when `server.state_dir` is not set:
// an existing `/data` (legacyExists), else the first directory of systemd's
// `$STATE_DIRECTORY`, `$XDG_STATE_HOME/kuben`, `$HOME/.local/state/kuben`,
// `%LOCALAPPDATA%\kuben`, or the working directory. getenv returns "" for a
// variable that is not set; empty variables count as unset.
func DefaultDataDir(legacyExists bool, getenv func(string) string) string {
	if legacyExists {
		return legacyDataDir
	}
	// systemd sets `$STATE_DIRECTORY` for `StateDirectory=`: absolute paths,
	// colon-separated when the unit lists several.
	for dir := range strings.SplitSeq(getenv("STATE_DIRECTORY"), ":") {
		if dir != "" {
			return dir
		}
	}
	if state := getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "kuben")
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "kuben")
	}
	if local := getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "kuben")
	}
	return "."
}

// GithubOIDCAudience is the audience of GitHub Actions OIDC tokens, if CI
// trust is on and one is known: `ci.github_oidc_audience`, else
// `server.public_url`, without surrounding space and trailing slashes.
func (c Config) GithubOIDCAudience() (string, bool) {
	if !c.CI.GithubActions {
		return "", false
	}
	audience, ok := c.CI.GithubOIDCAudience.Get()
	if !ok {
		audience, ok = c.Server.PublicURL.Get()
	}
	audience = strings.TrimRight(strings.TrimSpace(audience), "/")
	return audience, ok && audience != ""
}

// HasRole reports whether role is enabled (`all` enables every role).
func (c Config) HasRole(role Role) bool {
	return slices.ContainsFunc(c.Server.Roles, func(r Role) bool { return r == RoleAll || r == role })
}

// InsecureCookieWarning is why signing in would fail, if it would: browsers
// drop a `Secure` cookie over plain http (except on `localhost`), so a
// console reached at `http://<server-ip>:8080` loops back to the login page
// without an error. Nothing in a pod (reached through a port-forward on
// localhost or a TLS Gateway), with an https public URL, or when bound to
// loopback.
func (c Config) InsecureCookieWarning(inCluster bool) (string, bool) {
	if !c.CookieSecure() || inCluster || c.PublicURLIsHTTPS() || c.BindIsLoopback() {
		return "", false
	}
	return fmt.Sprintf("the session cookie is Secure and browsers drop it over plain http, so signing in at "+
		"http://<this server>:%d loops back to the login page. Serve the console over "+
		"HTTPS (and set KUBEN_SERVER__PUBLIC_URL), open it through an SSH tunnel to "+
		"localhost, or set KUBEN_SECURITY__COOKIE_SECURE=auto", c.BindPort()), true
}

// CookieSecure reports whether the session cookie gets `Secure`:
// `security.cookie_secure`, with `auto` resolved against
// `server.public_url`.
func (c Config) CookieSecure() bool {
	switch v := c.Security.CookieSecure.(type) {
	case CookieFixed:
		return bool(v)
	case CookieAuto:
		return c.PublicURLIsHTTPS()
	}
	return c.PublicURLIsHTTPS()
}

// PublicURLIsHTTPS reports whether the console is published over https.
func (c Config) PublicURLIsHTTPS() bool {
	return strings.HasPrefix(c.Server.PublicURL.Or(""), "https://")
}

// SetupWizard reports whether the first admin is created from the console
// (`/setup`) instead of a configured or generated password: outside a
// cluster (see [InCluster]), when `bootstrap.admin_password` is not set. A
// pod keeps the Secret flow.
func (c Config) SetupWizard(inCluster bool) bool {
	return !inCluster && !c.Bootstrap.AdminPassword.IsSet()
}

// ConsoleURLWithHost is the console's address for links:
// `server.public_url`, else plain http on host and the bound port.
func (c Config) ConsoleURLWithHost(host string) string {
	if url := c.Server.PublicURL.Or(""); url != "" {
		return strings.TrimRight(url, "/")
	}
	return fmt.Sprintf("http://%s:%d", host, c.BindPort())
}

// BindPort is the port of `server.bind`; 8080 when it names none.
func (c Config) BindPort() uint16 {
	bind := c.Server.Bind
	port, err := strconv.ParseUint(strings.TrimPrefix(bind[strings.LastIndex(bind, ":")+1:], "+"), 10, 16)
	if err != nil {
		return 8080
	}
	return uint16(port)
}

// BindIsLoopback reports whether the API listens on loopback only.
func (c Config) BindIsLoopback() bool {
	addr, err := netip.ParseAddrPort(c.Server.Bind)
	if err != nil {
		return strings.HasPrefix(c.Server.Bind, "localhost:")
	}
	return addr.Addr().IsLoopback() && !addr.Addr().Is4In6()
}

// BackupDir is where backups go.
func (c Config) BackupDir() string {
	if dir := c.Backup.Dir.Or(""); dir != "" {
		return dir
	}
	return filepath.Join(c.StateDir(), "backups")
}

// SecretKeyringFile is the keyring file of managed secrets.
func (c Config) SecretKeyringFile() string {
	if file := c.Secrets.KeyringFile.Or(""); file != "" {
		return file
	}
	return filepath.Join(c.StateDir(), "secrets.keyring")
}

// StateDir is where files that belong to this installation go (the setup
// token, a generated initial admin password): `server.state_dir` when set.
// Else an existing `/data` (the container volume, and where binaries before
// 1.0.3 kept their data), systemd's `StateDirectory=`, the user's state
// directory (`~/.local/state/kuben`), or the working directory, so `kuben
// serve` and `kuben setup-token` work without root.
func (c Config) StateDir() string {
	if dir := c.Server.StateDir.Or(""); dir != "" {
		return dir
	}
	legacy := false
	if info, err := os.Stat(legacyDataDir); err == nil {
		legacy = info.IsDir()
	}
	return DefaultDataDir(legacy, os.Getenv)
}

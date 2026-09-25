package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

// First run (setup.rs): GET /setup says whether the first admin still has
// to be created; POST /setup creates it and signs in. On an address other
// than loopback the request must carry the setup token — 128 random bits
// the server writes to SetupTokenFile in the state directory (owner-only)
// and the installer prints in the setup link's fragment. The admin's
// password never travels over plain HTTP from another machine (ADR-031).

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
	if err := WriteOwnerOnly(path, token+"\n"); err != nil {
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

// WriteOwnerOnly replaces path with a new file readable by its owner only,
// so a leftover never keeps a wider mode.
func WriteOwnerOnly(path, content string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the configured state directory
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close() //nolint:errcheck // the write error is the one to report
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close() //nolint:wrapcheck // the path is in the caller's message
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

func verifySetupToken(cfg config.Config, presented opt.Val[string], now time.Time) error {
	if !SetupTokenRequired(cfg) {
		return nil
	}
	given, ok := presented.Get()
	current, age, found := readSetupToken(cfg, now)
	if !ok || !found || age > SetupTokenTTL || subtle.ConstantTimeCompare([]byte(given), []byte(current)) != 1 {
		return kerr.ErrForbidden
	}
	return nil
}

// TransportSecure reports whether the admin's password may travel over
// this request: HTTPS through a trusted proxy, from this machine, to a
// loopback-only console, or allowed by security.insecure_setup.
func TransportSecure(cfg config.Config, forwardedProto string, peer opt.Val[netip.Addr], clientIP opt.Val[string]) bool {
	if cfg.BindIsLoopback() || cfg.Security.InsecureSetup {
		return true
	}
	https := false
	if cfg.Security.TrustsForwarded(peer) && forwardedProto != "" {
		hops := strings.Split(forwardedProto, ",")
		https = strings.EqualFold(strings.TrimSpace(hops[len(hops)-1]), "https")
	}
	local := false
	if ipText, ok := clientIP.Get(); ok {
		if ip, err := netip.ParseAddr(ipText); err == nil {
			local = ip.Unmap().IsLoopback()
		}
	}
	return https || local
}

// InsecureTransportHint says why the admin cannot be created over this
// connection and what works instead.
func InsecureTransportHint(cfg config.Config) string {
	port := cfg.BindPort()
	return fmt.Sprintf("the admin password is not sent over plain HTTP from another machine. Open the console over HTTPS, or "+
		"through an SSH tunnel: ssh -L %[1]d:127.0.0.1:%[1]d <you>@<this server>, then "+
		"http://localhost:%[1]d/setup#token=<token> (kuben setup-token prints it). To allow plain HTTP on a "+
		"trusted network, set security.insecure_setup = true", port)
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

func (s *Server) setupNeeded(ctx context.Context) (bool, error) {
	if !s.deps.Config.SetupWizard(s.deps.InCluster) {
		return false, nil
	}
	n, err := s.deps.Store.CountUsers(ctx)
	if err != nil {
		return false, err //nolint:wrapcheck // a store error, answered as internal
	}
	return n == 0, nil
}

func (s *Server) transportSecure(ctx context.Context) bool {
	req := httpx.RequestFrom(ctx)
	return TransportSecure(s.deps.Config, req.ForwardedProto, req.Peer, req.IP)
}

// SetupStatus says whether the first admin account still has to be created.
func (s *Server) SetupStatus(ctx context.Context) (*gen.SetupStatus, error) {
	needed, err := s.setupNeeded(ctx)
	if err != nil {
		return nil, err
	}
	return &gen.SetupStatus{
		Needed:        needed,
		TokenRequired: needed && SetupTokenRequired(s.deps.Config),
		Secure:        s.transportSecure(ctx),
	}, nil
}

// Setup creates the organization and its first admin (owner) and signs in.
func (s *Server) Setup(ctx context.Context, req *gen.SetupRequest) (gen.SetupRes, error) {
	s.setupMu.Lock() // two browsers cannot both become the first admin
	defer s.setupMu.Unlock()
	needed, err := s.setupNeeded(ctx)
	if err != nil {
		return nil, err
	}
	if !needed {
		return nil, kerr.New(kerr.NotFound, "setup is complete; sign in instead")
	}
	if !s.transportSecure(ctx) {
		return nil, kerr.New(kerr.InsecureTransport, "%s", InsecureTransportHint(s.deps.Config))
	}
	token := opt.None[string]()
	if t, ok := req.Token.Get(); ok {
		token = opt.Some(t)
	}
	if err := verifySetupToken(s.deps.Config, token, time.UnixMilli(s.deps.Clock.NowMs())); err != nil {
		return nil, err
	}
	email := ascii.Lower(strings.TrimSpace(req.Email))
	if len(email) < 3 || !strings.Contains(email, "@") {
		return nil, kerr.New(kerr.Validation, "enter a valid email address")
	}
	orgName := strings.TrimSpace(req.OrgName)
	if orgName == "" {
		return nil, kerr.New(kerr.Validation, "the organization needs a name")
	}
	minLen := s.deps.Config.Security.PasswordMinLength
	if uint64(utf8.RuneCountInString(req.Password)) < minLen { //nolint:gosec // a count is never negative
		return nil, kerr.New(kerr.Validation, "the password needs at least %d characters", minLen)
	}
	var hash string
	var hashErr error
	if err := s.withPermit(ctx, func() { hash, hashErr = s.deps.Hasher.Hash(req.Password) }); err != nil {
		return nil, err
	}
	if hashErr != nil {
		return nil, kerr.Wrap(hashErr, "hash the password")
	}
	slug := s.deps.Config.Bootstrap.OrgSlug
	org, found, err := s.deps.Store.FindOrgBySlug(ctx, slug)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		if org, err = s.deps.Store.CreateOrg(ctx, slug, orgName); err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	user, err := s.deps.Store.CreateUser(ctx, email, opt.None[string](), opt.Some(hash))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := s.deps.Store.AddMembership(ctx, org.ID, user.ID); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := s.deps.Store.BindOrgRole(ctx, org.ID, user.ID, perm.Owner); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	_ = os.Remove(SetupTokenPath(s.deps.Config)) //nolint:errcheck // gone already is fine
	s.deps.Logger.Info("setup complete: admin account created", "email", email, "org", org.Slug)
	return s.startSession(ctx, user)
}

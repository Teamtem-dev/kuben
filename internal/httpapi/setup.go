package httpapi

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/firstrun"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
)

// First run (setup.rs): GET /setup says whether the first admin still has
// to be created; POST /setup creates it and signs in. On an address other
// than loopback the request must carry the setup token — 128 random bits
// the server writes to firstrun.SetupTokenFile in the state directory (owner-only)
// and the installer prints in the setup link's fragment. The admin's
// password never travels over plain HTTP from another machine (ADR-031).

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
		TokenRequired: needed && firstrun.SetupTokenRequired(s.deps.Config),
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
		return nil, kerrors.New(kerrors.NotFound, "setup is complete; sign in instead")
	}
	if !s.transportSecure(ctx) {
		return nil, kerrors.New(kerrors.InsecureTransport, "%s", InsecureTransportHint(s.deps.Config))
	}
	token := opt.None[string]()
	if t, ok := req.Token.Get(); ok {
		token = opt.Some(t)
	}
	if err := firstrun.VerifySetupToken(s.deps.Config, token, time.UnixMilli(s.deps.Clock.NowMs())); err != nil {
		return nil, err
	}
	email := ascii.Lower(strings.TrimSpace(req.Email))
	if len(email) < 3 || !strings.Contains(email, "@") {
		return nil, kerrors.New(kerrors.Validation, "enter a valid email address")
	}
	orgName := strings.TrimSpace(req.OrgName)
	if orgName == "" {
		return nil, kerrors.New(kerrors.Validation, "the organization needs a name")
	}
	minLen := s.deps.Config.Security.PasswordMinLength
	if uint64(utf8.RuneCountInString(req.Password)) < minLen { //nolint:gosec // a count is never negative
		return nil, kerrors.New(kerrors.Validation, "the password needs at least %d characters", minLen)
	}
	var hash string
	var hashErr error
	if err := s.withPermit(ctx, func() { hash, hashErr = s.deps.Hasher.Hash(req.Password) }); err != nil {
		return nil, err
	}
	if hashErr != nil {
		return nil, kerrors.Wrap(hashErr, "hash the password")
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
	_ = os.Remove(firstrun.SetupTokenPath(s.deps.Config)) //nolint:errcheck // gone already is fine
	s.deps.Logger.Info("setup complete: admin account created", "email", email, "org", org.Slug)
	return s.startSession(ctx, user)
}

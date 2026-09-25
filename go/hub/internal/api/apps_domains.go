package api

// DNS check of an app's hostnames against the gateway (scenario 9,
// routes/apps/domains.rs). The verdicts are the doctor's.

import (
	"context"
	"net/netip"

	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
)

// appHosts returns the public hostnames of the app: explicit domains first,
// then the generated one (if base domain and environment are set).
func appHosts(a appScope, p render.Platform) []string {
	var domains []string
	if spec, ok := desiredSpec(a.app); ok {
		for _, d := range spec.Domains {
			domains = append(domains, d.Host)
		}
	}
	return render.HostnamesFor(a.app.Slug, opt.Some(a.env.resourceName()), domains, p)
}

// CheckAppDomains checks that every hostname of the app points at the gateway.
func (s *Server) CheckAppDomains(ctx context.Context, params gen.CheckAppDomainsParams) (gen.CheckAppDomainsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	platform := doctor.ReadPlatform(ctx, cluster.Dynamic)
	expected := doctor.ReadGateway(ctx, cluster.Dynamic, platform, s.deps.Resolver).Addresses()
	hosts := appHosts(a, platform)
	checks, err := checkHosts(ctx, s.deps.Resolver, hosts, expected)
	if err != nil {
		return nil, err
	}
	res := gen.CheckAppDomainsOKApplicationJSON(checks)
	return &res, nil
}

// checkHosts looks every host up at once and judges it against the
// Gateway's addresses gateway; checks keep the order of hosts.
func checkHosts(ctx context.Context, r doctor.Resolver, hosts []string, gateway []netip.Addr) ([]gen.DomainCheck, error) {
	checks := make([]gen.DomainCheck, len(hosts))
	var g errgroup.Group
	for i, host := range hosts {
		g.Go(func() error {
			resolved := doctor.Resolve(ctx, r, host)
			verdict, message := doctor.DNSVerdict(resolved, gateway)
			checks[i] = gen.DomainCheck{
				Host:      host,
				Addresses: addressStrings(resolved),
				Expected:  addressStrings(gateway),
				Status:    string(verdict),
				Message:   message,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err //nolint:wrapcheck // no lookup fails: an unresolved host has no address
	}
	return checks, nil
}

// addressStrings is ips as text, never nil (an empty JSON array).
func addressStrings(ips []netip.Addr) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

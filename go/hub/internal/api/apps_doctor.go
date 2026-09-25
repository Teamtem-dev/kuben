package api

// Doctor of an app (M2.13, routes/apps/doctor.rs): every check between the
// app and a visitor, from the Gateway's class to the agent that delivers
// it. The verdicts are platform/doctor's.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"

	"github.com/go-faster/jx"
	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/domain"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// factsFreshMs: a recorded observation older than this is asked again.
const factsFreshMs = 5 * 60 * 1000

// errNoNameServerLookup stands for the DNS-over-HTTPS resolver of dns.rs,
// which is not ported yet: the delegation of a custom domain is unknown.
var errNoNameServerLookup = errors.New("DNS-over-HTTPS lookups are not available in this version")

// GetAppDoctor is why the app is or is not reachable: the GatewayClass and
// the Gateway, the issuer, ports 80 and 443 (from the server), the route,
// each host's certificate and DNS, each custom domain's claim, delegation
// and proxy, and the agent that delivers it.
func (s *Server) GetAppDoctor(ctx context.Context, params gen.GetAppDoctorParams) (gen.GetAppDoctorRes, error) {
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
	checks, err := s.doctorChecks(ctx, a, cluster, &net.Dialer{})
	if err != nil {
		return nil, err
	}
	report := doctorReport(checks)
	return &report, nil
}

// doctorChecks is every check of app a against cluster, in the report's
// order; the Gateway's ports are probed with d.
func (s *Server) doctorChecks(ctx context.Context, a appScope, cluster registry.Cluster, d doctor.Dialer) ([]doctor.Check, error) {
	platform := doctor.ReadPlatform(ctx, cluster.Dynamic)
	gateway := doctor.ReadGateway(ctx, cluster.Dynamic, platform, s.deps.Resolver)
	facts, err := s.doctorFacts(ctx, a, cluster)
	if err != nil {
		return nil, err
	}
	addresses := gateway.Addresses()

	checks := doctor.PlatformChecks(opt.Some(facts), platform, gateway)
	hosts := appHosts(a, platform)
	http, https := probePorts(ctx, d, addresses)
	checks = append(checks, doctor.PortCheck(80, http))
	if platform.TLS {
		checks = append(checks, doctor.PortCheck(443, https))
	}
	exposure := opt.None[projection.ExposureView]()
	if view, ok := s.deps.Projections.Exposure(a.app.Namespace, a.app.Slug); ok {
		exposure = opt.Some(view)
	}
	checks = append(checks, doctor.ExposureChecks(exposure, len(hosts) > 0)...)
	checks = append(checks, dnsChecks(ctx, s.deps.Resolver, hosts, addresses)...)
	domains, err := s.domainChecks(ctx, a)
	if err != nil {
		return nil, err
	}
	checks = append(checks, domains...)
	if a.app.Delivery == store.DeliveryAgent {
		checks = append(checks, agentCheck())
	}
	return checks, nil
}

// probePorts probes ports 80 and 443 of the Gateway at once.
func probePorts(ctx context.Context, d doctor.Dialer, addresses []netip.Addr) (http, https doctor.PortProbe) {
	var g errgroup.Group
	g.Go(func() error {
		http = doctor.ProbePort(ctx, d, addresses, 80)
		return nil
	})
	g.Go(func() error {
		https = doctor.ProbePort(ctx, d, addresses, 443)
		return nil
	})
	_ = g.Wait() //nolint:errcheck // a probe reports its failure, it returns none
	return http, https
}

// dnsChecks looks every host up at once and judges it against the
// Gateway's addresses gateway; the checks keep the order of hosts.
func dnsChecks(ctx context.Context, r doctor.Resolver, hosts []string, gateway []netip.Addr) []doctor.Check {
	checks := make([]doctor.Check, len(hosts))
	var g errgroup.Group
	for i, host := range hosts {
		g.Go(func() error {
			checks[i] = doctor.DNSCheck(host, doctor.Resolve(ctx, r, host), gateway)
			return nil
		})
	}
	_ = g.Wait() //nolint:errcheck // an unresolved host has no address, not an error
	return checks
}

// domainChecks is each custom domain's claim, delegation and proxy (M5.2).
// A domain that is not a valid name is left out.
func (s *Server) domainChecks(ctx context.Context, a appScope) ([]doctor.Check, error) {
	org := a.env.project.org
	var hosts []string
	if spec, ok := desiredSpec(a.app); ok {
		for _, d := range spec.Domains {
			hosts = append(hosts, d.Host)
		}
	}
	var checks []doctor.Check
	for _, name := range hosts {
		host, err := domain.Canonical(name)
		if err != nil {
			continue
		}
		claimed, owner, found, err := s.deps.Store.DomainOwner(ctx, host)
		if err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		var holder doctor.ClaimOwner = doctor.ClaimNobody{}
		switch {
		case found && owner == org:
			holder = doctor.ClaimOurs{Domain: claimed}
		case found:
			holder = doctor.ClaimOthers{}
		}
		checks = append(checks,
			doctor.ClaimCheck(host, holder, s.deps.Config.Domains.RequireClaim),
			doctor.DelegationCheck(host, domain.LockKey(host), nil, errNoNameServerLookup),
			// No DNS provider account can be read yet (dns.rs, S5), as Rust
			// without a keyring: no account holds the record.
			doctor.ProxyCheck(host, opt.None[bool]()),
		)
	}
	return checks, nil
}

// doctorFacts is what discovery recorded of the app's cluster, or, when
// that is missing, old or unreadable, what the cluster says now.
func (s *Server) doctorFacts(ctx context.Context, a appScope, cluster registry.Cluster) (discovery.ClusterFacts, error) {
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return discovery.ClusterFacts{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	record, found, err := t.ClusterCapabilities(ctx, registry.Primary)
	_ = t.Rollback(ctx) //nolint:errcheck // read only
	if err != nil {
		return discovery.ClusterFacts{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if found && s.deps.Clock.NowMs()-record.ObservedAt < factsFreshMs {
		if facts, ok := decodeFacts(record.Facts); ok {
			return facts, nil
		}
	}
	return discovery.Discover(ctx, s.deps.Logger, cluster), nil
}

// decodeFacts reads recorded facts; false when they are not facts.
func decodeFacts(recorded any) (discovery.ClusterFacts, bool) {
	var facts discovery.ClusterFacts
	data, err := json.Marshal(recorded)
	if err != nil {
		return facts, false
	}
	if err := json.Unmarshal(data, &facts); err != nil {
		return discovery.ClusterFacts{}, false
	}
	return facts, true
}

// agentCheck is the agent of an app its cluster's agent delivers. The
// store does not read an agent's enrollment and last contact yet
// (repo/agents.rs cluster_agent, ported with the AgentLink), so the check
// is unknown, never ok; doctor.AgentCheck judges it once it can be read.
func agentCheck() doctor.Check {
	return doctor.Check{
		ID: "agent", Status: doctor.StatusUnknown,
		Detail: "the agent's link could not be checked",
	}
}

// doctorReport is the report of checks. The evidence graph and its
// findings (evidence.rs, routes/apps/evidence.rs) are not ported yet: the
// graph has no node and there is no finding.
func doctorReport(checks []doctor.Check) gen.DoctorReport {
	out := make([]gen.DoctorCheck, len(checks))
	for i, c := range checks {
		out[i] = gen.DoctorCheck{
			ID:      c.ID,
			Subject: c.Subject,
			Status:  string(c.Status),
			Detail:  c.Detail,
			Hint:    optNilString(c.Hint),
		}
	}
	return gen.DoctorReport{
		Status:   string(doctor.Overall(checks)),
		Checks:   out,
		Graph:    gen.DoctorReportGraph{"nodes": jx.Raw("[]"), "edges": jx.Raw("[]")},
		Findings: []gen.DoctorReportFindingsItem{},
	}
}

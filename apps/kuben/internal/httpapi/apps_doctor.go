package httpapi

// Doctor of an app (M2.13, routes/apps/doctor.rs): every check between the
// app and a visitor, from the Gateway's class to the agent that delivers
// it. The verdicts are doctor's.

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/netip"
	"slices"
	"unicode/utf8"

	"github.com/go-faster/jx"
	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/evidence"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/dns"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/notify"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// factsFreshMs: a recorded observation older than this is asked again.
const factsFreshMs = 5 * 60 * 1000

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
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	checks, err := s.doctorChecks(ctx, a, cluster, &net.Dialer{})
	if err != nil {
		return nil, err
	}
	graph, err := s.evidenceGraph(ctx, a, cluster, checks)
	if err != nil {
		return nil, err
	}
	report, err := doctorReport(checks, graph, evidence.Diagnose(graph))
	if err != nil {
		return nil, err
	}
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
		agent, err := s.agentState(ctx, a)
		if err != nil {
			return nil, err
		}
		checks = append(checks, doctor.AgentCheck(agent, agentStaleAfter(s.deps.Config.Agent.HeartbeatSecs)))
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
		host, err := dnsname.Canonical(name)
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
		zone := dnsname.LockKey(host)
		servers, lookupErr := s.deps.DNS.NS(ctx, zone)
		checks = append(checks,
			doctor.ClaimCheck(host, holder, s.deps.Config.Domains.RequireClaim),
			doctor.DelegationCheck(host, zone, servers, lookupErr),
			doctor.ProxyCheck(host, s.proxied(ctx, org, host)),
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

// proxied is whether a DNS provider account of org proxies host's record;
// none when no account holds it (or none answered). Accounts that cannot
// be opened are passed over.
func (s *Server) proxied(ctx context.Context, org ids.OrgID, host string) opt.Val[bool] {
	keyring, ok := s.deps.Keyring.Get()
	if !ok || keyring == nil {
		return opt.None[bool]()
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return opt.None[bool]()
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	providers, err := t.DNSProviders(ctx)
	if err != nil {
		return opt.None[bool]()
	}
	for _, p := range providers {
		kind, sealed, found, err := t.DNSProviderSecret(ctx, p.ID)
		if err != nil || !found {
			continue
		}
		token, err := notify.OpenSecret(keyring, org, p.ID, sealed)
		if err != nil || !utf8.Valid(token) {
			continue
		}
		api, ok := s.deps.DNS.Provider(kind, string(token))
		if !ok {
			continue
		}
		zone, found, err := api.ZoneFor(ctx, host)
		if err != nil || !found {
			continue
		}
		records, err := api.Records(ctx, zone, host)
		if err != nil || len(records) == 0 {
			continue
		}
		return opt.Some(slices.ContainsFunc(records, func(r dns.ProviderRecord) bool { return r.Proxied }))
	}
	return opt.None[bool]()
}

// agentStaleAfter is how long an agent sending a heartbeat every
// heartbeat seconds may stay silent, in seconds: three heartbeats, at least
// 30 (saturating where Rust's multiplication would overflow).
func agentStaleAfter(heartbeat uint64) uint64 {
	if heartbeat > math.MaxUint64/3 {
		return math.MaxUint64
	}
	return max(heartbeat*3, 30)
}

// agentState is what is known of the agent of the app's cluster (the
// primary one): none, revoked, or when it was last heard of.
func (s *Server) agentState(ctx context.Context, a appScope) (doctor.AgentState, error) {
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	// Rust dropped the tenant without committing: a cluster ensured here
	// is not kept.
	cluster, err := t.EnsureCluster(ctx, registry.Primary)
	_ = t.Rollback(ctx) //nolint:errcheck // nothing to keep
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	agent, found, err := s.deps.Store.ClusterAgent(ctx, cluster)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return doctor.AgentNone{}, nil
	case agent.RevokedAt.IsSome():
		return doctor.AgentRevoked{}, nil
	}
	age := opt.None[uint64]()
	if seen, ok := agent.LastSeenAt.Get(); ok {
		age = opt.Some(uint64(max(clock.SaturatingSub(s.deps.Clock.NowMs(), seen), 0) / 1000)) //nolint:gosec // not negative
	}
	return doctor.AgentSeen{Age: age}, nil
}

// doctorReport is the report of checks, with the evidence graph and its
// findings. Rust turned both into serde_json values, whose objects print
// their keys sorted: each member here is that canonical text.
func doctorReport(checks []doctor.Check, graph evidence.Graph, findings []evidence.Finding) (gen.DoctorReport, error) {
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
	members, err := rawMembers(graph)
	if err != nil {
		return gen.DoctorReport{}, err
	}
	items := make([]gen.DoctorReportFindingsItem, len(findings))
	for i, f := range findings {
		item, err := rawMembers(f)
		if err != nil {
			return gen.DoctorReport{}, err
		}
		items[i] = item
	}
	return gen.DoctorReport{
		Status:   string(doctor.Overall(checks)),
		Checks:   out,
		Graph:    members,
		Findings: items,
	}, nil
}

// rawMembers is the members of v's JSON object, each as its canonical
// text.
func rawMembers(v any) (map[string]jx.Raw, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, kerrors.Wrap(err, "encode the doctor's report")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, kerrors.Wrap(err, "encode the doctor's report")
	}
	out := make(map[string]jx.Raw, len(members))
	for key, member := range members {
		text, err := jsonx.Canonical(member)
		if err != nil {
			return nil, kerrors.Wrap(err, "encode the doctor's report")
		}
		out[key] = jx.Raw(text)
	}
	return out, nil
}

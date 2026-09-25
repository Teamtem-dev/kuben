package doctor

// The checks of a doctor report: the platform (GatewayClass, Gateway,
// issuer), the ports, the route and its certificates, the agent, and each
// custom domain's claim, delegation and proxy.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// Rank orders the verdicts from best to worst, as the Rust enum's Ord.
func (s Status) Rank() int {
	switch s {
	case StatusOK:
		return 0
	case StatusWarn:
		return 1
	case StatusUnknown:
		return 2
	case StatusFail:
		return 3
	}
	return 3
}

// Overall is the worst verdict of checks: `ok` for none. An unknown check
// outranks a warning, so a report with one is never `ok`.
func Overall(checks []Check) Status {
	worst := StatusOK
	for _, c := range checks {
		if c.Status.Rank() > worst.Rank() {
			worst = c.Status
		}
	}
	return worst
}

// availability judges a GatewayClass or an issuer found in the facts;
// known is whether the facts were read at all.
func availability(id, subject string, found discovery.Availability, known bool, ready string) Check {
	if !known {
		return newCheck(id, subject, StatusUnknown, "the cluster could not be asked yet")
	}
	switch a := found.(type) {
	case discovery.Unknown:
		return newCheck(id, subject, StatusUnknown, "the cluster could not be asked yet")
	case discovery.Ready:
		return newCheck(id, subject, StatusOK, ready)
	case discovery.NotReady:
		if a.Message == "" {
			return newCheck(id, subject, StatusFail, "exists but is not ready")
		}
		return newCheck(id, subject, StatusFail, a.Message)
	case discovery.Missing:
		return newCheck(id, subject, StatusFail, "does not exist")
	}
	return newCheck(id, subject, StatusUnknown, "the cluster could not be asked yet")
}

// PlatformChecks is the platform an app is exposed through: its
// GatewayClass, the Gateway, and the issuer of its certificates
// (platform_checks). Without facts every discovered dependency is unknown.
func PlatformChecks(facts opt.Val[discovery.ClusterFacts], platform render.Platform, gateway GatewayState) []Check {
	gw, ok := platform.Gateway.Get()
	if !ok {
		return []Check{
			newCheck("gateway", "", StatusFail, "no Gateway is configured: apps get no public address").
				WithHint("set KubenConfig spec.gatewayClassName (kuben setup does on its own k3s)"),
		}
	}
	f, known := facts.Get()
	var checks []Check
	if class, ok := platform.GatewayClass.Get(); ok {
		var found discovery.Availability = discovery.Unknown{}
		if known {
			found = f.GatewayClass(class)
		}
		checks = append(checks, availability("gateway-class", class, found, known, "accepted by its controller").
			WithHint("install the Gateway controller for this class (k3s: Traefik with the Gateway provider; elsewhere e.g. Envoy Gateway)"))
	}
	checks = append(checks, gatewayCheck(gw.Namespace+"/"+gw.Name, gateway))
	issuer, ok := platform.ClusterIssuer.Get()
	if !ok {
		return append(checks,
			newCheck("issuer", "", StatusWarn, "no ClusterIssuer: apps are served over plain HTTP").
				WithHint("kuben setup --acme-email you@example.com, or KubenConfig spec.clusterIssuer"))
	}
	var found discovery.Availability = discovery.Unknown{}
	if known {
		found = f.Issuer(issuer)
	}
	return append(checks, availability("issuer", issuer, found, known, "ready").
		WithHint("kubectl describe clusterissuer shows why (an ACME account, a CA Secret)"))
}

// gatewayCheck judges Kuben's Gateway, named name.
func gatewayCheck(name string, gateway GatewayState) Check {
	switch g := gateway.(type) {
	case GatewayUnreadable:
		return newCheck("gateway", name, StatusUnknown, "could not be read")
	case GatewayMissing:
		return newCheck("gateway", name, StatusFail, "does not exist").
			WithHint("Kuben creates it once a GatewayClass is set; see the Gateway condition of KubenConfig")
	case GatewayFound:
		programmed, answered := g.Programmed.Get()
		switch {
		case !answered:
			return newCheck("gateway", name, StatusUnknown, "no controller has answered for it yet")
		case programmed:
			at := "no address reported"
			if len(g.Addrs) > 0 {
				at = join(g.Addrs)
			}
			return newCheck("gateway", name, StatusOK, "programmed ("+at+")")
		default:
			return newCheck("gateway", name, StatusFail, g.Message.Or("not programmed")).
				WithHint("the Gateway controller's log says why")
		}
	}
	return newCheck("gateway", name, StatusUnknown, "could not be read")
}

// PortProbe is whether the Gateway answered on a port, from this server.
//
//sumtype:decl
type PortProbe interface{ isPortProbe() }

// PortNoAddress is a Gateway that reports no address to try.
type PortNoAddress struct{}

// PortReached is a Gateway that accepted a connection.
type PortReached struct{}

// PortUnreached is a connection that failed; Reason says how.
type PortUnreached struct{ Reason string }

func (PortNoAddress) isPortProbe() {}
func (PortReached) isPortProbe()   {}
func (PortUnreached) isPortProbe() {}

// PortCheck is whether the Gateway answered on the public port port
// (port_check): `port-80` for 80, `port-443` for any other.
func PortCheck(port uint16, reached PortProbe) Check {
	id := "port-443"
	if port == 80 {
		id = "port-80"
	}
	subject := strconv.FormatUint(uint64(port), 10)
	switch r := reached.(type) {
	case PortNoAddress:
		return newCheck(id, subject, StatusUnknown, "the Gateway reports no address to try")
	case PortReached:
		return newCheck(id, subject, StatusOK, "the Gateway accepts connections")
	case PortUnreached:
		return newCheck(id, subject, StatusFail, r.Reason).
			WithHint("another web server may hold the port, or a firewall blocks it (a cloud firewall too)")
	}
	return newCheck(id, subject, StatusUnknown, "the Gateway reports no address to try")
}

// Dialer opens TCP connections: a net.Dialer in production.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// probeTimeout bounds one connection attempt.
const probeTimeout = 3 * time.Second

// ProbePort is whether the first of addresses accepts TCP connections on
// port within three seconds (probe_port). A failure names the address as
// Rust's `{ip}:{port}` did (an IPv6 address without brackets).
func ProbePort(ctx context.Context, d Dialer, addresses []netip.Addr, port uint16) PortProbe {
	if len(addresses) == 0 {
		return PortNoAddress{}
	}
	ip := addresses[0]
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, port).String())
	at := ip.String() + ":" + strconv.FormatUint(uint64(port), 10)
	switch {
	case err == nil:
		_ = conn.Close() //nolint:errcheck // only the connect mattered
		return PortReached{}
	case ctx.Err() != nil:
		return PortUnreached{Reason: at + ": no answer within 3s"}
	default:
		return PortUnreached{Reason: at + ": " + dialReason(err)}
	}
}

// dialReason is the system's reason of a failed connect ("connection
// refused"), without Go's `dial tcp …: connect:` prefix.
func dialReason(err error) string {
	var sys *os.SyscallError
	if errors.As(err, &sys) {
		return sys.Err.Error()
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

// ExposureChecks is the app's route and the certificate of each host
// (exposure_checks); hasHosts is whether the app has a public hostname.
func ExposureChecks(exposure opt.Val[projection.ExposureView], hasHosts bool) []Check {
	e, ok := exposure.Get()
	if !ok {
		if hasHosts {
			return []Check{newCheck("route", "", StatusFail, "the app has no route yet").
				WithHint("the App's Exposed condition says why (no Gateway, no base domain)")}
		}
		return []Check{newCheck("route", "", StatusWarn, "the app has no public address").
			WithHint("add a domain, or set a base domain for generated ones")}
	}
	checks := make([]Check, 0, 1+len(e.Hosts))
	accepted, answered := e.Accepted.Get()
	switch {
	case !answered:
		checks = append(checks, newCheck("route", "", StatusUnknown, "the Gateway has not answered yet"))
	case accepted:
		checks = append(checks, newCheck("route", "", StatusOK, "accepted by the Gateway"))
	default:
		checks = append(checks, newCheck("route", "", StatusFail, e.Message.Or("refused by the Gateway")).
			WithHint("a host another environment holds, or a listener the Gateway lacks"))
	}
	for _, host := range e.Hosts {
		checks = append(checks, certificateCheck(host))
	}
	return checks
}

// certificateCheck is the certificate of one host of a route.
func certificateCheck(host projection.HostExposure) Check {
	check := func(status Status, detail string) Check {
		return newCheck("certificate", host.Host, status, detail)
	}
	switch host.TLS {
	case "none":
		return check(StatusWarn, "served over plain HTTP").
			WithHint("set a ClusterIssuer, or tls: auto on the domain")
	case "secret":
		return check(StatusUnknown, "uses a certificate Secret of its own (not checked)")
	}
	ready, known := host.CertificateReady.Get()
	switch {
	case !known:
		return check(StatusUnknown, "no certificate of its own was found")
	case ready:
		return check(StatusOK, "issued")
	default:
		return check(StatusFail, host.CertificateMessage.Or("not issued yet")).
			WithHint("HTTP-01 needs DNS pointing at the Gateway and port 80 open; the certificate's events say more")
	}
}

// AgentState is what is known of the agent that delivers an app.
//
//sumtype:decl
type AgentState interface{ isAgentState() }

// AgentNone is a cluster without an enrolled agent.
type AgentNone struct{}

// AgentRevoked is a cluster whose agent was revoked.
type AgentRevoked struct{}

// AgentSeen is an enrolled agent; Age is the seconds since it was last
// heard of, if ever.
type AgentSeen struct{ Age opt.Val[uint64] }

func (AgentNone) isAgentState()    {}
func (AgentRevoked) isAgentState() {}
func (AgentSeen) isAgentState()    {}

// AgentCheck is the agent of an app its cluster's agent delivers
// (agent_check); staleAfter is a few heartbeats, in seconds.
func AgentCheck(agent AgentState, staleAfter uint64) Check {
	check := func(status Status, detail string) Check { return newCheck("agent", "", status, detail) }
	switch a := agent.(type) {
	case AgentNone:
		return check(StatusFail, "the cluster has no enrolled agent").
			WithHint("kuben agent-token --cluster <cluster>, then start the agent with it")
	case AgentRevoked:
		return check(StatusFail, "the cluster's agent was revoked").
			WithHint("enroll a new agent with a fresh token")
	case AgentSeen:
		age, ok := a.Age.Get()
		switch {
		case !ok:
			return check(StatusFail, "the agent enrolled but never linked").
				WithHint("check that the agent reaches Kuben's AgentLink port")
		case age <= staleAfter:
			return check(StatusOK, fmt.Sprintf("linked, heard from %ds ago", age))
		default:
			return check(StatusFail, fmt.Sprintf("last heard from %ds ago", age)).
				WithHint("the agent's log says why it lost its link")
		}
	}
	return check(StatusFail, "the cluster has no enrolled agent")
}

// ClaimOwner is who holds the verified claim covering a host (M5.2).
//
//sumtype:decl
type ClaimOwner interface{ isClaimOwner() }

// ClaimOurs is this organization, through its claim on Domain.
type ClaimOurs struct{ Domain string }

// ClaimOthers is another organization.
type ClaimOthers struct{}

// ClaimNobody is a domain no organization verified.
type ClaimNobody struct{}

func (ClaimOurs) isClaimOwner()   {}
func (ClaimOthers) isClaimOwner() {}
func (ClaimNobody) isClaimOwner() {}

// ClaimCheck is whether the custom domain host is claimed (claim_check);
// with required, an unclaimed one fails.
func ClaimCheck(host string, owner ClaimOwner, required bool) Check {
	switch o := owner.(type) {
	case ClaimOurs:
		return newCheck("claim", host, StatusOK,
			"verified for this organization through its claim on "+o.Domain)
	case ClaimOthers:
		return newCheck("claim", host, StatusFail,
			"another organization verified this domain; the app cannot serve it").
			WithHint("Use a domain your organization owns.")
	case ClaimNobody:
	}
	status := StatusWarn
	if required {
		status = StatusFail
	}
	return newCheck("claim", host, status, "no organization verified this domain").
		WithHint("Claim the domain (Domains) and verify it with its TXT record or a DNS provider.")
}

// DelegationCheck is how zone is delegated: nameservers, as the public DNS
// answers, or lookupErr when they could not be read (delegation_check).
func DelegationCheck(host, zone string, nameservers []string, lookupErr error) Check {
	switch {
	case lookupErr != nil:
		return newCheck("delegation", host, StatusUnknown,
			fmt.Sprintf("the name servers of %s could not be read: %s", zone, lookupErr.Error()))
	case len(nameservers) == 0:
		return newCheck("delegation", host, StatusFail, zone+" has no name servers in the public DNS").
			WithHint("Register the domain and delegate it to a DNS provider.")
	}
	cloudflare := ""
	if allCloudflare(nameservers) {
		cloudflare = " (Cloudflare)"
	}
	return newCheck("delegation", host, StatusOK,
		fmt.Sprintf("%s is served by %s%s", zone, strings.Join(nameservers, ", "), cloudflare))
}

func allCloudflare(servers []string) bool {
	for _, s := range servers {
		if !strings.HasSuffix(s, ".ns.cloudflare.com") {
			return false
		}
	}
	return true
}

// ProxyCheck is whether a DNS provider proxies host's record; a proxy hides
// the Gateway, so HTTP-01 challenges and passthrough TLS may fail
// (proxy_check). None: no provider account holds the record.
func ProxyCheck(host string, proxied opt.Val[bool]) Check {
	p, ok := proxied.Get()
	switch {
	case !ok:
		return newCheck("proxy", host, StatusUnknown, "no DNS provider account holds this record")
	case !p:
		return newCheck("proxy", host, StatusOK, "the record points straight at its target")
	default:
		return newCheck("proxy", host, StatusWarn,
			"the provider proxies this record: visitors reach its edge, not the Gateway").
			WithHint("Use DNS-01 certificates (kuben dns01-issuer) and full TLS at the proxy, or turn the proxy off.")
	}
}

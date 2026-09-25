// Package doctor judges why an app is or is not reachable, as a list of
// checks (M2.13). It replaces crates/kuben-platform/src/doctor.rs: the
// platform (GatewayClass, Gateway, issuer), port, route and certificate,
// DNS, agent, claim, delegation and proxy checks, the report's overall
// verdict, the lookup of a hostname, the probe of a port, and the reading
// of the KubenConfig and of Kuben's Gateway.
//
// The functions only judge what the caller observed, so the API and the CLI
// give the same verdicts. A check that could not be made is `unknown`, and
// a report with an unknown check is never `ok`.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// Status is the verdict of one check. Its wire form is the lowercase name.
type Status string

// The verdicts of a check, from best to worst for a report.
const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	// StatusUnknown could not be checked; it never counts as fine.
	StatusUnknown Status = "unknown"
	StatusFail    Status = "fail"
)

// Check is one verdict of the doctor.
type Check struct {
	// ID is stable: `gateway-class`, `gateway`, `issuer`, `port-80`,
	// `route`, `certificate`, `dns`, `agent`.
	ID string `json:"id"`
	// Subject is what was checked, e.g. the host.
	Subject string `json:"subject"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
	// Hint is what to do about it; never given for a check that is ok.
	Hint opt.Val[string] `json:"hint,omitzero"`
}

// newCheck is Check::new: a check without a hint.
func newCheck(id, subject string, status Status, detail string) Check {
	return Check{ID: id, Subject: subject, Status: status, Detail: detail}
}

// WithHint is c with hint, unless c is ok (Check::hint).
func (c Check) WithHint(hint string) Check {
	if c.Status != StatusOK {
		c.Hint = opt.Some(hint)
	}
	return c
}

// Verdict is what a hostname resolves to against the Gateway's addresses.
type Verdict string

// The DNS verdicts, with their wire strings.
const (
	VerdictOK         Verdict = "ok"
	VerdictMismatch   Verdict = "mismatch"
	VerdictUnresolved Verdict = "unresolved"
	VerdictUnknown    Verdict = "unknown"
)

func join(ips []netip.Addr) string {
	parts := make([]string, len(ips))
	for i, ip := range ips {
		parts[i] = ip.String()
	}
	return strings.Join(parts, ", ")
}

// DNSVerdict is what resolved says against the Gateway's addresses
// gateway, and why (dns_verdict).
func DNSVerdict(resolved, gateway []netip.Addr) (Verdict, string) {
	if len(resolved) == 0 {
		return VerdictUnresolved, "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway"
	}
	if len(gateway) == 0 {
		return VerdictUnknown, fmt.Sprintf("resolves to %s; the gateway reports no address to compare with", join(resolved))
	}
	for _, ip := range resolved {
		if slices.Contains(gateway, ip) {
			return VerdictOK, "points at the gateway"
		}
	}
	return VerdictMismatch, fmt.Sprintf("points at %s but the gateway is %s", join(resolved), join(gateway))
}

// DNSCheck is host's DNS against the Gateway's addresses (dns_check).
func DNSCheck(host string, resolved, gateway []netip.Addr) Check {
	verdict, detail := DNSVerdict(resolved, gateway)
	var status Status
	switch verdict {
	case VerdictOK:
		status = StatusOK
	case VerdictUnknown:
		status = StatusUnknown
	case VerdictMismatch, VerdictUnresolved:
		status = StatusFail
	}
	return newCheck("dns", host, status, detail).
		WithHint("point the record at the Gateway's address; DNS may take a while to follow")
}

// Resolver looks hostnames up: net.DefaultResolver in production.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// lookupTimeout bounds one lookup; after it the host has no address.
const lookupTimeout = 3 * time.Second

// Resolve is the addresses host resolves to, sorted (IPv4 before IPv6,
// numerically) and without duplicates; none after three seconds or on an
// error (resolve).
func Resolve(ctx context.Context, r Resolver, host string) []netip.Addr {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	found, err := r.LookupHost(ctx, host)
	if err != nil || ctx.Err() != nil {
		return []netip.Addr{}
	}
	ips := make([]netip.Addr, 0, len(found))
	for _, text := range found {
		ip, err := netip.ParseAddr(text)
		if err != nil {
			continue
		}
		// A socket address's IP carries no scope, as in Rust.
		ips = append(ips, ip.WithZone(""))
	}
	slices.SortFunc(ips, netip.Addr.Compare)
	return slices.Compact(ips)
}

// DecodeKubenConfig is the KubenConfig obj holds.
func DecodeKubenConfig(obj *unstructured.Unstructured) (v1alpha1.KubenConfig, error) {
	var c v1alpha1.KubenConfig
	data, err := obj.MarshalJSON()
	if err != nil {
		return c, fmt.Errorf("encoding a KubenConfig: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("decoding a KubenConfig: %w", err)
	}
	return c, nil
}

// ReadPlatform is the platform settings of the cluster's KubenConfig; the
// defaults without one, or when it cannot be read (read_platform).
func ReadPlatform(ctx context.Context, client dynamic.Interface) render.Platform {
	gvr := v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.KubenConfigResource)
	obj, err := client.Resource(gvr).Get(ctx, controller.KubenConfigName, metav1.GetOptions{})
	if err != nil || obj == nil {
		return render.DefaultPlatform()
	}
	config, err := DecodeKubenConfig(obj)
	if err != nil {
		return render.DefaultPlatform()
	}
	return render.PlatformFromSpec(config.Spec)
}

// GatewayState is what was read of Kuben's Gateway.
//
//sumtype:decl
type GatewayState interface {
	// Addresses is the Gateway's addresses, when it was found.
	Addresses() []netip.Addr
	isGatewayState()
}

// GatewayUnreadable is a Gateway that could not be read.
type GatewayUnreadable struct{}

// GatewayMissing is a Gateway that does not exist, or none configured.
type GatewayMissing struct{}

// GatewayFound is the Gateway as its controller reports it.
type GatewayFound struct {
	// Programmed is its `Programmed` condition, once its controller answered.
	Programmed opt.Val[bool]
	Message    opt.Val[string]
	// Addrs is its status addresses in their order, a hostname
	// replaced by what it resolves to.
	Addrs []netip.Addr
}

func (GatewayUnreadable) isGatewayState() {}
func (GatewayMissing) isGatewayState()    {}
func (GatewayFound) isGatewayState()      {}

// Addresses is none: the Gateway could not be read.
func (GatewayUnreadable) Addresses() []netip.Addr { return []netip.Addr{} }

// Addresses is none: there is no Gateway.
func (GatewayMissing) Addresses() []netip.Addr { return []netip.Addr{} }

// Addresses is the Gateway's addresses.
func (g GatewayFound) Addresses() []netip.Addr { return g.Addrs }

// gatewayResource is the Gateway API's Gateway.
func gatewayResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
}

// ReadGateway is the Gateway apps attach to, as its controller reports it
// (read_gateway). Its addresses keep the order and the duplicates of
// `status.addresses`; a hostname among them is resolved with r.
func ReadGateway(ctx context.Context, client dynamic.Interface, platform render.Platform, r Resolver) GatewayState {
	gw, ok := platform.Gateway.Get()
	if !ok {
		return GatewayMissing{}
	}
	obj, err := client.Resource(gatewayResource()).Namespace(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return GatewayMissing{}
	case err != nil || obj == nil:
		return GatewayUnreadable{}
	}
	found := GatewayFound{Addrs: []netip.Addr{}}
	if cond, ok := discovery.Condition(obj.Object, "Programmed"); ok {
		found.Programmed = opt.Some(cond.True)
		found.Message = cond.Message
	}
	// Anything but a list of objects with a string value has no address.
	addresses, _, err := unstructured.NestedSlice(obj.Object, "status", "addresses")
	if err != nil {
		return found
	}
	for _, a := range addresses {
		entry, ok := a.(map[string]any)
		if !ok {
			continue
		}
		value, ok := entry["value"].(string)
		if !ok {
			continue
		}
		if ip, err := parseIP(value); err == nil {
			found.Addrs = append(found.Addrs, ip)
		} else {
			found.Addrs = append(found.Addrs, Resolve(ctx, r, value)...)
		}
	}
	return found
}

// parseIP is Rust's `str::parse::<IpAddr>`: a zone is not an address.
func parseIP(text string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(text)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parsing an address: %w", err)
	}
	if ip.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%q has a zone", text)
	}
	return ip, nil
}

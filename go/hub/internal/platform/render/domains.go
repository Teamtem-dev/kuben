package render

import (
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Gateway listener and certificate names (controller::gateway). They are
// derived from a hash that must never change between releases: listeners
// and certificate Secrets already exist under these names.
const (
	// HTTPListener is the plain HTTP listener of Kuben's Gateway.
	HTTPListener = "http"
	// WildcardListener is the `*.<baseDomain>` HTTPS listener.
	WildcardListener = "https"
	// TLSSecretPrefix prefixes the certificate Secrets (and cert-manager
	// Certificates) Kuben's gateway orders.
	TLSSecretPrefix = "kuben-tls-" //nolint:gosec // a name prefix, not a credential
)

// fnv1a is 64-bit FNV-1a: stable forever, unlike a seeded hash.
func fnv1a(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 0x00000100000001b3
	}
	return h
}

func hostHash(host string) string {
	return fmt.Sprintf("%012x", fnv1a(host)>>16)
}

// HostListenerName is the HTTPS listener of one hostname: `h-<hash>`.
func HostListenerName(host string) string { return "h-" + hostHash(host) }

// HostSecretName is the certificate Secret of one hostname.
func HostSecretName(host string) string { return TLSSecretPrefix + hostHash(host) }

// PlainListenerName is the plain-HTTP listener of a `tls: none` host.
func PlainListenerName(host string) string { return "p-" + hostHash(host) }

// coveredByWildcard: `*.base` matches exactly one additional DNS label.
func coveredByWildcard(host string, p Platform) bool {
	base, ok := p.BaseDomain.Get()
	if !ok || p.WildcardTLSSecret.IsNone() {
		return false
	}
	rest, ok := strings.CutSuffix(host, base)
	if !ok {
		return false
	}
	label, ok := strings.CutSuffix(rest, ".")
	return ok && label != "" && !strings.Contains(label, ".")
}

// SectionForHost is the listener (`sectionName`) an app route must attach
// to for host.
func SectionForHost(host string, p Platform) string {
	if coveredByWildcard(host, p) {
		return WildcardListener
	}
	return HostListenerName(host)
}

// SectionFor is the listener an app route attaches to for claim while TLS
// is on: its own certificate always gets a listener of its own.
func SectionFor(claim DomainClaim, p Platform) string {
	mode := claim.Mode()
	switch mode.Kind {
	case TLSAuto:
		return SectionForHost(claim.Host, p)
	case TLSSecret:
		return HostListenerName(claim.Host)
	case TLSPlain:
		return PlainListenerName(claim.Host)
	}
	return HostListenerName(claim.Host)
}

// TLSKind is how one hostname is served.
type TLSKind string

// The TLS kinds.
const (
	// TLSAuto is a certificate from the configured ClusterIssuer.
	TLSAuto TLSKind = "auto"
	// TLSPlain is plain HTTP only (`tls: none`).
	TLSPlain TLSKind = "none"
	// TLSSecret is the app's own certificate, in a Secret of its namespace.
	TLSSecret TLSKind = "secret"
)

// TLSMode is a TLSKind and, for TLSSecret, the Secret's name.
type TLSMode struct {
	Kind   TLSKind
	Secret string
}

// ParseTLSMode reads `auto` (or empty), `none`, or a Secret name.
func ParseTLSMode(tls string) (TLSMode, bool) {
	switch {
	case tls == "" || tls == "auto":
		return TLSMode{Kind: TLSAuto}, true
	case tls == "none":
		return TLSMode{Kind: TLSPlain}, true
	case isDNSSubdomain(tls):
		return TLSMode{Kind: TLSSecret, Secret: tls}, true
	}
	return TLSMode{}, false
}

// isDNSSubdomain: a DNS-1123 subdomain, as Kubernetes object names are.
func isDNSSubdomain(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for part := range strings.SplitSeq(name, ".") {
		if part == "" || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return false
		}
		for i := range len(part) {
			b := part[i]
			if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
				return false
			}
		}
	}
	return true
}

// DomainClaim is a hostname of an app and its `tls` setting, as the route
// annotation (DomainsAnnotation) carries them.
type DomainClaim struct {
	Host string `json:"host"`
	TLS  string `json:"tls"`
}

// Mode is the TLS mode; an unreadable setting counts as `auto` (validation
// refuses it before anything is built).
func (c DomainClaim) Mode() TLSMode {
	if m, ok := ParseTLSMode(c.TLS); ok {
		return m
	}
	return TLSMode{Kind: TLSAuto}
}

// NamespaceName is the namespace of an environment: `kb-<environment>`.
func NamespaceName(environment string) string { return "kb-" + environment }

// HostnamesFor is explicit domains first (lowercased), then
// `<app>-<environment>.<base_domain>`.
func HostnamesFor(app string, environment opt.Val[string], domains []string, p Platform) []string {
	hosts := make([]string, 0, len(domains)+1)
	for _, d := range domains {
		hosts = append(hosts, ascii.Lower(d))
	}
	base, okBase := p.BaseDomain.Get()
	env, okEnv := environment.Get()
	if okBase && okEnv {
		generated := app + "-" + env + "." + base
		if !contains(hosts, generated) {
			hosts = append(hosts, generated)
		}
	}
	return hosts
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func labelOf(app *v1alpha1.App, key string) opt.Val[string] {
	v, ok := app.Labels[key]
	if !ok {
		return opt.None[string]()
	}
	return opt.Some(v)
}

// Hostnames are the hostnames of app on p.
func Hostnames(app *v1alpha1.App, p Platform) []string {
	domains := make([]string, 0, len(app.Spec.Domains))
	for _, d := range app.Spec.Domains {
		domains = append(domains, d.Host)
	}
	return HostnamesFor(app.Name, labelOf(app, v1alpha1.LabelEnvironment), domains, p)
}

// DomainClaims are the hostnames with their TLS setting: explicit domains
// first, then the generated one (always `auto`).
func DomainClaims(app *v1alpha1.App, p Platform) []DomainClaim {
	explicit := make(map[string]string, len(app.Spec.Domains))
	for _, d := range app.Spec.Domains {
		explicit[ascii.Lower(d.Host)] = d.TLS
	}
	hosts := Hostnames(app, p)
	claims := make([]DomainClaim, 0, len(hosts))
	for _, host := range hosts {
		tls, ok := explicit[host]
		if !ok || tls == "" {
			tls = "auto"
		}
		claims = append(claims, DomainClaim{Host: host, TLS: tls})
	}
	return claims
}

// Secured reports whether claim is served over HTTPS on p.
func Secured(claim DomainClaim, p Platform) bool {
	return p.TLS && claim.Mode().Kind != TLSPlain
}

// URL is the public URL shown in the UI: the first hostname, with the
// scheme it is served with.
func URL(app *v1alpha1.App, p Platform) opt.Val[string] {
	claims := DomainClaims(app, p)
	if len(claims) == 0 {
		return opt.None[string]()
	}
	scheme := "http"
	if Secured(claims[0], p) {
		scheme = "https"
	}
	return opt.Some(scheme + "://" + claims[0].Host)
}

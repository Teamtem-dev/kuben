package config

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"

	"github.com/Teamtem-dev/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/sso"
)

// SecurityCfg is `[security]`: sessions, password hashing and sign-in limits.
type SecurityCfg struct {
	SessionTTLHours     uint64 `koanf:"session_ttl_hours"`
	SessionCacheTTLSecs uint64 `koanf:"session_cache_ttl_secs"`
	Argon2MKib          uint32 `koanf:"argon2_m_kib"`
	Argon2T             uint32 `koanf:"argon2_t"`
	Argon2P             uint32 `koanf:"argon2_p"`
	LoginConcurrency    uint64 `koanf:"login_concurrency"`
	// CookieSecure is `Secure` on the session cookie: `true`, `false`, or
	// `"auto"` (the default), which follows `server.public_url`. See
	// [CookieSecure]. A nil value (a SecurityCfg made without
	// [DefaultSecurityCfg]) counts as auto.
	CookieSecure CookieSecure `koanf:"cookie_secure"`
	// LoginMaxFailures is the failed logins allowed per (email, client IP)
	// within LoginWindowSecs.
	LoginMaxFailures uint32 `koanf:"login_max_failures"`
	// LoginMaxFailuresPerIP is the failed logins allowed per client IP,
	// across all accounts.
	LoginMaxFailuresPerIP uint32 `koanf:"login_max_failures_per_ip"`
	// LoginMaxFailuresPerAccount is the failed logins allowed per account,
	// across all IPs (high on purpose, so victims cannot be locked out
	// cheaply).
	LoginMaxFailuresPerAccount uint32 `koanf:"login_max_failures_per_account"`
	LoginWindowSecs            uint64 `koanf:"login_window_secs"`
	// TrustForwardedFor takes the client IP from the **last**
	// `X-Forwarded-For` hop. Enable only behind a proxy that appends it (the
	// Helm chart does); otherwise the TCP peer address is used.
	TrustForwardedFor bool `koanf:"trust_forwarded_for"`
	// TrustedProxies, with TrustForwardedFor: the proxies (CIDRs, e.g. the
	// pod network `10.42.0.0/16`) whose `X-Forwarded-*` headers are
	// believed. Empty: every peer's, for a proxy that is the only way in
	// (the Helm chart).
	TrustedProxies []string `koanf:"trusted_proxies"`
	// InsecureSetup lets the first admin be created over plain HTTP from
	// another machine. Off (the default): only over HTTPS (a trusted proxy's
	// `X-Forwarded-Proto`), from this machine (an SSH tunnel), or when the
	// console listens on loopback only (ADR-031).
	InsecureSetup     bool   `koanf:"insecure_setup"`
	PasswordMinLength uint64 `koanf:"password_min_length"`
}

// DefaultSecurityCfg is `[security]` when nothing is configured.
func DefaultSecurityCfg() SecurityCfg {
	return SecurityCfg{
		SessionTTLHours:            12,
		SessionCacheTTLSecs:        5,
		Argon2MKib:                 19 * 1024,
		Argon2T:                    2,
		Argon2P:                    1,
		LoginConcurrency:           2,
		CookieSecure:               CookieAuto{},
		LoginMaxFailures:           5,
		LoginMaxFailuresPerIP:      30,
		LoginMaxFailuresPerAccount: 100,
		LoginWindowSecs:            900,
		TrustedProxies:             []string{},
		PasswordMinLength:          12,
	}
}

// TrustsForwarded reports whether the `X-Forwarded-*` headers of a request
// from peer are believed.
func (s SecurityCfg) TrustsForwarded(peer opt.Val[netip.Addr]) bool {
	if !s.TrustForwardedFor {
		return false
	}
	if len(s.TrustedProxies) == 0 {
		return true
	}
	ip, known := peer.Get()
	return known && slices.ContainsFunc(s.TrustedProxies, func(cidr string) bool { return CIDRContains(cidr, ip) })
}

// CIDRContains reports whether ip lies in cidr (`10.42.0.0/16`, `fd00::/8`,
// or one address). Address families never mix, and a network that does not
// parse contains nothing.
func CIDRContains(cidr string, ip netip.Addr) bool {
	network, bits, _ := strings.Cut(cidr, "/")
	addr, err := netip.ParseAddr(strings.TrimSpace(network))
	if err != nil || addr.Zone() != "" || !ip.IsValid() {
		return false
	}
	// Prefix bits that are not a number count as the whole address.
	length := addr.BitLen()
	if n, err := strconv.ParseUint(strings.TrimPrefix(bits, "+"), 10, 32); err == nil {
		length = min(length, int(min(n, 128)))
	}
	prefix, err := addr.Prefix(length)
	return err == nil && prefix.Contains(ip.WithZone(""))
}

// SsoCfg is `[sso]`: single sign-on with an OpenID Connect provider (M4.3).
type SsoCfg struct {
	Enabled bool `koanf:"enabled"`
	// Issuer is the provider's issuer URL; its discovery document names the
	// rest.
	Issuer   opt.Val[string] `koanf:"issuer,omitempty"`
	ClientID opt.Val[string] `koanf:"client_id,omitempty"`
	// ClientSecretFile is a file holding the client secret (preferred);
	// ClientSecret is the secret itself.
	ClientSecretFile opt.Val[string] `koanf:"client_secret_file,omitempty"`
	ClientSecret     Secret          `koanf:"client_secret"`
	// DisplayName is shown on the sign-in button.
	DisplayName string   `koanf:"display_name"`
	Scopes      []string `koanf:"scopes"`
	// GroupClaim is the ID token claim that lists the person's groups.
	GroupClaim string `koanf:"group_claim"`
	// Groups maps a provider group to an organization role (`viewer` …
	// `owner`).
	Groups map[string]string `koanf:"groups"`
	// DefaultRole is the role of people in no mapped group; unset refuses
	// them.
	DefaultRole opt.Val[string] `koanf:"default_role,omitempty"`
	// AllowedDomains are the email domains allowed; empty for any.
	AllowedDomains       []string `koanf:"allowed_domains"`
	RequireVerifiedEmail bool     `koanf:"require_verified_email"`
	// Org is the organization people join; default `bootstrap.org_slug`.
	Org opt.Val[string] `koanf:"org,omitempty"`
	// DisablePasswordForLinked: accounts linked to the provider cannot sign
	// in with a password. Off by default so a local owner keeps a way in
	// while the provider is down.
	DisablePasswordForLinked bool `koanf:"disable_password_for_linked"`
}

// DefaultSsoCfg is `[sso]` when nothing is configured.
func DefaultSsoCfg() SsoCfg {
	return SsoCfg{
		DisplayName:          "Single sign-on",
		Scopes:               []string{"openid", "email", "profile"},
		GroupClaim:           "groups",
		Groups:               map[string]string{},
		AllowedDomains:       []string{},
		RequireVerifiedEmail: true,
	}
}

// Policy is who may sign in and as what, from this configuration. It fails
// on a role name that is not a role.
func (s SsoCfg) Policy() (sso.Policy, error) {
	groups := make(map[string]perm.Role, len(s.Groups))
	for group, name := range s.Groups {
		role, err := perm.ParseRole(name)
		if err != nil {
			return sso.Policy{}, err
		}
		groups[group] = role
	}
	defaultRole := opt.None[perm.Role]()
	if name, ok := s.DefaultRole.Get(); ok {
		role, err := perm.ParseRole(name)
		if err != nil {
			return sso.Policy{}, err
		}
		defaultRole = opt.Some(role)
	}
	domains := make([]string, 0, len(s.AllowedDomains))
	for _, d := range s.AllowedDomains {
		domains = append(domains, ascii.Lower(strings.TrimLeft(strings.TrimSpace(d), "@")))
	}
	return sso.Policy{
		Groups:               groups,
		DefaultRole:          defaultRole,
		AllowedDomains:       domains,
		RequireVerifiedEmail: s.RequireVerifiedEmail,
	}, nil
}

// QuotaCfg is `[quota]`: what every organization of the installation may
// request at most (M4.5). Set by the operator; nobody raises it from the
// console. Unset is unlimited.
type QuotaCfg struct {
	// OrgCPU: CPU requests of all apps of an organization at their peak,
	// e.g. `32`.
	OrgCPU opt.Val[string] `koanf:"org_cpu,omitempty"`
	// OrgMemory: memory requests of all apps of an organization at their
	// peak, e.g. `64Gi`.
	OrgMemory opt.Val[string] `koanf:"org_memory,omitempty"`
	// OrgPods: pods of all apps of an organization at their peak.
	OrgPods opt.Val[uint64] `koanf:"org_pods,omitempty"`
	// OrgApps: live apps (application targets) per organization.
	OrgApps opt.Val[uint64] `koanf:"org_apps,omitempty"`
	// OrgEnvironments: live environments per organization.
	OrgEnvironments opt.Val[uint64] `koanf:"org_environments,omitempty"`
}

// OrgLimits are the organization limits, or why a quantity is not one.
func (q QuotaCfg) OrgLimits() (capacity.Limits, error) {
	parse := func(name string, value opt.Val[string], read func(string) (uint64, bool)) (opt.Val[uint64], error) {
		text, set := value.Get()
		if !set {
			return opt.None[uint64](), nil
		}
		n, ok := read(text)
		if !ok {
			return opt.None[uint64](), kerr.New(kerr.Validation, "quota.%s `%s` is not a quantity", name, text)
		}
		return opt.Some(n), nil
	}
	cpu, err := parse("org_cpu", q.OrgCPU, capacity.CPUMillis)
	if err != nil {
		return capacity.Limits{}, err
	}
	memory, err := parse("org_memory", q.OrgMemory, capacity.Bytes)
	if err != nil {
		return capacity.Limits{}, err
	}
	return capacity.Limits{CPUMillis: cpu, MemoryBytes: memory, Pods: q.OrgPods}, nil
}

// BuildCfg is `[build]`: isolated builds (ADR-028), one rootless BuildKit
// Job per attempt.
type BuildCfg struct {
	// Enabled runs the build worker.
	Enabled bool `koanf:"enabled"`
	// Namespace of build Jobs; default: Kuben's own, else `kuben-builds`.
	Namespace opt.Val[string] `koanf:"namespace,omitempty"`
	// BuildkitImage is the rootless BuildKit image. The defaults are pinned
	// by tag; production pins every build image by digest
	// (`image@sha256:…`), and the build worker warns at start about any
	// image that is not.
	BuildkitImage string `koanf:"buildkit_image"`
	// FetchImage is the image with `git` that fetches the source.
	FetchImage string `koanf:"fetch_image"`
	// RailpackFrontend is Railpack's BuildKit frontend image; with
	// RailpackImage, enables Railpack builds. Unset, only Dockerfile builds
	// run.
	RailpackFrontend opt.Val[string] `koanf:"railpack_frontend,omitempty"`
	// RailpackImage is the image with `sh` and the `railpack` CLI that
	// writes the build plan; unset, only Dockerfile builds run.
	RailpackImage opt.Val[string] `koanf:"railpack_image,omitempty"`
	CPURequest    string          `koanf:"cpu_request"`
	CPULimit      string          `koanf:"cpu_limit"`
	// Memory request and limit are equal (ADR-028).
	Memory           string `koanf:"memory"`
	EphemeralStorage string `koanf:"ephemeral_storage"`
	// DeadlineSecs is the hard deadline of one attempt.
	DeadlineSecs uint64 `koanf:"deadline_secs"`
	// MaxConcurrent is builds at once, over all organizations; 0 disables
	// building.
	MaxConcurrent       uint32 `koanf:"max_concurrent"`
	MaxConcurrentPerOrg uint32 `koanf:"max_concurrent_per_org"`
	// PushSecret is a `kubernetes.io/dockerconfigjson` Secret in the build
	// namespace with push access to the image repositories; unset for an
	// open registry.
	PushSecret opt.Val[string] `koanf:"push_secret,omitempty"`
	// InsecureRegistry pushes over plain HTTP (an in-cluster registry
	// without TLS).
	InsecureRegistry bool `koanf:"insecure_registry"`
	// RegistryAuthFile has the registry credentials the verifier uses
	// (`user:password`), if any.
	RegistryAuthFile opt.Val[string] `koanf:"registry_auth_file,omitempty"`
	// NodePool is the node selector `key=value` of the build pool; required
	// when set.
	NodePool opt.Val[string] `koanf:"node_pool,omitempty"`
	// ScannerImage is the Trivy image that writes the SBOM of every built
	// image and scans it (M4.6); empty disables scanning, and scans are
	// then unavailable.
	ScannerImage string `koanf:"scanner_image"`
	// ScannerMemory is the memory request and limit of the scan container.
	ScannerMemory string `koanf:"scanner_memory"`
	// RescanHours: rescan the images apps run when their newest scan is
	// older than this.
	RescanHours uint32 `koanf:"rescan_hours"`
}

// DefaultBuildCfg is `[build]` when nothing is configured.
func DefaultBuildCfg() BuildCfg {
	return BuildCfg{
		BuildkitImage:       "moby/buildkit:v0.33.0-rootless",
		FetchImage:          "alpine/git:2.49.1",
		ScannerImage:        "aquasec/trivy:0.74.0",
		ScannerMemory:       "1Gi",
		RescanHours:         24,
		CPURequest:          "500m",
		CPULimit:            "2",
		Memory:              "2Gi",
		EphemeralStorage:    "10Gi",
		DeadlineSecs:        1800,
		MaxConcurrent:       2,
		MaxConcurrentPerOrg: 1,
	}
}

// UnpinnedImages are the build images that are not pinned by digest.
func (b BuildCfg) UnpinnedImages() []string {
	images := []string{
		b.BuildkitImage, b.FetchImage, b.RailpackImage.Or(""), b.RailpackFrontend.Or(""), b.ScannerImage,
	}
	return slices.DeleteFunc(images, func(image string) bool {
		return image == "" || strings.Contains(image, "@sha256:")
	})
}

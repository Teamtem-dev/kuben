package render

import (
	"encoding/json"
	"strings"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// GatewayRef names the Gateway API Gateway app routes attach to.
type GatewayRef struct {
	Namespace string
	Name      string
}

// OwnedDefaultGateway is the Gateway Kuben creates when only a
// GatewayClass is configured.
func OwnedDefaultGateway() GatewayRef {
	return GatewayRef{Namespace: "kuben-system", Name: "kuben"}
}

// parseGateway reads `namespace/name`; both parts must be non-empty.
func parseGateway(text string) opt.Val[GatewayRef] {
	ns, name, ok := strings.Cut(text, "/")
	if !ok || ns == "" || name == "" {
		return opt.None[GatewayRef]()
	}
	return opt.Some(GatewayRef{Namespace: ns, Name: name})
}

// Platform is the platform settings resolved from the KubenConfig singleton
// (or its defaults), as the App controller and the renderer read them
// (controller::resources::Platform).
type Platform struct {
	Sizes      []v1alpha1.SizePreset
	BaseDomain opt.Val[string]
	Gateway    opt.Val[GatewayRef]
	// GatewayClass: Kuben creates and owns Gateway with this class (M2.2).
	GatewayClass opt.Val[string]
	// GatewayPorts are the ports of the listeners Kuben writes on its
	// Gateway.
	GatewayPorts v1alpha1.GatewayPorts
	// TLS: routes are served over TLS (a ClusterIssuer is configured and,
	// once gated, usable).
	TLS           bool
	ClusterIssuer opt.Val[string]
	// WildcardTLSSecret is a Secret with a `*.<BaseDomain>` certificate
	// (scenario 9).
	WildcardTLSSecret opt.Val[string]
}

// nonEmpty is absent for nil and for "".
func nonEmpty(p *string) opt.Val[string] {
	if p == nil || *p == "" {
		return opt.None[string]()
	}
	return opt.Some(*p)
}

// DefaultPlatform is the platform of a cluster without a KubenConfig: the
// CRD defaults (the size presets), no gateway and no TLS.
func DefaultPlatform() Platform {
	// `{}` decodes to the CRD defaults, as the Rust from_spec(None) did.
	var spec v1alpha1.KubenConfigSpec
	if err := json.Unmarshal([]byte("{}"), &spec); err != nil {
		return Platform{GatewayPorts: v1alpha1.DefaultGatewayPorts()}
	}
	return PlatformFromSpec(spec)
}

// PlatformFromSpec resolves the settings of a KubenConfig spec.
func PlatformFromSpec(spec v1alpha1.KubenConfigSpec) Platform {
	gatewayClass := nonEmpty(spec.GatewayClassName)
	var gateway opt.Val[GatewayRef]
	if g, ok := nonEmpty(spec.Gateway).Get(); ok {
		gateway = parseGateway(g)
	} else if gatewayClass.IsSome() {
		gateway = opt.Some(OwnedDefaultGateway())
	}
	ports := v1alpha1.DefaultGatewayPorts()
	if spec.GatewayPorts != nil {
		ports = *spec.GatewayPorts
	}
	return Platform{
		Sizes:             append([]v1alpha1.SizePreset(nil), spec.Sizes...),
		BaseDomain:        nonEmpty(spec.BaseDomain),
		Gateway:           gateway,
		GatewayClass:      gatewayClass,
		GatewayPorts:      ports,
		TLS:               spec.ClusterIssuer != nil,
		ClusterIssuer:     opt.FromPtr(spec.ClusterIssuer),
		WildcardTLSSecret: nonEmpty(spec.WildcardTLSSecret),
	}
}

// IssuerFacts is what the capability gate needs from the discovered cluster
// facts (discovery::ClusterFacts): whether a ClusterIssuer is usable, or at
// least not proven unusable (Availability::usable_or_unknown).
type IssuerFacts interface {
	IssuerUsableOrUnknown(name string) bool
}

// Gated narrows the settings to what the cluster can do (ADR-031's
// capability gate): TLS needs the configured ClusterIssuer to be Ready.
// Without facts, or when the probe failed, the configured intent stands. A
// missing capability blocks only TLS; plain HTTP routing still works.
func (p Platform) Gated(facts opt.Val[IssuerFacts]) Platform {
	f, known := facts.Get()
	issuer, configured := p.ClusterIssuer.Get()
	if known && f != nil && configured && !f.IssuerUsableOrUnknown(issuer) {
		p.TLS = false
	}
	return p
}

// Size is the size preset named name.
func (p Platform) Size(name string) (v1alpha1.SizePreset, bool) {
	for _, s := range p.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return v1alpha1.SizePreset{}, false
}

// Capabilities are the cluster facts a plan depends on (ADR-026's
// capability snapshot): the size presets, domains, gateway and TLS settings
// of KubenConfig, narrowed to what the cluster can do (Platform.Gated). The
// JSON is stored with every plan.
type Capabilities struct {
	Sizes      []v1alpha1.SizePreset `json:"sizes"`
	BaseDomain opt.Val[string]       `json:"baseDomain,omitzero"`
	// Gateway is `namespace/name` of the Gateway routes attach to.
	Gateway           opt.Val[string] `json:"gateway,omitzero"`
	ClusterIssuer     opt.Val[string] `json:"clusterIssuer,omitzero"`
	WildcardTLSSecret opt.Val[string] `json:"wildcardTlsSecret,omitzero"`
	// TLSUnavailable: a ClusterIssuer is configured but was not Ready when
	// the plan was rendered (M2.1): routes are served over plain HTTP. Left
	// out when false, so older snapshots stay byte-identical.
	TLSUnavailable bool `json:"tlsUnavailable,omitzero"`
}

// MarshalJSON writes a nil Sizes as [], as the Rust Vec was always written.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	type plain Capabilities
	out := plain(c)
	if out.Sizes == nil {
		out.Sizes = []v1alpha1.SizePreset{}
	}
	return json.Marshal(out)
}

// CapabilitiesOf is the snapshot of the platform settings the App
// controller uses.
func CapabilitiesOf(p Platform) Capabilities {
	var gateway opt.Val[string]
	if g, ok := p.Gateway.Get(); ok {
		gateway = opt.Some(g.Namespace + "/" + g.Name)
	}
	return Capabilities{
		Sizes:             append([]v1alpha1.SizePreset(nil), p.Sizes...),
		BaseDomain:        p.BaseDomain,
		Gateway:           gateway,
		ClusterIssuer:     p.ClusterIssuer,
		WildcardTLSSecret: p.WildcardTLSSecret,
		TLSUnavailable:    p.ClusterIssuer.IsSome() && !p.TLS,
	}
}

// Platform is the platform settings the builder reads, from the snapshot
// alone.
func (c Capabilities) Platform() Platform {
	var gateway opt.Val[GatewayRef]
	if g, ok := c.Gateway.Get(); ok {
		gateway = parseGateway(g)
	}
	return Platform{
		Sizes:      append([]v1alpha1.SizePreset(nil), c.Sizes...),
		BaseDomain: c.BaseDomain,
		Gateway:    gateway,
		// Rendering never needs these: the gateway controller owns the
		// Gateway.
		GatewayClass:      opt.None[string](),
		GatewayPorts:      v1alpha1.DefaultGatewayPorts(),
		TLS:               c.ClusterIssuer.IsSome() && !c.TLSUnavailable,
		ClusterIssuer:     c.ClusterIssuer,
		WildcardTLSSecret: c.WildcardTLSSecret,
	}
}

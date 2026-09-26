// Package discovery is cluster capability discovery (M2.1, ADR-031); it
// replaces the Rust module crates/kuben-platform/src/discovery.rs.
//
// "Installed" is not "compatible": features are gated on what the cluster
// reports, not on what KubenConfig asks for. [Discover] reads the Gateway
// API CRDs (bundle version, channel, served kinds), the GatewayClasses and
// whether their controller accepted them, cert-manager and the readiness of
// its ClusterIssuers, the Metrics API, the API server's version, the
// default StorageClass, what enforces NetworkPolicies and the largest
// schedulable node. A probe that fails (RBAC, timeout) makes its facts
// unknown, never absent: it is named in [ClusterFacts.Unknown].
//
// [Run] repeats discovery, publishes the facts to the controllers and the
// materializer through a [Watch], and records them in SQL for every
// organization's `primary` cluster, where the API and Doctor read them.
package discovery

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
)

// The names of the probes that can fail, as listed in
// [ClusterFacts.Unknown]; stored, and read by the Doctor. A failed listing
// of one Gateway API version is `resources:<group/version>`.
const (
	ProbeAPIGroups         = "apiGroups"
	ProbeGatewayClasses    = "gatewayClasses"
	ProbeClusterIssuers    = "clusterIssuers"
	ProbeGatewayAPIVersion = "gatewayApiVersion"
	ProbeVersion           = "version"
	ProbeStorageClasses    = "storageClasses"
	ProbeNetworkPolicy     = "networkPolicy"
	probeResourcesPrefix   = "resources:"
)

// ClusterFacts is what a cluster can do, as last discovered. Its JSON is
// stored (cluster_capabilities.facts) and served: the field names, the
// always-present lists and the left-out absent values are the contract.
type ClusterFacts struct {
	// GatewayAPI is the Gateway API CRDs, when installed.
	GatewayAPI     opt.Val[GatewayAPI] `json:"gatewayApi,omitzero"`
	GatewayClasses []Readiness         `json:"gatewayClasses"`
	CertManager    bool                `json:"certManager"`
	ClusterIssuers []Readiness         `json:"clusterIssuers"`
	MetricsAPI     bool                `json:"metricsApi"`
	// KubernetesVersion is the API server's version, e.g. `v1.36.4+k3s1`.
	KubernetesVersion opt.Val[string] `json:"kubernetesVersion,omitzero"`
	// DefaultStorageClass is the StorageClass volumes get when they name none.
	DefaultStorageClass opt.Val[string] `json:"defaultStorageClass,omitzero"`
	// NetworkPolicy is what enforces NetworkPolicies (e.g. `cilium`,
	// `k3s`), when found.
	NetworkPolicy opt.Val[string] `json:"networkPolicy,omitzero"`
	// LargestNode is the allocatable resources of the largest node pods may
	// be scheduled on (M4.5).
	LargestNode opt.Val[NodeSize] `json:"largestNode,omitzero"`
	// Unknown is the probes that failed; what they would have found is
	// unknown.
	Unknown []string `json:"unknown,omitempty"`
}

// plainFacts is ClusterFacts without its MarshalJSON. Decoding needs no
// method: every member may be absent (older or partial records still read).
type plainFacts ClusterFacts

// MarshalJSON writes the lists as `[]` when empty, as serde did.
func (f ClusterFacts) MarshalJSON() ([]byte, error) {
	p := plainFacts(f)
	p.GatewayClasses = nonNil(p.GatewayClasses)
	p.ClusterIssuers = nonNil(p.ClusterIssuers)
	return json.Marshal(p)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// NodeSize is a node's allocatable CPU and memory.
type NodeSize struct {
	CPUMillis   uint64 `json:"cpuMillis"`
	MemoryBytes uint64 `json:"memoryBytes"`
}

// UnmarshalJSON requires both members, as serde did.
func (n *NodeSize) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out NodeSize
	if err := jsonx.Required(o, "cpuMillis", &out.CPUMillis); err != nil {
		return err
	}
	if err := jsonx.Required(o, "memoryBytes", &out.MemoryBytes); err != nil {
		return err
	}
	*n = out
	return nil
}

// GatewayAPI is the installed Gateway API.
type GatewayAPI struct {
	// BundleVersion is `gateway.networking.k8s.io/bundle-version` of the
	// Gateway CRD.
	BundleVersion opt.Val[string] `json:"bundleVersion,omitzero"`
	// Channel is `standard` or `experimental`.
	Channel opt.Val[string] `json:"channel,omitzero"`
	// Kinds is the served kinds, e.g. `GRPCRoute`, `HTTPRoute`,
	// `ReferenceGrant`: a set, written sorted and without duplicates.
	Kinds []string `json:"kinds"`
}

// plainGatewayAPI is GatewayAPI without its JSON methods.
type plainGatewayAPI GatewayAPI

// MarshalJSON writes the kinds as the sorted set Rust's BTreeSet was.
func (g GatewayAPI) MarshalJSON() ([]byte, error) {
	p := plainGatewayAPI(g)
	p.Kinds = kindSet(p.Kinds)
	return json.Marshal(p)
}

// UnmarshalJSON reads the kinds as a set.
func (g *GatewayAPI) UnmarshalJSON(data []byte) error {
	var p plainGatewayAPI
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	p.Kinds = kindSet(p.Kinds)
	*g = GatewayAPI(p)
	return nil
}

// kindSet is kinds sorted by their bytes without duplicates, never nil.
func kindSet(kinds []string) []string {
	out := slices.Clone(kinds)
	slices.Sort(out)
	return nonNil(slices.Compact(out))
}

// Readiness is a GatewayClass or ClusterIssuer and whether its controller
// made it usable.
type Readiness struct {
	Name string `json:"name"`
	// Controller is a GatewayClass's `spec.controllerName`.
	Controller opt.Val[string] `json:"controller,omitzero"`
	// Ready is `Accepted` for a GatewayClass, `Ready` for a ClusterIssuer.
	Ready bool `json:"ready"`
	// Message is why it is not ready, when its controller said.
	Message opt.Val[string] `json:"message,omitzero"`
}

// UnmarshalJSON requires the name and the readiness, as serde did.
func (r *Readiness) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out Readiness
	if err := jsonx.Required(o, "name", &out.Name); err != nil {
		return err
	}
	if err := jsonx.Optional(o, "controller", &out.Controller); err != nil {
		return err
	}
	if err := jsonx.Required(o, "ready", &out.Ready); err != nil {
		return err
	}
	if err := jsonx.Optional(o, "message", &out.Message); err != nil {
		return err
	}
	*r = out
	return nil
}

// Equal reports whether f and other are the same facts: an empty list
// equals an absent one and kinds compare as sets.
func (f ClusterFacts) Equal(other ClusterFacts) bool {
	if f.CertManager != other.CertManager || f.MetricsAPI != other.MetricsAPI ||
		f.KubernetesVersion != other.KubernetesVersion ||
		f.DefaultStorageClass != other.DefaultStorageClass ||
		f.NetworkPolicy != other.NetworkPolicy || f.LargestNode != other.LargestNode ||
		!slices.Equal(f.GatewayClasses, other.GatewayClasses) ||
		!slices.Equal(f.ClusterIssuers, other.ClusterIssuers) ||
		!slices.Equal(f.Unknown, other.Unknown) {
		return false
	}
	a, aok := f.GatewayAPI.Get()
	b, bok := other.GatewayAPI.Get()
	if aok != bok {
		return false
	}
	return !aok || a.BundleVersion == b.BundleVersion && a.Channel == b.Channel &&
		slices.Equal(kindSet(a.Kinds), kindSet(b.Kinds))
}

// clone is a deep copy, so a published value cannot be changed through a
// copy handed to a reader.
func (f ClusterFacts) clone() ClusterFacts {
	out := f
	if g, ok := f.GatewayAPI.Get(); ok {
		g.Kinds = slices.Clone(g.Kinds)
		out.GatewayAPI = opt.Some(g)
	}
	out.GatewayClasses = slices.Clone(f.GatewayClasses)
	out.ClusterIssuers = slices.Clone(f.ClusterIssuers)
	out.Unknown = slices.Clone(f.Unknown)
	return out
}

// NodeCapacity is the largest schedulable node, for admission.
func (f ClusterFacts) NodeCapacity() opt.Val[capacity.NodeCapacity] {
	n, ok := f.LargestNode.Get()
	if !ok {
		return opt.None[capacity.NodeCapacity]()
	}
	return opt.Some(capacity.NodeCapacity{CPUMillis: n.CPUMillis, MemoryBytes: n.MemoryBytes})
}

// Availability is whether a named dependency can be used.
//
//sumtype:decl
type Availability interface{ availability() }

// Ready is usable.
type Ready struct{}

// NotReady is present but not usable; Message is its controller's reason,
// possibly empty.
type NotReady struct{ Message string }

// Missing is proven absent.
type Missing struct{}

// Unknown is a failed probe: neither usable nor proven missing.
type Unknown struct{}

func (Ready) availability()    {}
func (NotReady) availability() {}
func (Missing) availability()  {}
func (Unknown) availability()  {}

// UsableOrUnknown reports whether a is usable, or not proven otherwise.
func UsableOrUnknown(a Availability) bool {
	switch a.(type) {
	case Ready, Unknown:
		return true
	case NotReady, Missing:
		return false
	}
	return false
}

// find is the availability of name in list; probe is the probe that fills
// list.
func find(list []Readiness, name, probe string, unknown []string) Availability {
	i := slices.IndexFunc(list, func(r Readiness) bool { return r.Name == name })
	switch {
	case i >= 0 && list[i].Ready:
		return Ready{}
	case i >= 0:
		return NotReady{Message: list[i].Message.Or("")}
	case slices.ContainsFunc(unknown, func(u string) bool { return u == probe || u == ProbeAPIGroups }):
		return Unknown{}
	default:
		return Missing{}
	}
}

// Issuer is the availability of the ClusterIssuer name.
func (f ClusterFacts) Issuer(name string) Availability {
	return find(f.ClusterIssuers, name, ProbeClusterIssuers, f.Unknown)
}

// IssuerUsableOrUnknown reports whether the ClusterIssuer name is usable,
// or not proven otherwise: the capability gate of render.Platform.Gated.
func (f ClusterFacts) IssuerUsableOrUnknown(name string) bool {
	return UsableOrUnknown(f.Issuer(name))
}

// GatewayClass is the availability of the GatewayClass name.
func (f ClusterFacts) GatewayClass(name string) Availability {
	return find(f.GatewayClasses, name, ProbeGatewayClasses, f.Unknown)
}

// Serves reports whether the Gateway API serves kind (e.g. `GRPCRoute`).
func (f ClusterFacts) Serves(kind string) bool {
	g, ok := f.GatewayAPI.Get()
	return ok && slices.Contains(g.Kinds, kind)
}

// Summary is one line for logs and Doctor.
func (f ClusterFacts) Summary() string {
	gateway := "no Gateway API"
	if g, ok := f.GatewayAPI.Get(); ok {
		gateway = fmt.Sprintf("Gateway API %s (%s)",
			g.BundleVersion.Or("unknown version"), g.Channel.Or("unknown channel"))
	}
	unknown := ""
	if len(f.Unknown) > 0 {
		unknown = "; unknown: " + strings.Join(f.Unknown, ", ")
	}
	return fmt.Sprintf("%s; gateway classes: %s; cert-manager: %s; cluster issuers: %s; metrics: %s%s",
		gateway, readyNames(f.GatewayClasses), yesNo(f.CertManager),
		readyNames(f.ClusterIssuers), yesNo(f.MetricsAPI), unknown)
}

func readyNames(list []Readiness) string {
	var names []string
	for _, r := range list {
		if r.Ready {
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return "none ready"
	}
	return strings.Join(names, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

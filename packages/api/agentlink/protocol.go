// Package agentlink is the AgentLink protocol (ADR-027, plan §12), the port
// of crates/kuben-agent/src/protocol.rs, tls.rs and the parts of enroll.rs
// both ends share: the messages the cluster agent and the hub exchange over
// the mTLS stream the agent opens, their framing, the handshake's version
// and feature negotiation, the TLS configurations of both ends and the
// device key an agent enrolls with.
//
//   - A frame is a big-endian uint32 length followed by that many bytes of
//     JSON, at most [MaxFrame]. A peer that announces more is cut off before
//     anything is allocated.
//   - The agent speaks first: [Hello] offers protocol versions and
//     capabilities. The hub answers [Welcome] with the highest version both
//     speak and the features both know, or with [Refused]. A build speaks
//     its own version N and N-1 ([SupportedVersions]), so a hub at N serves
//     agents at N and N-1.
//   - A message type this build does not know decodes as [Unknown] instead
//     of breaking the link, so a newer peer may add messages. A command that
//     needs a feature outside the negotiated set is refused with
//     [RefusalUnsupportedCapability] and never applied.
//
// The JSON of every message is written byte for byte as serde_json wrote
// the Rust types (tag first, members in declaration order, serde's string
// escaping), and read as strictly as serde read them.
package agentlink

import "slices"

// ProtocolVersion is the protocol version of this build.
const ProtocolVersion uint32 = 1

// oldestVersion is N-1, or 1 while N is 1.
const oldestVersion = max(ProtocolVersion, 2) - 1

// Versions is an inclusive range of protocol versions.
type Versions struct {
	Min, Max uint32
}

// Contains says whether v is in the range.
func (r Versions) Contains(v uint32) bool { return r.Min <= v && v <= r.Max }

// Descending lists the range from its highest version down, as Hello
// offers it.
func (r Versions) Descending() []uint32 {
	var out []uint32
	for v := r.Max; v >= r.Min && v > 0; v-- {
		out = append(out, v)
	}
	return out
}

// SupportedVersions are the versions this build speaks: N and N-1.
var SupportedVersions = Versions{Min: oldestVersion, Max: ProtocolVersion} //nolint:gochecknoglobals // a constant range

// MaxFrame is the largest frame either side sends or accepts, in bytes.
// Execution envelopes are bounded far below it (128 KiB).
const MaxFrame = 1024 * 1024

// ApplicationRuntime is the feature that lets the hub hand the agent
// execution envelopes ([Apply]) and hear back ([Observation]).
const ApplicationRuntime = "applicationRuntime"

// Features is a set of feature names, sorted and without duplicates (the
// Rust BTreeSet): the capabilities an agent offers, the features a hub
// knows, the ones both agreed on.
type Features []string

// NewFeatures is the set of names.
func NewFeatures(names ...string) Features {
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}

// Has says whether the set holds name.
func (f Features) Has(name string) bool { return slices.Contains(f, name) }

// Intersect is the names both sets hold.
func (f Features) Intersect(other Features) Features {
	out := Features{}
	for _, name := range NewFeatures(f...) {
		if other.Has(name) {
			out = append(out, name)
		}
	}
	return out
}

// RuntimePhase is how far the agent got with a target's envelope.
type RuntimePhase string

// The phases. A phase this build does not know reads as
// [RuntimePhaseUnknown] (serde's `other`).
const (
	// RuntimePhaseAccepted: the envelope is written to the cluster.
	RuntimePhaseAccepted RuntimePhase = "accepted"
	// RuntimePhaseApplying: its resources are applied and rolling out.
	RuntimePhaseApplying RuntimePhase = "applying"
	// RuntimePhaseReady: every workload of the envelope's generation is
	// available.
	RuntimePhaseReady RuntimePhase = "ready"
	// RuntimePhaseFailed: it will not become ready without a new generation.
	RuntimePhaseFailed RuntimePhase = "failed"
	// RuntimePhaseRejected: the agent or the cluster refused the envelope (a
	// stale generation, a digest that does not match, a kind the agent does
	// not apply).
	RuntimePhaseRejected RuntimePhase = "rejected"
	// RuntimePhaseUnknown is a phase this build does not know.
	RuntimePhaseUnknown RuntimePhase = "unknown"
)

// ParseRuntimePhase reads a phase; an unknown one is RuntimePhaseUnknown,
// as serde's `other` made it.
func ParseRuntimePhase(s string) RuntimePhase {
	switch p := RuntimePhase(s); p {
	case RuntimePhaseAccepted, RuntimePhaseApplying, RuntimePhaseReady, RuntimePhaseFailed,
		RuntimePhaseRejected, RuntimePhaseUnknown:
		return p
	}
	return RuntimePhaseUnknown
}

// Refusal says why one side refused. It is an error, so a function that
// refuses returns it as one.
type Refusal string

// The refusals. A reason this build does not know reads as RefusalOther
// (serde's `other`).
const (
	// RefusalUnsupportedProtocol: no protocol version in common.
	RefusalUnsupportedProtocol Refusal = "unsupportedProtocol"
	// RefusalUnsupportedCapability: a command needs a feature outside the
	// negotiated set.
	RefusalUnsupportedCapability Refusal = "unsupportedCapability"
	// RefusalUnknownCluster: the certificate names no enrolled cluster.
	RefusalUnknownCluster Refusal = "unknownCluster"
	// RefusalRevoked: the cluster's enrollment was revoked: re-enroll.
	RefusalRevoked Refusal = "revoked"
	// RefusalBadRequest: the message does not fit the protocol at this point.
	RefusalBadRequest Refusal = "badRequest"
	// RefusalInvalidToken: the bootstrap token is unknown, expired, for
	// another cluster or redeemed by another device (one answer for all, to
	// reveal nothing).
	RefusalInvalidToken Refusal = "invalidToken"
	// RefusalOther is a reason this build does not know.
	RefusalOther Refusal = "other"
)

// ParseRefusal reads a reason; an unknown one is RefusalOther.
func ParseRefusal(s string) Refusal {
	switch r := Refusal(s); r {
	case RefusalUnsupportedProtocol, RefusalUnsupportedCapability, RefusalUnknownCluster, RefusalRevoked,
		RefusalBadRequest, RefusalInvalidToken, RefusalOther:
		return r
	}
	return RefusalOther
}

// Error is the reason as Rust's Debug printed it.
func (r Refusal) Error() string {
	switch r {
	case RefusalUnsupportedProtocol:
		return "UnsupportedProtocol"
	case RefusalUnsupportedCapability:
		return "UnsupportedCapability"
	case RefusalUnknownCluster:
		return "UnknownCluster"
	case RefusalRevoked:
		return "Revoked"
	case RefusalBadRequest:
		return "BadRequest"
	case RefusalInvalidToken:
		return "InvalidToken"
	case RefusalOther:
		return "Other"
	}
	return "Other"
}

// Token is a bootstrap token on the wire: written as the plain string,
// never shown by fmt, so logging a message cannot leak it.
type Token struct {
	secret string
}

// NewToken wraps a bootstrap token.
func NewToken(secret string) Token { return Token{secret: secret} }

// Expose is the token itself, for the one place that hashes it.
func (t Token) Expose() string { return t.secret }

// String hides the token.
func (Token) String() string { return "Token(***)" }

// GoString hides the token from %#v.
func (Token) GoString() string { return "Token(***)" }

// Message is one AgentLink message.
//
//sumtype:decl
type Message interface {
	// messageType is the wire tag.
	messageType() string
}

// Hello is the agent's opening: who it is and what it can do.
type Hello struct {
	ProtocolVersions []uint32
	AgentVersion     string
	ClusterID        string
	// Capabilities default to none when absent.
	Capabilities Features
	// KubernetesVersion is left out when nil.
	KubernetesVersion *string
}

// Welcome is the hub's acceptance: the version and features both sides
// agreed on, and how often the agent sends a heartbeat.
type Welcome struct {
	ProtocolVersion uint32
	HubVersion      string
	HeartbeatMs     uint64
	Features        Features
}

// Refused says the other side will not go on; the link closes after it.
type Refused struct {
	Reason  Refusal
	Message string
}

// Heartbeat is the agent's keep-alive.
type Heartbeat struct {
	Seq uint64
}

// HeartbeatAck is the hub's answer to a Heartbeat.
type HeartbeatAck struct {
	Seq uint64
}

// Enroll is an anonymous agent asking for its identity: the bootstrap
// token, its cluster and a CSR (PEM) signed by its device key.
type Enroll struct {
	Token     Token
	ClusterID string
	CSR       string
}

// Renew is a linked agent asking for a fresh certificate before its own
// expires: a CSR (PEM) signed by the same device key.
type Renew struct {
	CSR string
}

// Enrolled is the hub's answer to Enroll and Renew: the client certificate
// (PEM) and when it expires (Unix seconds).
type Enrolled struct {
	Certificate string
	NotAfter    int64
}

// Apply is an execution envelope for one application target, hub to agent
// ([ApplicationRuntime]).
type Apply struct {
	// Target is the application target (its SQL id).
	Target string `json:"target"`
	// Namespace and Name are where the target's ApplicationRuntime lives.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Spec is the ApplicationRuntime spec, as JSON.
	Spec string `json:"spec"`
}

// Observation is what the agent observed of a target's runtime, agent to
// hub (the `observed` message, [ApplicationRuntime]).
type Observation struct {
	Target string
	// Generation is the generation the observation is about.
	Generation int64
	Phase      RuntimePhase
	// Reason and Message are left out when nil.
	Reason  *string
	Message *string
}

// Unknown is a message type this build does not know.
type Unknown struct{}

func (Hello) messageType() string        { return "hello" }
func (Welcome) messageType() string      { return "welcome" }
func (Refused) messageType() string      { return "refused" }
func (Heartbeat) messageType() string    { return "heartbeat" }
func (Enroll) messageType() string       { return "enroll" }
func (Renew) messageType() string        { return "renew" }
func (Enrolled) messageType() string     { return "enrolled" }
func (HeartbeatAck) messageType() string { return "heartbeatAck" }
func (Apply) messageType() string        { return "apply" }
func (Observation) messageType() string  { return "observed" }
func (Unknown) messageType() string      { return "unknown" }

// Negotiated is what both sides agreed on in the handshake.
type Negotiated struct {
	Version  uint32
	Features Features
}

// Require refuses a command that needs feature unless both sides know it.
func (n Negotiated) Require(feature string) error {
	if n.Features.Has(feature) {
		return nil
	}
	return RefusalUnsupportedCapability
}

// Negotiate is the hub's side of the handshake: the highest version in
// versions the agent offers, and the features in features the agent also
// offers. The error is RefusalUnsupportedProtocol.
func Negotiate(versions Versions, features Features, offeredVersions []uint32, offeredFeatures Features) (Negotiated, error) {
	var best uint32
	found := false
	for _, v := range offeredVersions {
		if versions.Contains(v) && (!found || v > best) {
			best, found = v, true
		}
	}
	if !found {
		return Negotiated{}, RefusalUnsupportedProtocol
	}
	return Negotiated{Version: best, Features: features.Intersect(offeredFeatures)}, nil
}

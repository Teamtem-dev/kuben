// Package protocol is the hub↔agent protocol (crates/kuben-agent/src/
// protocol.rs). Only the execution envelope the materializer hands to the
// hub is here so far; the frames, the handshake and the rest of the
// protocol follow with the agent (slice S2).
package protocol

// Apply is an execution envelope for one application target, hub to agent.
type Apply struct {
	// Target is the application target (its SQL id).
	Target string `json:"target"`
	// Namespace and Name are where the target's ApplicationRuntime lives.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Spec is the ApplicationRuntime spec, as JSON.
	Spec string `json:"spec"`
}

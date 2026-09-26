package v1alpha1

import "encoding/json"

// Condition is a kstatus-style condition. It mirrors metav1.Condition
// without its required members: the Rust type let reason, message,
// lastTransitionTime and observedGeneration be absent.
type Condition struct {
	// Type is the condition type, e.g. ConditionReady.
	Type string `json:"type"`
	// Status is "True", "False" or "Unknown".
	Status string `json:"status"`
	// Reason is a CamelCase reason for the last transition.
	Reason *string `json:"reason,omitempty"`
	// Message is a human readable explanation.
	Message *string `json:"message,omitempty"`
	// LastTransitionTime is an RFC 3339 timestamp.
	LastTransitionTime *string `json:"lastTransitionTime,omitempty"`
	// ObservedGeneration is the metadata.generation the condition was set
	// for.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
}

// NewCondition returns a condition of the given type whose status is "True"
// or "False", with a reason and nothing else.
func NewCondition(conditionType string, status bool, reason string) Condition {
	s := "False"
	if status {
		s = "True"
	}
	return Condition{Type: conditionType, Status: s, Reason: &reason}
}

// Standard condition types used across Kuben resources.
const (
	// ConditionReady means the resource reached its desired state.
	ConditionReady = "Ready"
	// ConditionProgressing means the resource is moving towards it.
	ConditionProgressing = "Progressing"
	// ConditionDegraded means the resource is not working as it should.
	ConditionDegraded = "Degraded"
)

// Conditions is a status's list of conditions. Most statuses always wrote
// the list, empty or not (serde default without a skip rule), so a nil
// Conditions encodes as [] rather than null; statuses that leave an empty
// list out tag the field omitempty.
type Conditions []Condition

// MarshalJSON encodes nil as an empty list.
func (c Conditions) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Condition(c))
}

// KeyRef references a key inside a Secret, ConfigMap or Service in the same
// namespace.
type KeyRef struct {
	// Name is the object's name.
	Name string `json:"name"`
	// Key is the key inside the object.
	Key string `json:"key"`
}

// SizePreset is a compute size preset, resolved from
// KubenConfigSpec.Sizes.
type SizePreset struct {
	// Name is what App processes refer to, e.g. "small".
	Name string `json:"name"`
	// CPURequest is a Kubernetes quantity, e.g. "100m".
	CPURequest string `json:"cpuRequest"`
	// CPULimit is a Kubernetes quantity; nil means no limit. Unlike the
	// other optional fields it is written as null when unset, as the Rust
	// type did (no skip rule).
	// +optional
	CPULimit *string `json:"cpuLimit"`
	// MemoryRequest is a Kubernetes quantity, e.g. "128Mi".
	MemoryRequest string `json:"memoryRequest"`
	// MemoryLimit is a Kubernetes quantity, e.g. "256Mi".
	MemoryLimit string `json:"memoryLimit"`
}

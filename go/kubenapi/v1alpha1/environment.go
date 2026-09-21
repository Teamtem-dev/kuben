package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Environment is cluster-scoped and owns exactly one Namespace
// (kb-<project>-<env>) with PSA, ResourceQuota and NetworkPolicy. Deletion
// is soft by default (ADR-018).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=kenv
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type="string",JSONPath=".spec.project"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".status.namespace"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
type Environment struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired environment.
	Spec EnvironmentSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *EnvironmentStatus `json:"status,omitempty"`
}

// EnvironmentList is a list of Environment.
//
// +kubebuilder:object:root=true
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []Environment `json:"items"`
}

// EnvironmentSpec is the desired environment.
type EnvironmentSpec struct {
	// Project is the name of the owning Project.
	Project string `json:"project"`
	// Type is the environment's kind; decoding defaults it to
	// EnvironmentTypeStandard.
	// +optional
	// +kubebuilder:default="standard"
	Type EnvironmentType `json:"type"`
	// DeletionPolicy is what happens to the namespace when this resource is
	// deleted; decoding defaults it to DeletionPolicyRetain.
	// +optional
	// +kubebuilder:default="Retain"
	DeletionPolicy DeletionPolicy `json:"deletionPolicy"`
	// Protection has the production protection rules (approvals, windows).
	Protection *Protection `json:"protection,omitempty"`
	// Quota is the resource quota applied to the namespace.
	Quota *Quota `json:"quota,omitempty"`
	// TTL has the time-to-live settings of a preview.
	TTL *TTL `json:"ttl,omitempty"`
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (s *EnvironmentSpec) UnmarshalJSON(data []byte) error {
	type plain EnvironmentSpec
	out := plain{Type: EnvironmentTypeStandard, DeletionPolicy: DeletionPolicyRetain}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*s = EnvironmentSpec(out)
	return nil
}

// EnvironmentType is an environment's kind. The zero value means
// EnvironmentTypeStandard, the Rust default.
//
// +kubebuilder:validation:Enum=standard;production;preview
type EnvironmentType string

// Environment types.
const (
	// EnvironmentTypeStandard is an ordinary environment.
	EnvironmentTypeStandard EnvironmentType = "standard"
	// EnvironmentTypeProduction is a production environment.
	EnvironmentTypeProduction EnvironmentType = "production"
	// EnvironmentTypePreview is a short-lived preview environment.
	EnvironmentTypePreview EnvironmentType = "preview"
)

// Valid reports whether t is one of the EnvironmentType constants.
func (t EnvironmentType) Valid() bool {
	switch t {
	case EnvironmentTypeStandard, EnvironmentTypeProduction, EnvironmentTypePreview:
		return true
	}
	return false
}

// ParseEnvironmentType returns the EnvironmentType spelled text.
func ParseEnvironmentType(text string) (EnvironmentType, error) {
	return parseEnum(text, "environment type", EnvironmentType.Valid)
}

// MarshalJSON writes the wire string; the zero value is "standard".
func (t EnvironmentType) MarshalJSON() ([]byte, error) {
	return marshalEnum(t, EnvironmentTypeStandard, "environment type", EnvironmentType.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (t *EnvironmentType) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, t, "environment type", EnvironmentType.Valid)
}

// DeletionPolicy is what happens to an environment's namespace when the
// Environment is deleted. The zero value means DeletionPolicyRetain, the
// Rust default. Unlike the other enums its wire strings are capitalized
// (the Rust type had no rename_all).
//
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

// Deletion policies.
const (
	// DeletionPolicyRetain keeps the namespace and its data; only Kuben's
	// ownership is removed.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete deletes the namespace after the grace period.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// Valid reports whether p is one of the DeletionPolicy constants.
func (p DeletionPolicy) Valid() bool {
	switch p {
	case DeletionPolicyRetain, DeletionPolicyDelete:
		return true
	}
	return false
}

// ParseDeletionPolicy returns the DeletionPolicy spelled text.
func ParseDeletionPolicy(text string) (DeletionPolicy, error) {
	return parseEnum(text, "deletion policy", DeletionPolicy.Valid)
}

// MarshalJSON writes the wire string; the zero value is "Retain".
func (p DeletionPolicy) MarshalJSON() ([]byte, error) {
	return marshalEnum(p, DeletionPolicyRetain, "deletion policy", DeletionPolicy.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (p *DeletionPolicy) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, p, "deletion policy", DeletionPolicy.Valid)
}

// Protection has the protection rules of a production environment.
type Protection struct {
	// RequireApprovals is the number of distinct approvers required to
	// promote a release here.
	// +optional
	// +kubebuilder:default=0
	RequireApprovals uint32 `json:"requireApprovals"`
	// DeletionGrace is the grace period before a soft-deleted environment
	// is purged, e.g. "168h"; decoding defaults it to
	// DefaultDeletionGrace.
	// +optional
	// +kubebuilder:default="168h"
	DeletionGrace string `json:"deletionGrace"`
}

// DefaultDeletionGrace is the grace period of a protection that does not
// name one.
const DefaultDeletionGrace = "168h"

// UnmarshalJSON applies the serde defaults of the Rust type.
func (p *Protection) UnmarshalJSON(data []byte) error {
	type plain Protection
	out := plain{DeletionGrace: DefaultDeletionGrace}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = Protection(out)
	return nil
}

// Quota is the resource quota of an environment's namespace.
type Quota struct {
	// CPU is a Kubernetes quantity.
	CPU *string `json:"cpu,omitempty"`
	// Memory is a Kubernetes quantity.
	Memory *string `json:"memory,omitempty"`
	// Pods is the maximum number of pods.
	Pods *uint32 `json:"pods,omitempty"`
}

// TTL has the time-to-live settings of a preview environment.
type TTL struct {
	// Idle deletes the environment after this much time without traffic,
	// e.g. "48h".
	Idle *string `json:"idle,omitempty"`
	// Max is the hard maximum lifetime, e.g. "14d".
	Max *string `json:"max,omitempty"`
}

// EnvironmentStatus is what the environment controller observed.
type EnvironmentStatus struct {
	// ObservedGeneration is the metadata.generation last acted on.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// Namespace is the namespace the environment owns.
	Namespace *string `json:"namespace,omitempty"`
	// Phase is Pending, Ready, Terminating or Degraded.
	Phase *string `json:"phase,omitempty"`
	// DeletionScheduledAt is an RFC 3339 timestamp.
	DeletionScheduledAt *string `json:"deletionScheduledAt,omitempty"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}

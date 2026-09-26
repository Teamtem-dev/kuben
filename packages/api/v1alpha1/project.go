package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Project is a cluster-scoped grouping of environments. It is owned by an
// Org (label kuben.dev/org) and referenced from SQL only by uid (ADR-015).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=kproj
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Display",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Environments",type="integer",JSONPath=".status.environments"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Project struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired project.
	Spec ProjectSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *ProjectStatus `json:"status,omitempty"`
}

// ProjectList is a list of Project.
//
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []Project `json:"items"`
}

// ProjectSpec is the desired project.
type ProjectSpec struct {
	// DisplayName is the human readable name.
	DisplayName string `json:"displayName"`
	// Description is free text.
	Description *string `json:"description,omitempty"`
	// Previews is the preview environment policy; decoding defaults it to
	// DefaultPreviewPolicy.
	// +optional
	// +kubebuilder:default={max:10}
	Previews PreviewPolicy `json:"previews"`
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (s *ProjectSpec) UnmarshalJSON(data []byte) error {
	type plain ProjectSpec
	out := plain{Previews: DefaultPreviewPolicy()}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*s = ProjectSpec(out)
	return nil
}

// PreviewPolicy is a project's preview environment policy.
type PreviewPolicy struct {
	// Max is the maximum number of concurrent preview environments;
	// decoding defaults it to 10.
	// +optional
	// +kubebuilder:default=10
	Max uint32 `json:"max"`
	// Template is the environment used as template for previews.
	Template *string `json:"template,omitempty"`
}

// DefaultPreviewPolicy allows 10 previews without a template, the Rust
// Default.
func DefaultPreviewPolicy() PreviewPolicy { return PreviewPolicy{Max: 10} }

// UnmarshalJSON applies the serde defaults of the Rust type.
func (p *PreviewPolicy) UnmarshalJSON(data []byte) error {
	type plain PreviewPolicy
	out := plain(DefaultPreviewPolicy())
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = PreviewPolicy(out)
	return nil
}

// ProjectStatus is what the project controller observed.
type ProjectStatus struct {
	// ObservedGeneration is the metadata.generation last acted on.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// Environments is the number of the project's environments.
	// +optional
	// +kubebuilder:default=0
	Environments uint32 `json:"environments"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}

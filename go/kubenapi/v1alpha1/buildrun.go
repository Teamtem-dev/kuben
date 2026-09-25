package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildRun is one image build. It doubles as a durable queue: the controller
// dispatches at most maxConcurrent builds per org (Master Blueprint §4.4).
// Build pods never receive a ServiceAccount token (Invariant I-7).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=kbr
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type="string",JSONPath=".spec.app"
// +kubebuilder:printcolumn:name="Ref",type="string",JSONPath=".spec.gitRef"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type BuildRun struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the build to run.
	Spec BuildRunSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *BuildRunStatus `json:"status,omitempty"`
}

// BuildRunList is a list of BuildRun.
//
// +kubebuilder:object:root=true
type BuildRunList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []BuildRun `json:"items"`
}

// BuildRunSpec is the build to run.
type BuildRunSpec struct {
	// App is the namespace/name of the App being built.
	App string `json:"app"`
	// Repo is the repository URL.
	Repo string `json:"repo"`
	// GitRef is a commit SHA (preferred) or ref.
	GitRef string `json:"gitRef"`
	// Path is the build context inside the repository; empty is the root
	// and is left out of the JSON.
	Path string `json:"path,omitempty"`
	// Strategy picks the builder; decoding defaults it to
	// BuildStrategyAuto.
	// +optional
	// +kubebuilder:default="auto"
	Strategy BuildStrategy `json:"strategy"`
	// Image is the target image (tag form); the resulting digest lands in
	// the status.
	Image string `json:"image"`
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (s *BuildRunSpec) UnmarshalJSON(data []byte) error {
	type plain BuildRunSpec
	out := plain{Strategy: BuildStrategyAuto}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*s = BuildRunSpec(out)
	return nil
}

// BuildRunStatus is what the build controller observed.
type BuildRunStatus struct {
	// Phase is Queued, Running, Succeeded, Failed or Cancelled.
	Phase *string `json:"phase,omitempty"`
	// JobName is the build Job.
	JobName *string `json:"jobName,omitempty"`
	// ImageDigest is the digest of the pushed image.
	ImageDigest *string `json:"imageDigest,omitempty"`
	// StartedAt is an RFC 3339 timestamp.
	StartedAt *string `json:"startedAt,omitempty"`
	// FinishedAt is an RFC 3339 timestamp.
	FinishedAt *string `json:"finishedAt,omitempty"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}

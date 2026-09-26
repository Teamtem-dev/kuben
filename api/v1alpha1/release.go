package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Release is the immutable record of what was deployed: an image digest
// plus a snapshot of the non-secret App spec and Secret references.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=krel
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type="string",JSONPath=".spec.app"
// +kubebuilder:printcolumn:name="Digest",type="string",JSONPath=".spec.imageDigest"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Release struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is what the release deploys.
	Spec ReleaseSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *ReleaseStatus `json:"status,omitempty"`
}

// ReleaseList is a list of Release.
//
// +kubebuilder:object:root=true
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []Release `json:"items"`
}

// ReleaseSpec is what a release deploys.
type ReleaseSpec struct {
	// App is the name of the App this release belongs to.
	App string `json:"app"`
	// ImageDigest is the fully qualified image reference pinned by digest
	// (repo@sha256:...).
	ImageDigest string `json:"imageDigest"`
	// GitSHA is the git commit that produced the image, if Kuben built it.
	GitSHA *string `json:"gitSha,omitempty"`
	// AppSpec is the snapshot of the App spec at release time.
	AppSpec AppSpec `json:"appSpec"`
	// SecretRefs are the Secret names and resourceVersions in effect at
	// release time.
	SecretRefs []SecretVersionRef `json:"secretRefs,omitempty"`
	// CreatedBy is who or what created this release (a user id or
	// "webhook:github").
	CreatedBy *string `json:"createdBy,omitempty"`
}

// SecretVersionRef pins a Secret at a resourceVersion.
type SecretVersionRef struct {
	// Name is the Secret's name.
	Name string `json:"name"`
	// ResourceVersion is the Secret's metadata.resourceVersion.
	ResourceVersion string `json:"resourceVersion"`
}

// ReleaseStatus is what the release controller observed.
type ReleaseStatus struct {
	// Phase is Pending, AwaitingApproval, RollingOut, Active, Superseded or
	// Failed.
	Phase *string `json:"phase,omitempty"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}

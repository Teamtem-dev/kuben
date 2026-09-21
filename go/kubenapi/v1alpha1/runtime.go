package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MaxEnvelopeBytes is the upper bound of an envelope's frozen resources, in
// bytes (ADR-027): a larger model is refused or moved to an artifact
// referenced by digest.
const MaxEnvelopeBytes = 128 * 1024

// ApplicationRuntime is the execution envelope of one application target
// (ADR-027, plan §9.5). It is internal: the hub writes it, the cluster agent
// acts on it. It holds the last accepted envelope (lifecycle UID, control
// epoch, generation, input hash and the frozen RenderPlan) and, in the
// status, what the agent observed and made effective.
//
// The apiserver enforces the envelope's ordering with CEL: the target and
// its lifecycle UID never change; generation and controlEpoch never
// decrease; the same generation carries the same envelope (a different
// inputHash, release or plan under an unchanged generation is refused as
// an integrity error, while re-applying the same envelope is a no-op).
//
// The plan's resources are canonical JSON in a string rather than a list of
// objects: CEL cannot see fields a schema leaves unknown, so only a string
// makes "the same generation, the same resources" enforceable. The agent
// checks the resources against the plan digest before applying them.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=kbrt
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Generation",type="integer",JSONPath=".spec.generation"
// +kubebuilder:printcolumn:name="Observed",type="integer",JSONPath=".status.observedGeneration"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ApplicationRuntime struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the accepted envelope.
	Spec ApplicationRuntimeSpec `json:"spec"`
	// Status is what the agent observed; nil until it wrote one.
	Status *ApplicationRuntimeStatus `json:"status,omitempty"`
}

// ApplicationRuntimeList is a list of ApplicationRuntime.
//
// +kubebuilder:object:root=true
type ApplicationRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []ApplicationRuntime `json:"items"`
}

// ApplicationRuntimeSpec is the envelope the hub accepted for a target.
//
// +kubebuilder:validation:XValidation:rule="self.generation != oldSelf.generation || (self.inputHash == oldSelf.inputHash && self.releaseId == oldSelf.releaseId && self.plan == oldSelf.plan)",message="the same generation must carry the same envelope (input hash, release and plan)"
type ApplicationRuntimeSpec struct {
	// TargetID is the application target (SQL application_targets.id).
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetId is immutable"
	TargetID string `json:"targetId"`
	// LifecycleUID is the target's lifecycle UID: a recreated target is a
	// new runtime.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="lifecycleUid is immutable"
	LifecycleUID string `json:"lifecycleUid"`
	// ControlEpoch is raised when control of the target moves (a new
	// controller or a cutover); an older epoch never writes again.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="controlEpoch never decreases"
	ControlEpoch int64 `json:"controlEpoch"`
	// Generation is the target's desired generation this envelope carries.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="a lower generation is rejected"
	Generation int64 `json:"generation"`
	// InputHash is the digest of the run's inputs (release, configuration,
	// generation).
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	InputHash string `json:"inputHash"`
	// ReleaseID is the release this generation makes effective.
	// +kubebuilder:validation:MaxLength=64
	ReleaseID string `json:"releaseId"`
	// Plan is the frozen RenderPlan.
	Plan PlanEnvelope `json:"plan"`
}

// PlanEnvelope is the frozen RenderPlan an envelope carries (ADR-026):
// enough to apply and resume without asking the hub.
type PlanEnvelope struct {
	// ID is SQL render_plans.id.
	// +kubebuilder:validation:MaxLength=64
	ID string `json:"id"`
	// RendererVersion names the renderer that produced the plan.
	// +kubebuilder:validation:MaxLength=64
	RendererVersion string `json:"rendererVersion"`
	// Digest is the sha256: digest of Resources.
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Resources are the normalized resources as canonical JSON (an array of
	// objects).
	// +kubebuilder:validation:MaxLength=131072
	Resources string `json:"resources"`
}

// ApplicationRuntimeStatus is what the agent observed and made effective.
type ApplicationRuntimeStatus struct {
	// ObservedGeneration is the newest generation the agent has acted on.
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="observedGeneration never decreases"
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// EffectiveGeneration is the generation whose resources are live and
	// ready.
	EffectiveGeneration *int64 `json:"effectiveGeneration,omitempty"`
	// EffectiveRelease is the release of EffectiveGeneration.
	EffectiveRelease *string `json:"effectiveRelease,omitempty"`
	// RecoveryLatch is set when the agent stopped moving forward and waits
	// for a decision (a failed rollout with no pre-authorized
	// compensation).
	RecoveryLatch *RecoveryLatch `json:"recoveryLatch,omitempty"`
	// Inventory is what the agent applied for ObservedGeneration.
	Inventory []InventoryItem `json:"inventory,omitempty"`
	// InventoryHash is the digest of Inventory.
	InventoryHash *string `json:"inventoryHash,omitempty"`
	// Conditions are left out when empty.
	Conditions Conditions `json:"conditions,omitempty"`
}

// RecoveryLatch records why the agent stopped moving forward.
type RecoveryLatch struct {
	// Generation is the generation that failed.
	Generation int64 `json:"generation"`
	// Reason says why.
	Reason string `json:"reason"`
	// Since is an RFC 3339 timestamp.
	Since string `json:"since"`
}

// InventoryItem is one object of the applied inventory.
type InventoryItem struct {
	// APIVersion is the object's apiVersion.
	APIVersion string `json:"apiVersion"`
	// Kind is the object's kind.
	Kind string `json:"kind"`
	// Name is the object's name.
	Name string `json:"name"`
	// UID is the object's metadata.uid, once known.
	UID *string `json:"uid,omitempty"`
}

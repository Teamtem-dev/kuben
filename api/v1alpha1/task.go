package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ReceiptFinalizer keeps a task, and its receipt, until the hub has
// acknowledged the receipt.
const ReceiptFinalizer = "kuben.dev/receipt"

// ExecutionTask is one attempt of a bounded execution: a build, backup,
// restore, migration or platform change (ADR-027, plan §9.5). It is
// internal: the hub creates it, the cluster agent runs it and writes the
// receipt.
//
// The apiserver enforces with CEL: the attempt's identity, kind, input,
// input hash and deadline never change; a cancel request is a separate
// channel that can be added, never changed or withdrawn; a receipt, once
// written, is final.
//
// The input is canonical JSON in a string so that immutability covers all
// of it (CEL cannot see fields a schema leaves unknown). The receipt stays
// until the hub has durably acknowledged it: the agent removes
// ReceiptFinalizer only then.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=kbet
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Attempt",type="integer",JSONPath=".spec.attempt"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ExecutionTask struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the attempt to run.
	Spec ExecutionTaskSpec `json:"spec"`
	// Status is what the agent observed; nil until it wrote one.
	Status *ExecutionTaskStatus `json:"status,omitempty"`
}

// ExecutionTaskList is a list of ExecutionTask.
//
// +kubebuilder:object:root=true
type ExecutionTaskList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []ExecutionTask `json:"items"`
}

// ExecutionTaskSpec is the attempt to run.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancel) || (has(self.cancel) && self.cancel == oldSelf.cancel)",message="a cancel request can be added, never changed or withdrawn"
type ExecutionTaskSpec struct {
	// Kind is what the attempt runs.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kind is immutable"
	Kind ExecutionKind `json:"kind"`
	// OperationID is the SQL operation this attempt belongs to.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="operationId is immutable"
	OperationID string `json:"operationId"`
	// Attempt numbers the attempts of an operation from 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="attempt is immutable"
	Attempt int64 `json:"attempt"`
	// Input is the kind's input as canonical JSON; never a secret value.
	// +kubebuilder:validation:MaxLength=131072
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="input is immutable"
	Input string `json:"input"`
	// InputHash is the sha256: digest of Input.
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="inputHash is immutable"
	InputHash string `json:"inputHash"`
	// Deadline is an RFC 3339 timestamp; the agent stops the attempt and
	// reports OutcomeTimedOut after it.
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="deadline is immutable"
	Deadline string `json:"deadline"`
	// Cancel asks the agent to stop the attempt.
	Cancel *CancelRequest `json:"cancel,omitempty"`
}

// ExecutionKind is what an attempt runs; each kind has its own input schema
// and permissions. It has no default: the zero value is invalid.
//
// +kubebuilder:validation:Enum=Build;Backup;Restore;Migration;PlatformChange
type ExecutionKind string

// Execution kinds.
const (
	// ExecutionKindBuild builds an image.
	ExecutionKindBuild ExecutionKind = "Build"
	// ExecutionKindBackup backs up a volume or database.
	ExecutionKindBackup ExecutionKind = "Backup"
	// ExecutionKindRestore restores a backup.
	ExecutionKindRestore ExecutionKind = "Restore"
	// ExecutionKindMigration runs a migration.
	ExecutionKindMigration ExecutionKind = "Migration"
	// ExecutionKindPlatformChange changes the platform.
	ExecutionKindPlatformChange ExecutionKind = "PlatformChange"
)

// Valid reports whether k is one of the ExecutionKind constants.
func (k ExecutionKind) Valid() bool {
	switch k {
	case ExecutionKindBuild, ExecutionKindBackup, ExecutionKindRestore,
		ExecutionKindMigration, ExecutionKindPlatformChange:
		return true
	}
	return false
}

// ParseExecutionKind returns the ExecutionKind spelled text.
func ParseExecutionKind(text string) (ExecutionKind, error) {
	return parseEnum(text, "execution kind", ExecutionKind.Valid)
}

// MarshalJSON writes the wire string and refuses the zero value.
func (k ExecutionKind) MarshalJSON() ([]byte, error) {
	return marshalEnum(k, "", "execution kind", ExecutionKind.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (k *ExecutionKind) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, k, "execution kind", ExecutionKind.Valid)
}

// CancelRequest asks the agent to stop an attempt.
type CancelRequest struct {
	// RequestedAt is an RFC 3339 timestamp.
	// +kubebuilder:validation:MaxLength=40
	RequestedAt string `json:"requestedAt"`
	// Reason says why.
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason"`
}

// ExecutionTaskStatus is what the agent observed.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.receipt) || (has(self.receipt) && self.receipt == oldSelf.receipt)",message="a receipt is final"
type ExecutionTaskStatus struct {
	// Phase is Pending, Running, Succeeded, Failed, Cancelled or TimedOut.
	Phase *string `json:"phase,omitempty"`
	// StartedAt is an RFC 3339 timestamp.
	StartedAt *string `json:"startedAt,omitempty"`
	// Receipt is the terminal result, once written.
	Receipt *Receipt `json:"receipt,omitempty"`
	// Conditions are left out when empty.
	Conditions Conditions `json:"conditions,omitempty"`
}

// Receipt is the terminal result of an attempt.
type Receipt struct {
	// Outcome is how the attempt ended.
	Outcome Outcome `json:"outcome"`
	// FinishedAt is an RFC 3339 timestamp.
	// +kubebuilder:validation:MaxLength=40
	FinishedAt string `json:"finishedAt"`
	// Result is the kind's result as canonical JSON (an artifact digest, a
	// backup location, …), bounded like the input.
	// +kubebuilder:validation:MaxLength=16384
	Result *string `json:"result,omitempty"`
	// Message is a human readable explanation.
	// +kubebuilder:validation:MaxLength=1024
	Message *string `json:"message,omitempty"`
}

// Outcome is how an attempt ended. It has no default: the zero value is
// invalid.
//
// +kubebuilder:validation:Enum=Succeeded;Failed;Cancelled;TimedOut
type Outcome string

// Outcomes.
const (
	// OutcomeSucceeded means the attempt did its work.
	OutcomeSucceeded Outcome = "Succeeded"
	// OutcomeFailed means the attempt failed.
	OutcomeFailed Outcome = "Failed"
	// OutcomeCancelled means the attempt stopped on a cancel request.
	OutcomeCancelled Outcome = "Cancelled"
	// OutcomeTimedOut means the attempt passed its deadline.
	OutcomeTimedOut Outcome = "TimedOut"
)

// Valid reports whether o is one of the Outcome constants.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeSucceeded, OutcomeFailed, OutcomeCancelled, OutcomeTimedOut:
		return true
	}
	return false
}

// ParseOutcome returns the Outcome spelled text.
func ParseOutcome(text string) (Outcome, error) {
	return parseEnum(text, "outcome", Outcome.Valid)
}

// MarshalJSON writes the wire string and refuses the zero value.
func (o Outcome) MarshalJSON() ([]byte, error) {
	return marshalEnum(o, "", "outcome", Outcome.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (o *Outcome) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, o, "outcome", Outcome.Valid)
}

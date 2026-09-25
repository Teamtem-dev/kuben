// Package materializer is the materializer (ADR-032; the Rust module
// crates/kuben-platform/src/materializer): SQL is the only desired-state
// writer, and the existing resources are its output.
//
//   - render.go: the Project, Environment and App objects of SQL rows.
//   - fence.go: when a run may write its target's App object.
//   - kube.go: conditional writes, a resourceVersion precondition or a
//     create that fails when the object appeared meanwhile.
//   - progress.go: the App controller's status as run progress.
//   - worker.go: claims operations and carries deployment runs from
//     `planned` to `succeeded` or `failed`.
//   - lifecycle.go: writes projects and environments as soon as they exist,
//     and removes what is being deleted.
//   - secrets.go: the immutable Secrets of the secret revisions a run is
//     bound to (M4.4).
//   - agent.go: delivery through the cluster's agent (M1.9).
//   - detach.go: lets go of an app and leaves its objects running (M4.11).
//   - drift.go: reports changes someone else made to a materialized App
//     object and writes it again.
package materializer

import (
	"fmt"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// FieldManager is the field manager of every write the materializer makes.
const FieldManager = "kuben-materializer"

// Timing of the worker (the Rust constants).
const (
	// Lease is how long a claim is held without renewal.
	Lease = 2 * time.Minute
	// idle is the pause between claims when nothing is due.
	idle = 2 * time.Second
	// poll is the pause between reads while waiting on the cluster.
	poll = 3 * time.Second
	// namespaceWait is how long delivery waits for the environment
	// controller's namespace.
	namespaceWait = time.Minute
	// VerifyDeadline is how long verification waits for the App to become
	// ready, unless the worker is told otherwise.
	VerifyDeadline = 15 * time.Minute
	// approvalRecheck is the longest sleep of a run waiting for approval.
	approvalRecheck = time.Hour
	// pauseRecheck is the longest sleep of a run of a paused target;
	// resuming wakes it at once.
	pauseRecheck = 5 * time.Minute
	// DeletionCheck is how long an environment deletion waits before it
	// checks again, unless the worker is told otherwise.
	DeletionCheck = 20 * time.Second
	// maxAttempts is the claims of one deployment operation before its run
	// fails for good. Lifecycle operations are idempotent and never give
	// up.
	maxAttempts = 20
	// maxConflicts is the conflicting writes tolerated for one object
	// before the attempt is retried later.
	maxConflicts = 5
)

// ErrorCode is the code recorded on an operation while it waits for a
// retry.
type ErrorCode string

// The error codes (worker::Error::code).
const (
	CodeKubernetes       ErrorCode = "KubernetesError"
	CodeStore            ErrorCode = "StoreError"
	CodeContended        ErrorCode = "Contended"
	CodeNamespacePending ErrorCode = "NamespacePending"
	CodeIncomplete       ErrorCode = "IncompleteObject"
	CodeIllegal          ErrorCode = "IllegalTransition"
	CodeUnsupported      ErrorCode = "UnsupportedPhase"
)

// Error is a failure a later attempt may not repeat.
type Error struct {
	Code    ErrorCode
	message string
	err     error
}

func (e *Error) Error() string { return e.message }

func (e *Error) Unwrap() error { return e.err }

func kubeError(err error) *Error {
	return &Error{Code: CodeKubernetes, message: "kubernetes API: " + err.Error(), err: err}
}

func storeError(err error) *Error {
	return &Error{Code: CodeStore, message: "store: " + err.Error(), err: err}
}

func contended(name string) *Error {
	return &Error{Code: CodeContended, message: fmt.Sprintf("`%s` kept changing under concurrent writes", name)}
}

func namespacePending(name string) *Error {
	return &Error{Code: CodeNamespacePending, message: fmt.Sprintf("namespace `%s` does not exist yet", name)}
}

func incomplete(what string) *Error {
	return &Error{
		Code: CodeIncomplete, message: fmt.Sprintf("the API server returned `%s` without a UID or generation", what),
	}
}

func illegal(err error) *Error {
	return &Error{Code: CodeIllegal, message: "the run cannot take the next step: " + err.Error(), err: err}
}

func unsupported(phase run.Phase) *Error {
	return &Error{Code: CodeUnsupported, message: fmt.Sprintf(
		"runs in phase `%s` wait for a later milestone (approval, blocking, cancellation, recovery)", phase)}
}

// stop is why the work on a claim stopped.
//
//sumtype:decl
type stop interface{ isStop() }

type (
	// stopFenced: another worker holds the operation now; drop everything.
	stopFenced struct{}
	// stopShutdown: shutting down; the lease runs out and another worker
	// resumes.
	stopShutdown struct{}
	// stopSettled: the run is settled; finish the operation in this phase.
	stopSettled struct {
		phase run.Phase
		code  opt.Val[string]
	}
	// stopRefused: the run cannot go on; fail it with this code.
	stopRefused struct{ code string }
	// stopSuperseded: a newer run owns the target.
	stopSuperseded struct{}
	// stopRetry: try again later.
	stopRetry struct{ err *Error }
	// stopWait: check again after this long; waiting is not a failure.
	stopWait struct {
		after time.Duration
		code  string
	}
)

func (stopFenced) isStop()     {}
func (stopShutdown) isStop()   {}
func (stopSettled) isStop()    {}
func (stopRefused) isStop()    {}
func (stopSuperseded) isStop() {}
func (stopRetry) isStop()      {}
func (stopWait) isStop()       {}

func refused(code string) stop { return stopRefused{code: code} }

func settled(phase run.Phase, code string) stop {
	if code == "" {
		return stopSettled{phase: phase}
	}
	return stopSettled{phase: phase, code: opt.Some(code)}
}

// retryKube is a Kubernetes API failure to retry.
func retryKube(err error) stop { return stopRetry{err: kubeError(err)} }

// retryStore is a store failure to retry.
func retryStore(err error) stop { return stopRetry{err: storeError(err)} }

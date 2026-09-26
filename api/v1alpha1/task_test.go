package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
)

// Ports the tests of crates/kuben-crd/src/v1alpha1/task.rs.

const taskHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func task() *v1alpha1.ExecutionTask {
	return &v1alpha1.ExecutionTask{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kuben.dev/v1alpha1", Kind: v1alpha1.ExecutionTaskKind},
		ObjectMeta: metav1.ObjectMeta{Name: "build-1"},
		Spec: v1alpha1.ExecutionTaskSpec{
			Kind:        v1alpha1.ExecutionKindBuild,
			OperationID: "0199a0c0-0000-7000-8000-000000000001",
			Attempt:     1,
			Input:       `{"source":"git"}`,
			InputHash:   taskHash,
			Deadline:    "2026-09-15T12:00:00Z",
		},
	}
}

func cancel(reason string) *v1alpha1.CancelRequest {
	return &v1alpha1.CancelRequest{RequestedAt: "2026-09-15T11:00:00Z", Reason: reason}
}

func receipt(outcome v1alpha1.Outcome) *v1alpha1.Receipt {
	return &v1alpha1.Receipt{Outcome: outcome, FinishedAt: "2026-09-15T11:30:00Z"}
}

func phase(p string) *string { return &p }

func TestANewTaskIsValid(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ExecutionTaskKind)
	api.accepts("new", api.create(task()))
	zero := task()
	zero.Spec.Attempt = 0
	api.refuses("attempt 0", api.create(zero))
}

func TestTheInputAndIdentityNeverChange(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ExecutionTaskKind)
	old := task()
	input := task()
	input.Spec.Input = `{"source":"other"}`
	api.refuses("input changed", api.update(input, old))
	attempt := task()
	attempt.Spec.Attempt = 2
	api.refuses("attempt changed", api.update(attempt, old))
	kind := task()
	kind.Spec.Kind = v1alpha1.ExecutionKindBackup
	api.refuses("kind changed", api.update(kind, old))
	deadline := task()
	deadline.Spec.Deadline = "2026-09-16T12:00:00Z"
	api.refuses("deadline moved", api.update(deadline, old))
	api.accepts("the same task again", api.update(task(), old))
}

func TestACancelRequestIsAddedOnceAndKept(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ExecutionTaskKind)
	old := task()
	cancelled := task()
	cancelled.Spec.Cancel = cancel("user")
	api.accepts("cancel added", api.update(cancelled, old))
	changed := task()
	changed.Spec.Cancel = cancel("someone else")
	api.refuses("cancel changed", api.update(changed, cancelled))
	api.refuses("cancel withdrawn", api.update(old, cancelled))
}

func TestAReceiptIsFinal(t *testing.T) {
	api := newAPIServer(t, v1alpha1.ExecutionTaskKind)
	running := task()
	running.Status = &v1alpha1.ExecutionTaskStatus{Phase: phase("Running")}
	done := task()
	done.Status = &v1alpha1.ExecutionTaskStatus{Phase: phase("Succeeded"), Receipt: receipt(v1alpha1.OutcomeSucceeded)}
	api.accepts("receipt written", api.update(done, running))
	rewritten := task()
	rewritten.Status = &v1alpha1.ExecutionTaskStatus{Phase: phase("Failed"), Receipt: receipt(v1alpha1.OutcomeFailed)}
	api.refuses("receipt rewritten", api.update(rewritten, done))
	api.refuses("receipt removed", api.update(running, done))
}

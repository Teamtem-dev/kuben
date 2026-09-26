package build

import (
	"context"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// CreateObjects exposes createObjects to the tests.
func CreateObjects(ctx context.Context, w *Worker, attempt store.BuildAttempt) (string, error) {
	name, err := w.createObjects(ctx, attempt)
	if err != nil {
		return name, err
	}
	return name, nil
}

// Mirror exposes mirror to the tests.
func Mirror(ctx context.Context, w *Worker, attempt store.BuildAttempt, phase opbuild.Phase, digest opt.Val[string]) {
	w.mirror(ctx, attempt, phase, digest)
}

// CrdPhase exposes crdPhase to the tests.
func CrdPhase(phase opbuild.Phase) string { return crdPhase(phase) }

// StatusPatch exposes statusPatch to the tests.
func StatusPatch(attempt store.BuildAttempt, phase opbuild.Phase, digest opt.Val[string], nowMs int64) map[string]any {
	return statusPatch(attempt, phase, digest, nowMs)
}

// Expired exposes expired to the tests.
func Expired(run v1alpha1.BuildRun, nowMs int64) bool { return expired(run, nowMs) }

// OperationPhase exposes operationPhase to the tests.
func OperationPhase(phase opbuild.Phase) string { return operationPhase(phase) }

// IsNotFound exposes isNotFound to the tests.
func IsNotFound(err error) bool { return isNotFound(err) }

// IsConflict exposes isConflict to the tests.
func IsConflict(err error) bool { return isConflict(err) }

// IsInvalid exposes isInvalid to the tests.
func IsInvalid(err error) bool { return isInvalid(err) }

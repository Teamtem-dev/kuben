package ops_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops"
)

func TestIllegalTransitionsSayWhatWasRefused(t *testing.T) {
	for _, tc := range []struct {
		err  ops.IllegalTransitionError
		want string
	}{
		{
			ops.IllegalTransitionError{Machine: "BuildAttempt", From: "queued", Event: "verified"},
			"BuildAttempt: event `verified` is not allowed in phase `queued`",
		},
		{
			ops.IllegalTransitionError{Machine: "DeploymentRun", From: "succeeded", Event: "failed", Terminal: true},
			"DeploymentRun: event `failed` is not allowed in phase `succeeded` (terminal)",
		},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %s", got)
		}
	}
}

package materializer_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/materializer"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// observed is an App whose controller observed generation, with a Ready
// condition when reason is not empty.
func observed(generation int64, ready bool, reason string) *v1alpha1.App {
	status := &v1alpha1.AppStatus{ObservedGeneration: &generation}
	if reason != "" {
		c := v1alpha1.NewCondition(v1alpha1.ConditionReady, ready, reason)
		c.ObservedGeneration = &generation
		status.Conditions = v1alpha1.Conditions{c}
	}
	return &v1alpha1.App{Status: status}
}

func TestFollowsTheWrittenGeneration(t *testing.T) {
	cases := []struct {
		app  *v1alpha1.App
		want materializer.Progress
		why  string
	}{
		{&v1alpha1.App{}, materializer.ProgressPending{}, "no status"},
		{observed(3, true, "Available"), materializer.ProgressPending{}, "ready, but for an older generation"},
		{observed(4, false, ""), materializer.ProgressApplied{}, "no Ready condition"},
		{observed(4, false, "Progressing"), materializer.ProgressApplied{}, "progressing"},
		{observed(4, true, "Available"), materializer.ProgressReady{}, "ready"},
		{observed(5, true, "Scheduled"), materializer.ProgressReady{}, "a later generation observed"},
	}
	for _, c := range cases {
		if got := materializer.ProgressOf(c.app, 4); got != c.want {
			t.Errorf("%s: %#v", c.why, got)
		}
	}
}

func TestPermanentReasonsFailTheRun(t *testing.T) {
	for _, reason := range []string{"RolloutFailed", "UnknownSize"} {
		if got := materializer.ProgressOf(observed(4, false, reason), 4); got != (materializer.ProgressFailed{Reason: reason}) {
			t.Errorf("%s: %#v", reason, got)
		}
	}
}

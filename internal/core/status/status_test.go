package status_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/status"
)

func TestComponentsAreJudgedByWhatWasObserved(t *testing.T) {
	ready := status.Observed{Ready: opt.Some(true), Pods: 2, ReadyPods: 2}
	with := func(edit func(*status.Observed)) status.Observed {
		o := ready
		edit(&o)
		return o
	}
	cases := []struct {
		name string
		in   status.Observed
		want status.Service
	}{
		{"ready", ready, status.Operational},
		{"half", with(func(o *status.Observed) { o.ReadyPods = 1 }), status.Degraded},
		{"down", with(func(o *status.Observed) { o.Ready = opt.Some(false); o.ReadyPods = 0 }), status.MajorOutage},
		{"gone", with(func(o *status.Observed) { o.Ready = opt.Some(false); o.Pods, o.ReadyPods = 0, 0 }), status.MajorOutage},
		{"failing", with(func(o *status.Observed) { o.CriticalIncident = true }), status.Degraded},
		{"nothing observed", status.Observed{}, status.Degraded},
	}
	for _, c := range cases {
		if got := status.Component(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestAPageIsItsWorstComponent(t *testing.T) {
	if got := status.Page(nil); got != status.Operational {
		t.Errorf("got %s", got)
	}
	if got := status.Page([]status.Service{status.Operational, status.Degraded}); got != status.Degraded {
		t.Errorf("got %s", got)
	}
	if got := status.Page([]status.Service{status.MajorOutage, status.Degraded}); got != status.MajorOutage {
		t.Errorf("got %s", got)
	}
}

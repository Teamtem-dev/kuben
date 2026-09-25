package materializer

// What the App controller's status says about a written generation.
//
// Until M1.9 the existing controllers carry a run out: the App controller
// reconciles the App object into workloads and reports
// `status.observedGeneration` and a `Ready` condition. The materializer
// maps that onto the run's phases.

import (
	"slices"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// failedReasons are the App controller's `Ready` reasons no wait will
// change: a rollout past its progress deadline, or a spec it cannot build.
var failedReasons = []string{
	"RolloutFailed",
	string(render.ReasonNoProcesses),
	string(render.ReasonMultiplePorts),
	string(render.ReasonUnknownSize),
	string(render.ReasonAwaitingBuild),
	string(render.ReasonInvalidSource),
	string(render.ReasonVolumeNeedsSingleReplica),
	string(render.ReasonScheduledWithPort),
	string(render.ReasonInvalidSchedule),
}

// Progress is how far the controller got with the written
// metadata.generation.
//
//sumtype:decl
type Progress interface{ isProgress() }

type (
	// ProgressPending means the controller has not reconciled the written
	// generation yet.
	ProgressPending struct{}
	// ProgressApplied means it reconciled it and its workloads are rolling out.
	ProgressApplied struct{}
	// ProgressReady means every workload of the written generation is
	// available.
	ProgressReady struct{}
	// ProgressFailed means it will not become ready without a new generation.
	ProgressFailed struct{ Reason string }
)

func (ProgressPending) isProgress() {}
func (ProgressApplied) isProgress() {}
func (ProgressReady) isProgress()   {}
func (ProgressFailed) isProgress()  {}

// ProgressOf is the progress of app for its metadata.generation written.
func ProgressOf(app *v1alpha1.App, written int64) Progress {
	status := app.Status
	if status == nil {
		return ProgressPending{}
	}
	observed := int64(0)
	if status.ObservedGeneration != nil {
		observed = *status.ObservedGeneration
	}
	if observed < written {
		return ProgressPending{}
	}
	i := slices.IndexFunc(status.Conditions, func(c v1alpha1.Condition) bool { return c.Type == v1alpha1.ConditionReady })
	if i < 0 {
		return ProgressApplied{}
	}
	ready := status.Conditions[i]
	if g := ready.ObservedGeneration; g != nil && *g < written {
		return ProgressApplied{}
	}
	switch {
	case ready.Status == "True":
		return ProgressReady{}
	case ready.Status == "False" && ready.Reason != nil && slices.Contains(failedReasons, *ready.Reason):
		return ProgressFailed{Reason: *ready.Reason}
	}
	return ProgressApplied{}
}

package build

// The stages of a build (2.1), as the console's timeline shows them:
//
//	clone   the fetch container
//	plan    the plan container
//	build   the BuildKit container (it pushes, too)
//	scan    the scan container; skipped without a scanner
//	push    the registry holds the reported digest (the worker verifies it)
//	deploy  the release is deployed, or kept by the target's policy
//
// The pod's container states give the first four, with the times the
// kubelet recorded; the worker sets push and deploy, and closes every
// stage when the attempt settles. Recording stages is best effort: a build
// never waits or fails for its timeline.

import (
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"

	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// stageOrder is the order of stages.
var stageOrder = []store.BuildStageName{ //nolint:gochecknoglobals // a constant list
	store.StageClone, store.StagePlan, store.StageBuild, store.StageScan, store.StagePush, store.StageDeploy,
}

// containerStages are the stages the pod's containers are.
var containerStages = map[string]store.BuildStageName{ //nolint:gochecknoglobals // a constant table
	outcome.FetchContainer: store.StageClone,
	PlanContainer:          store.StagePlan,
	outcome.BuildContainer: store.StageBuild,
	outcome.ScanContainer:  store.StageScan,
}

// maxStageDetail is the most of a container's message a stage keeps.
const maxStageDetail = 512

// InitialStages are the stages of a build that has not started: all
// pending, the scan skipped without a scanner.
func InitialStages(scanning bool) []store.BuildStage {
	out := make([]store.BuildStage, 0, len(stageOrder))
	for _, name := range stageOrder {
		s := store.BuildStage{Name: name, Status: store.StagePending}
		if name == store.StageScan && !scanning {
			s.Status = store.StageSkipped
			s.Detail = opt.Some("scanning is not configured")
		}
		out = append(out, s)
	}
	return out
}

// PodStages are the stages pod's containers show.
func PodStages(pod *corev1.Pod) []store.BuildStage {
	out := make([]store.BuildStage, 0, len(containerStages))
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...),
		pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		name, ok := containerStages[status.Name]
		if !ok {
			continue
		}
		out = append(out, containerStage(name, status.State))
	}
	return out
}

// containerStage is the stage name of a container in state.
func containerStage(name store.BuildStageName, state corev1.ContainerState) store.BuildStage {
	s := store.BuildStage{Name: name, Status: store.StagePending}
	switch {
	case state.Terminated != nil:
		t := state.Terminated
		s.StartedAt, s.FinishedAt = millis(t.StartedAt.UnixMilli(), !t.StartedAt.IsZero()),
			millis(t.FinishedAt.UnixMilli(), !t.FinishedAt.IsZero())
		s.Status = store.StageSucceeded
		if t.ExitCode != 0 {
			s.Status = store.StageFailed
			s.Detail = detail(t.Reason, t.Message)
		}
	case state.Running != nil:
		s.Status = store.StageRunning
		s.StartedAt = millis(state.Running.StartedAt.UnixMilli(), !state.Running.StartedAt.IsZero())
	case state.Waiting != nil:
		switch state.Waiting.Reason {
		case "", "PodInitializing", "ContainerCreating":
		default:
			// ErrImagePull, CreateContainerConfigError, …: why it waits.
			s.Detail = detail(state.Waiting.Reason, state.Waiting.Message)
		}
	}
	return s
}

func millis(ms int64, ok bool) opt.Val[int64] {
	if !ok {
		return opt.None[int64]()
	}
	return opt.Some(ms)
}

// detail is `reason: message`, either alone when the other is empty.
func detail(reason, message string) opt.Val[string] {
	text := reason
	switch {
	case reason == "":
		text = message
	case message != "":
		text = reason + ": " + message
	}
	if text == "" {
		return opt.None[string]()
	}
	return opt.Some(firstChars(text, maxStageDetail))
}

// MergeStages is previous (all stages, in order) with updates applied: an
// update replaces its stage, except that a pending update leaves a stage
// that started (its container is gone) as it was.
func MergeStages(previous []store.BuildStage, updates ...store.BuildStage) []store.BuildStage {
	byName := make(map[store.BuildStageName]store.BuildStage, len(stageOrder))
	for _, s := range InitialStages(true) {
		byName[s.Name] = s
	}
	for _, s := range previous {
		byName[s.Name] = s
	}
	for _, u := range updates {
		if u.Status == store.StagePending && byName[u.Name].Status != store.StagePending {
			continue
		}
		byName[u.Name] = u
	}
	out := make([]store.BuildStage, 0, len(stageOrder))
	for _, name := range stageOrder {
		out = append(out, byName[name])
	}
	return out
}

// PushRunning marks the push as being verified, from nowMs.
func PushRunning(stages []store.BuildStage, nowMs int64) []store.BuildStage {
	for _, s := range stages {
		if s.Name == store.StagePush && s.Status != store.StagePending {
			return stages
		}
	}
	return MergeStages(stages, store.BuildStage{Name: store.StagePush, Status: store.StageRunning, StartedAt: opt.Some(nowMs)})
}

// FinalStages closes every stage of a build that ended in phase with code
// (the failure, or the decision of a kept build) at nowMs: nothing stays
// pending or running.
func FinalStages(stages []store.BuildStage, phase opbuild.Phase, code opt.Val[string], nowMs int64) []store.BuildStage {
	out := MergeStages(stages)
	closeAs := func(s *store.BuildStage, status store.BuildStageStatus, why opt.Val[string]) {
		if s.StartedAt.IsNone() && status != store.StageSkipped {
			s.StartedAt = opt.Some(nowMs)
		}
		if status != store.StageSkipped {
			s.FinishedAt = opt.Some(nowMs)
		}
		s.Status = status
		if why.IsSome() {
			s.Detail = why
		}
	}
	switch phase {
	case opbuild.Succeeded:
		for i := range out {
			s := &out[i]
			switch {
			case s.Name == store.StageDeploy && code.IsSome():
				closeAs(s, store.StageSkipped, code)
			case s.Status == store.StagePending || s.Status == store.StageRunning:
				closeAs(s, store.StageSucceeded, opt.None[string]())
			}
		}
		return out
	case opbuild.Cancelled:
		closeRest(out, closeAs, opt.Some("cancelled"))
		return out
	case opbuild.Failed:
		if code.Or("") == string(outcome.OutputRejected) {
			for i := range out {
				if out[i].Name == store.StagePush {
					closeAs(&out[i], store.StageFailed, code)
				}
			}
		}
		closeRest(out, closeAs, code)
		if !slices.ContainsFunc(out, func(s store.BuildStage) bool { return s.Status == store.StageFailed }) {
			// It failed before any stage ran (no token, no Job): the
			// first stage that did not succeed carries the failure.
			for i := range out {
				if out[i].Status != store.StageSucceeded {
					closeAs(&out[i], store.StageFailed, code)
					break
				}
			}
		}
		return out
	case opbuild.Queued, opbuild.Blocked, opbuild.Preparing, opbuild.Running, opbuild.Publishing,
		opbuild.VerifyingOutput, opbuild.CancelRequested, opbuild.Cancelling:
	}
	return out
}

// closeRest fails the running stages with why and skips the pending ones.
func closeRest(stages []store.BuildStage, closeAs func(*store.BuildStage, store.BuildStageStatus, opt.Val[string]), why opt.Val[string]) {
	for i := range stages {
		s := &stages[i]
		switch s.Status {
		case store.StageRunning:
			closeAs(s, store.StageFailed, why)
		case store.StagePending:
			closeAs(s, store.StageSkipped, opt.None[string]())
		case store.StageSucceeded, store.StageFailed, store.StageSkipped:
		}
	}
}

// stagesOf are attempt's recorded stages, or the initial ones.
func (w *Worker) stagesOf(attempt store.BuildAttempt) []store.BuildStage {
	if len(attempt.Stages) == 0 {
		return InitialStages(w.d.Settings.ScannerImage.IsSome())
	}
	return MergeStages(attempt.Stages)
}

// keepStages records stages for attempt when they differ from those it
// has; best effort.
func (w *Worker) keepStages(ctx context.Context, attempt store.BuildAttempt, stages []store.BuildStage) {
	if slices.Equal(attempt.Stages, stages) {
		return
	}
	t, err := w.d.Store.Tenant(ctx, attempt.Org)
	if err == nil {
		defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		if _, err = t.SetBuildStages(ctx, attempt.ID, stages); err == nil {
			err = t.Commit(ctx)
		}
	}
	if err != nil {
		w.d.Logger.Warn("the stages of a build were not recorded", "build", attempt.ID.String(), "error", err)
	}
}

// observeStages records the stages attempt's pod shows while it runs, and
// the push as verified next when plan verifies it.
func (w *Worker) observeStages(ctx context.Context, attempt store.BuildAttempt, pod opt.Val[*corev1.Pod], plan Plan) {
	stages := w.stagesOf(attempt)
	if p, ok := pod.Get(); ok {
		stages = MergeStages(stages, PodStages(p)...)
	}
	if _, verify := plan.(PlanVerify); verify {
		stages = PushRunning(stages, w.d.Clock.NowMs())
	}
	w.keepStages(ctx, attempt, stages)
}

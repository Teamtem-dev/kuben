package build

// Kubernetes Job and Pod status → outcome.JobObservation, without I/O
// (observe.rs).

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

func exits(statuses []corev1.ContainerStatus) ([]outcome.ContainerExit, []string) {
	ended := []outcome.ContainerExit{}
	running := []string{}
	for _, status := range statuses {
		if t := status.State.Terminated; t != nil {
			ended = append(ended, outcome.ContainerExit{
				Name:     status.Name,
				ExitCode: t.ExitCode,
				Reason:   nonEmpty(t.Reason),
				Message:  nonEmpty(t.Message),
			})
		} else if status.State.Running != nil {
			running = append(running, status.Name)
		}
	}
	return ended, running
}

// nonEmpty is a status text: Go's API types leave an absent one empty,
// where Rust's had None.
func nonEmpty(s string) opt.Val[string] {
	if s == "" {
		return opt.None[string]()
	}
	return opt.Some(s)
}

// ObservePod is what pod shows at nowMs (unix milliseconds).
func ObservePod(pod *corev1.Pod, nowMs int64) outcome.PodObservation {
	status := pod.Status
	all, running := exits(status.InitContainerStatuses)
	mainExits, mainRunning := exits(status.ContainerStatuses)
	all = append(all, mainExits...)
	running = append(running, mainRunning...)
	unschedulable := opt.None[uint64]()
	for _, c := range status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		secs := uint64(0)
		if !c.LastTransitionTime.IsZero() {
			if d := nowMs/1000 - c.LastTransitionTime.Unix(); d > 0 {
				secs = uint64(d)
			}
		}
		unschedulable = opt.Some(secs)
		break
	}
	return outcome.PodObservation{
		Phase:             string(status.Phase),
		Reason:            nonEmpty(status.Reason),
		Message:           nonEmpty(status.Message),
		Exits:             all,
		Running:           running,
		UnschedulableSecs: unschedulable,
	}
}

// ObserveJob is what job and its newest pod show at nowMs; a Job that does
// not exist when job is none.
func ObserveJob(job opt.Val[*batchv1.Job], pods []corev1.Pod, nowMs int64) outcome.JobObservation {
	j, ok := job.Get()
	if !ok || j == nil {
		return outcome.JobObservation{}
	}
	condition := func(kind batchv1.JobConditionType) (batchv1.JobCondition, bool) {
		for _, c := range j.Status.Conditions {
			if c.Type == kind && c.Status == corev1.ConditionTrue {
				return c, true
			}
		}
		return batchv1.JobCondition{}, false
	}
	failed, isFailed := condition(batchv1.JobFailed)
	_, complete := condition(batchv1.JobComplete)
	seen := outcome.JobObservation{
		Exists:    true,
		Succeeded: complete || j.Status.Succeeded > 0,
		Failed:    isFailed,
	}
	if isFailed {
		seen.FailedReason = nonEmpty(failed.Reason)
	}
	if newest, ok := newestPod(pods).Get(); ok {
		seen.Pod = opt.Some(ObservePod(newest, nowMs))
	}
	return seen
}

// newestPod is the pod created last; of equals the last one, as Rust's
// max_by_key chose (a pod without a creation time is the oldest).
func newestPod(pods []corev1.Pod) opt.Val[*corev1.Pod] {
	newest := opt.None[*corev1.Pod]()
	for i := range pods {
		p := &pods[i]
		if n, ok := newest.Get(); ok && p.CreationTimestamp.Before(&n.CreationTimestamp) {
			continue
		}
		newest = opt.Some(p)
	}
	return newest
}

package build

// The log a build leaves (2.1): when an attempt settles, before its Job and
// pod are deleted, the end of the fetch, plan and build containers' logs is
// kept with the attempt (store.SetBuildLogTail), so `…/builds/{build}/logs`
// still answers after the pod is gone. The scan container's log is the
// image's SBOM (evidence.go), kept with the scan instead.

import (
	"context"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const (
	// DefaultLogTailBytes is how much of a build's log is kept unless the
	// settings say otherwise.
	DefaultLogTailBytes = 256 << 10
	// logTailLines is how many lines of each container are read.
	logTailLines = 5000
)

// loggedContainers are the containers whose logs a build keeps, in order,
// with the stage each is.
var loggedContainers = []struct { //nolint:gochecknoglobals // a constant table
	container string
	stage     store.BuildStageName
}{
	{outcome.FetchContainer, store.StageClone},
	{PlanContainer, store.StagePlan},
	{outcome.BuildContainer, store.StageBuild},
}

// logTailBytes is the most of a log the worker keeps.
func (w *Worker) logTailBytes() int {
	n := w.d.Settings.LogTailBytes
	if n <= 0 {
		n = DefaultLogTailBytes
	}
	return min(n, store.MaxBuildLogTail)
}

// LogOf is the log a build keeps from the logs of its containers (in the
// order of loggedContainers, those that ran): each under a header naming
// its stage, the whole cut to its last maxBytes on a character boundary.
func LogOf(parts map[string]string, maxBytes int) string {
	var b strings.Builder
	for _, c := range loggedContainers {
		text, ok := parts[c.container]
		if !ok {
			continue
		}
		b.WriteString("==> " + string(c.stage) + " <==\n")
		b.WriteString(strings.ToValidUTF8(text, "�"))
		if text != "" && !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	return tail(b.String(), maxBytes)
}

// tail is the last maxBytes of s at most, starting on a character.
func tail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// startedContainers are the names of pod's containers that started.
func startedContainers(pod *corev1.Pod) map[string]bool {
	started := map[string]bool{}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, s := range statuses {
			if s.State.Running != nil || s.State.Terminated != nil {
				started[s.Name] = true
			}
		}
	}
	return started
}

// newestBuildPod is attempt's newest pod; none when it has none (any more).
func (w *Worker) newestBuildPod(ctx context.Context, attempt store.BuildAttempt) (opt.Val[*corev1.Pod], error) {
	listed, err := w.d.Client.CoreV1().Pods(w.d.Settings.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: AttemptLabel + "=" + attempt.ID.String(),
	})
	if err != nil {
		return opt.None[*corev1.Pod](), err //nolint:wrapcheck // a Kubernetes API error, logged by the caller
	}
	return newestPod(listed.Items), nil
}

// podLog reads the logs of pod's containers that ran.
func (w *Worker) podLog(ctx context.Context, pod *corev1.Pod) map[string]string {
	pods := w.d.Client.CoreV1().Pods(w.d.Settings.Namespace)
	started := startedContainers(pod)
	lines := int64(logTailLines)
	limit := int64(w.logTailBytes())
	parts := map[string]string{}
	for _, c := range loggedContainers {
		if !started[c.container] {
			continue
		}
		raw, err := pods.GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: c.container, TailLines: &lines, LimitBytes: &limit,
		}).DoRaw(ctx)
		if err != nil {
			w.d.Logger.Debug("a build container's log could not be read", "pod", pod.Name, "container", c.container,
				"error", err)
			continue
		}
		parts[c.container] = string(raw)
	}
	return parts
}

// settleRecords closes the stages of attempt, final in phase with code, and
// keeps its log, before its pod is deleted; best effort.
func (w *Worker) settleRecords(ctx context.Context, attempt store.BuildAttempt, phase opbuild.Phase, code opt.Val[string]) {
	pod, err := w.newestBuildPod(ctx, attempt)
	if err != nil {
		w.d.Logger.Warn("the build pod could not be read for its log", "build", attempt.ID.String(), "error", err)
	}
	stages := w.stagesOf(attempt)
	log := ""
	if p, ok := pod.Get(); ok {
		stages = MergeStages(stages, PodStages(p)...)
		log = LogOf(w.podLog(ctx, p), w.logTailBytes())
	}
	w.keepStages(ctx, attempt, FinalStages(stages, phase, code, w.d.Clock.NowMs()))
	if log == "" {
		// The pod is gone (a repeated cleanup): the kept log stays.
		return
	}
	t, err := w.d.Store.Tenant(ctx, attempt.Org)
	if err == nil {
		defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		if _, err = t.SetBuildLogTail(ctx, attempt.ID, log); err == nil {
			err = t.Commit(ctx)
		}
	}
	if err != nil {
		w.d.Logger.Warn("the log of a build was not kept", "build", attempt.ID.String(), "error", err)
	}
}

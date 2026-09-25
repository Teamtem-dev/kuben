package build_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

// Ported from observe.rs.

func parse[T any](t *testing.T, text string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ms(t *testing.T, text string) int64 {
	t.Helper()
	return check(time.Parse(time.RFC3339, text)).must(t).UnixMilli()
}

func TestAMissingJobDoesNotExist(t *testing.T) {
	if build.ObserveJob(opt.None[*batchv1.Job](), nil, 0).Exists {
		t.Error("a missing Job exists")
	}
}

func TestStatusesBecomeObservations(t *testing.T) {
	j := parse[batchv1.Job](t, `{
		"metadata": {"name": "kbuild-1"},
		"status": {"conditions": [{"type": "Failed", "status": "True", "reason": "DeadlineExceeded"}]}}`)
	old := parse[corev1.Pod](t, `{
		"metadata": {"name": "old", "creationTimestamp": "2026-09-17T10:00:00Z"},
		"status": {"phase": "Failed"}}`)
	newer := parse[corev1.Pod](t, `{
		"metadata": {"name": "new", "creationTimestamp": "2026-09-17T10:05:00Z"},
		"status": {
			"phase": "Running",
			"initContainerStatuses": [{"name": "fetch", "image": "i", "imageID": "", "ready": false, "restartCount": 0,
				"state": {"terminated": {"exitCode": 0, "reason": "Completed"}}}],
			"containerStatuses": [{"name": "build", "image": "i", "imageID": "", "ready": true, "restartCount": 0,
				"state": {"running": {}}}]}}`)
	seen := build.ObserveJob(opt.Some(&j), []corev1.Pod{old, newer}, time.Now().UnixMilli())
	if !seen.Exists || !seen.Failed || seen.Succeeded {
		t.Errorf("seen %+v", seen)
	}
	if seen.FailedReason != opt.Some("DeadlineExceeded") {
		t.Errorf("reason %v", seen.FailedReason)
	}
	pod, ok := seen.Pod.Get()
	if !ok || pod.Phase != "Running" {
		t.Fatalf("the newest pod: %+v", pod)
	}
	if len(pod.Exits) != 1 || pod.Exits[0].Name != "fetch" {
		t.Errorf("exits %+v", pod.Exits)
	}
	if diff := cmp.Diff([]string{"build"}, pod.Running); diff != "" {
		t.Errorf("running (-want +got):\n%s", diff)
	}
}

func TestUnschedulableTimeIsMeasured(t *testing.T) {
	p := parse[corev1.Pod](t, `{
		"metadata": {"name": "p"},
		"status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False",
			"reason": "Unschedulable", "lastTransitionTime": "2026-09-17T10:00:00Z", "message": "0/1 nodes are available"}]}}`)
	now := ms(t, "2026-09-17T10:07:00Z")
	if got := build.ObservePod(&p, now).UnschedulableSecs; got != opt.Some[uint64](420) {
		t.Errorf("unschedulable %v", got)
	}
	scheduled := parse[corev1.Pod](t, `{"metadata": {"name": "p"}, "status": {"phase": "Pending"}}`)
	if got := build.ObservePod(&scheduled, now).UnschedulableSecs; got.IsSome() {
		t.Errorf("scheduled %v", got)
	}
}

func TestOomKillsKeepTheirReason(t *testing.T) {
	p := parse[corev1.Pod](t, `{
		"metadata": {"name": "p"},
		"status": {"phase": "Failed", "containerStatuses": [{"name": "build", "image": "i", "imageID": "",
			"ready": false, "restartCount": 0, "state": {"terminated": {"exitCode": 137, "reason": "OOMKilled"}}}]}}`)
	seen := build.ObservePod(&p, time.Now().UnixMilli())
	if seen.Exits[0].Reason != opt.Some("OOMKilled") || seen.Exits[0].ExitCode != 137 {
		t.Errorf("exit %+v", seen.Exits[0])
	}
}

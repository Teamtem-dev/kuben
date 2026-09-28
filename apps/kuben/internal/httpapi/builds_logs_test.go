package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
)

// buildPod is attempt's pod in the build namespace, its fetch and plan
// containers done and its build container running unless done.
func buildPod(attempt ids.BuildAttemptID, done bool) *corev1.Pod {
	terminated := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	building := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	phase := corev1.PodRunning
	if done {
		building, phase = terminated, corev1.PodSucceeded
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kbuild-1-abcde", Namespace: "kuben-builds",
			Labels: map[string]string{build.AttemptLabel: attempt.String()},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "fetch", State: terminated}, {Name: "plan", State: terminated},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "build", State: building}},
		},
	}
}

// The fake clientset answers every log read with "fake logs".
func TestABuildPodsLogIsReadLikeTheKeptTail(t *testing.T) {
	attempt := ids.New[ids.BuildAttempt]()
	pods := fake.NewClientset(buildPod(attempt, false)).CoreV1().Pods("kuben-builds")
	got, err := httpapi.ReadBuildLog(t.Context(), httpapi.BuildLogOf{Pods: pods, Attempt: attempt, Tail: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	want := "==> clone <==\nfake logs\n==> plan <==\nfake logs\n==> build <==\nfake logs\n"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
	other := ids.New[ids.BuildAttempt]()
	got, err = httpapi.ReadBuildLog(t.Context(), httpapi.BuildLogOf{Pods: pods, Attempt: other, Tail: "kept"})
	if err != nil || got != "kept" {
		t.Errorf("another build's pod is not this one's: %q %v", got, err)
	}
	got, err = httpapi.ReadBuildLog(t.Context(), httpapi.BuildLogOf{Attempt: attempt, Settled: true, Tail: "kept"})
	if err != nil || got != "kept" {
		t.Errorf("without a cluster: %q %v", got, err)
	}
}

func TestAFollowedBuildLogFollowsEachContainerThenEnds(t *testing.T) {
	attempt := ids.New[ids.BuildAttempt]()
	pods := fake.NewClientset(buildPod(attempt, true)).CoreV1().Pods("kuben-builds")
	var out strings.Builder
	if err := httpapi.FollowBuildLog(t.Context(), &out, httpapi.BuildLogOf{Pods: pods, Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, stage := range []string{"clone", "plan", "build"} {
		want.WriteString(`event: line` + "\n" + `data: {"pod":"kbuild-1-abcde","process":"` + stage +
			`","time":null,"line":"fake logs"}` + "\n\n")
	}
	want.WriteString("event: end\ndata: {\"pod\":\"kbuild-1-abcde\",\"error\":null}\n\n")
	if diff := cmp.Diff(want.String(), out.String()); diff != "" {
		t.Error(diff)
	}
}

func TestAFollowedBuildWithoutItsPodSendsTheKeptTail(t *testing.T) {
	attempt := ids.New[ids.BuildAttempt]()
	var out strings.Builder
	err := httpapi.FollowBuildLog(t.Context(), &out, httpapi.BuildLogOf{
		Pods: fake.NewClientset().CoreV1().Pods("kuben-builds"), Attempt: attempt, Job: "kbuild-1",
		Settled: true, Tail: "==> clone <==\ncloned\n==> build <==\n2026-09-28T10:00:00Z built\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "event: line\ndata: {\"pod\":\"kbuild-1\",\"process\":\"clone\",\"time\":null,\"line\":\"cloned\"}\n\n" +
		"event: line\ndata: {\"pod\":\"kbuild-1\",\"process\":\"build\",\"time\":null,\"line\":\"2026-09-28T10:00:00Z built\"}\n\n" +
		"event: end\ndata: {\"pod\":null,\"error\":null}\n\n"
	if diff := cmp.Diff(want, out.String()); diff != "" {
		t.Error(diff)
	}
}

func TestFollowedBuildLogsOutliveTheRequestTimeout(t *testing.T) {
	s, err := httpapi.New(httpapi.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/p/environments/e/apps/a/builds/b/logs"
	for query, want := range map[string]bool{"?follow=true": true, "?follow=false": false, "": false} {
		r, err := http.NewRequest(http.MethodGet, path+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := httpapi.FollowsLogs(s, r); got != want {
			t.Errorf("%q: %v", query, got)
		}
	}
}

// An app without a Git source has nothing to build, and no build logs.
func TestAppsWithoutAGitSourceHaveNoBuildsToTriggerOrRead(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	builds := "/api/v1/projects/shop/environments/prod/apps/api/builds"
	if status := bob.status("POST", builds, nil); status != http.StatusForbidden {
		t.Fatalf("a viewer triggers: %d", status)
	}
	if status := alice.status("POST", builds, nil); status != http.StatusConflict {
		t.Fatalf("no Git source: %d", status)
	}
	for _, build := range []string{"nope", ids.New[ids.BuildAttempt]().String()} {
		if status := alice.status("GET", builds+"/"+build+"/logs", nil); status != http.StatusNotFound {
			t.Errorf("build %s: %d", build, status)
		}
	}
}

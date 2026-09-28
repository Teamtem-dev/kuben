package build_test

// The stages and the kept log of a build (2.1), and sources read through a
// Git connection, without a database.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// stage is a stage for comparisons.
func stage(name store.BuildStageName, status store.BuildStageStatus, times ...int64) store.BuildStage {
	s := store.BuildStage{Name: name, Status: status}
	if len(times) > 0 {
		s.StartedAt = opt.Some(times[0])
	}
	if len(times) > 1 {
		s.FinishedAt = opt.Some(times[1])
	}
	return s
}

func withDetail(s store.BuildStage, detail string) store.BuildStage {
	s.Detail = opt.Some(detail)
	return s
}

var stageCmp = cmp.AllowUnexported(opt.Val[int64]{}, opt.Val[string]{})

func kubeTime(ms int64) metav1.Time { return metav1.NewTime(time.UnixMilli(ms)) }

// buildPod is a pod whose fetch and plan ran, and whose build is in state.
func buildPod(build corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kbuild-x-abcde"},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: outcome.FetchContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, StartedAt: kubeTime(1000), FinishedAt: kubeTime(2000),
				}}},
				{Name: "plan", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, StartedAt: kubeTime(2000), FinishedAt: kubeTime(3000),
				}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: outcome.BuildContainer, State: build}},
		},
	}
}

func TestABuildStartsWithPendingStages(t *testing.T) {
	want := []store.BuildStage{
		stage(store.StageClone, store.StagePending), stage(store.StagePlan, store.StagePending),
		stage(store.StageBuild, store.StagePending),
		withDetail(stage(store.StageScan, store.StageSkipped), "scanning is not configured"),
		stage(store.StagePush, store.StagePending), stage(store.StageDeploy, store.StagePending),
	}
	if diff := cmp.Diff(want, build.InitialStages(false), stageCmp); diff != "" {
		t.Error(diff)
	}
	if got := build.InitialStages(true); got[3].Status != store.StagePending {
		t.Errorf("with a scanner: %+v", got[3])
	}
}

func TestThePodShowsTheStagesItRan(t *testing.T) {
	running := buildPod(corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: kubeTime(3000)}})
	stages := build.MergeStages(build.InitialStages(false), build.PodStages(running)...)
	want := []store.BuildStage{
		stage(store.StageClone, store.StageSucceeded, 1000, 2000), stage(store.StagePlan, store.StageSucceeded, 2000, 3000),
		stage(store.StageBuild, store.StageRunning, 3000),
		withDetail(stage(store.StageScan, store.StageSkipped), "scanning is not configured"),
		stage(store.StagePush, store.StagePending), stage(store.StageDeploy, store.StagePending),
	}
	if diff := cmp.Diff(want, stages, stageCmp); diff != "" {
		t.Error(diff)
	}

	// A pod that is gone (pending again) does not undo what was seen.
	gone := build.MergeStages(stages, stage(store.StageBuild, store.StagePending), stage(store.StageClone, store.StagePending))
	if diff := cmp.Diff(stages, gone, stageCmp); diff != "" {
		t.Errorf("gone: %s", diff)
	}

	failed := buildPod(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 2, Reason: "Error", Message: strings.Repeat("x", 600), StartedAt: kubeTime(3000), FinishedAt: kubeTime(9000),
	}})
	build3 := build.PodStages(failed)[2]
	if build3.Status != store.StageFailed || build3.FinishedAt != opt.Some(int64(9000)) ||
		!strings.HasPrefix(build3.Detail.Or(""), "Error: xxx") || len(build3.Detail.Or("")) > 512 {
		t.Errorf("a failed build: %+v", build3)
	}

	waiting := buildPod(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "no such image"}})
	if got := build.PodStages(waiting)[2]; got.Status != store.StagePending || got.Detail.Or("") != "ErrImagePull: no such image" {
		t.Errorf("waiting for its image: %+v", got)
	}
	initializing := buildPod(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})
	if got := build.PodStages(initializing)[2]; got.Detail.IsSome() {
		t.Errorf("initializing: %+v", got)
	}
}

func TestSettledBuildsCloseEveryStage(t *testing.T) {
	const now = 10_000
	done := buildPod(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: kubeTime(3000), FinishedAt: kubeTime(8000)}})
	ran := build.PushRunning(build.MergeStages(build.InitialStages(false), build.PodStages(done)...), 8500)
	if ran[4].Status != store.StageRunning || ran[4].StartedAt != opt.Some(int64(8500)) {
		t.Fatalf("push %+v", ran[4])
	}
	if again := build.PushRunning(ran, 9000); again[4].StartedAt != opt.Some(int64(8500)) {
		t.Errorf("the push started once: %+v", again[4])
	}

	deployed := build.FinalStages(ran, opbuild.Succeeded, opt.None[string](), now)
	if deployed[4] != stage(store.StagePush, store.StageSucceeded, 8500, now) ||
		deployed[5] != stage(store.StageDeploy, store.StageSucceeded, now, now) {
		t.Errorf("deployed: %+v", deployed[4:])
	}
	kept := build.FinalStages(ran, opbuild.Succeeded, opt.Some("ApprovalRequired"), now)
	if kept[5] != withDetail(stage(store.StageDeploy, store.StageSkipped), "ApprovalRequired") {
		t.Errorf("kept: %+v", kept[5])
	}

	rejected := build.FinalStages(ran, opbuild.Failed, opt.Some(string(outcome.OutputRejected)), now)
	if rejected[4].Status != store.StageFailed || rejected[5].Status != store.StageSkipped {
		t.Errorf("rejected: %+v", rejected[4:])
	}

	// Failed before anything ran: the first stage carries the failure.
	refused := build.FinalStages(build.InitialStages(false), opbuild.Failed, opt.Some("CredentialsRefused"), now)
	if refused[0] != withDetail(stage(store.StageClone, store.StageFailed, now, now), "CredentialsRefused") ||
		refused[1].Status != store.StageSkipped || refused[5].Status != store.StageSkipped {
		t.Errorf("refused: %+v", refused)
	}

	running := build.MergeStages(build.InitialStages(false),
		build.PodStages(buildPod(corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: kubeTime(3000)}}))...)
	cancelled := build.FinalStages(running, opbuild.Cancelled, opt.None[string](), now)
	if cancelled[2] != withDetail(stage(store.StageBuild, store.StageFailed, 3000, now), "cancelled") ||
		cancelled[4].Status != store.StageSkipped {
		t.Errorf("cancelled: %+v", cancelled)
	}
	for _, final := range [][]store.BuildStage{deployed, kept, rejected, refused, cancelled} {
		for _, s := range final {
			if s.Status == store.StagePending || s.Status == store.StageRunning {
				t.Errorf("left open: %+v", s)
			}
		}
	}
}

func TestTheKeptLogIsTheEndOfEachContainer(t *testing.T) {
	log := build.LogOf(map[string]string{
		outcome.FetchContainer: "fetched\n", "plan": "planned", outcome.BuildContainer: "#1 building\n#2 done\n",
	}, 1<<20)
	want := "==> clone <==\nfetched\n==> plan <==\nplanned\n==> build <==\n#1 building\n#2 done\n"
	if log != want {
		t.Errorf("log %q", log)
	}
	cut := build.LogOf(map[string]string{outcome.BuildContainer: strings.Repeat("é", 100)}, 51)
	if len(cut) > 51 || !strings.HasSuffix(cut, "é\n") || strings.ContainsRune(cut, '�') {
		t.Errorf("cut %q", cut)
	}
	if got := build.LogOf(map[string]string{outcome.BuildContainer: "bad \xff byte"}, 100); !strings.Contains(got, "bad � byte") {
		t.Errorf("invalid UTF-8 %q", got)
	}
	if got := build.LogOf(nil, 100); got != "" {
		t.Errorf("no containers ran: %q", got)
	}

	c := newFakeCluster()
	w := c.worker(0)
	if n := build.LogTailBytes(w); n != build.DefaultLogTailBytes {
		t.Errorf("default tail %d", n)
	}
	pod := buildPod(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})
	parts := build.PodLog(t.Context(), w, pod)
	// The fake API answers "fake logs" for every container that started.
	if diff := cmp.Diff(map[string]string{outcome.FetchContainer: "fake logs", "plan": "fake logs"}, parts); diff != "" {
		t.Errorf("parts: %s", diff)
	}
}

// connections is a Git connection whose token is "glpat-x".
type connections struct {
	heads int
	err   error
}

func (c *connections) Head(context.Context, ids.OrgID, ids.GitConnectionID, source.RepoName, source.BranchName) (build.Head, error) {
	c.heads++
	if c.err != nil {
		return build.Head{}, c.err
	}
	sha, err := source.ParseCommitSha(head)
	return build.Head{Commit: sha, RepositoryID: 9}, err
}

func (c *connections) Access(_ context.Context, _ ids.OrgID, _ ids.GitConnectionID, repository source.RepoName) (build.ConnectionAccess, error) {
	if c.err != nil {
		return build.ConnectionAccess{}, c.err
	}
	return build.ConnectionAccess{
		Token:    build.FetchToken{Token: "glpat-x"},
		CloneURL: "https://gitlab.example.com/" + repository.String() + ".git",
	}, nil
}

func TestABuildThroughAConnectionFetchesWithItsToken(t *testing.T) {
	a := attempt(t, source.BuildRecipe{Strategy: source.Dockerfile})
	a.Provider, a.InstallationID, a.Connection = store.GitProviderGitLab, 0, opt.Some(ids.New[ids.GitConnection]())
	s := settings()
	// A Secret left by an interrupted admission holds the connection's
	// token: it is replaced, never revoked.
	left := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: build.SecretName(a), Namespace: s.Namespace},
		Data:       map[string][]byte{build.TokenKey: []byte("glpat-x")},
	}
	c := newFakeCluster(left)
	conns := &connections{}
	w := build.NewWorker(build.Deps{
		Client: c.client, Dynamic: c.dynamic, ID: "w", Provider: c.provider, Connections: conns,
		Verifier: registry{}, Settings: c.settings, Clock: clock.Fixed(0), Logger: discard(),
	})
	ctx := t.Context()
	name := check(build.CreateObjects(ctx, w, a)).must(t)
	if len(c.provider.revoked) != 0 {
		t.Errorf("the organization's token was revoked: %v", c.provider.revoked)
	}
	secret := check(c.client.CoreV1().Secrets(s.Namespace).Get(ctx, build.SecretName(a), metav1.GetOptions{})).must(t)
	if string(secret.Data[build.TokenKey]) != "glpat-x" {
		t.Errorf("token %q", secret.Data[build.TokenKey])
	}
	job := check(c.client.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{})).must(t)
	fetch := job.Spec.Template.Spec.InitContainers[0]
	i := slices.IndexFunc(fetch.Env, func(e corev1.EnvVar) bool { return e.Name == "KUBEN_CLONE_URL" })
	if i < 0 || fetch.Env[i].Value != "https://gitlab.example.com/acme/shop.git" {
		t.Errorf("clone URL %+v", fetch.Env)
	}

	conns.err = build.Refused{Reason: "401 Unauthorized"}
	b := attempt(t, source.BuildRecipe{})
	b.AttemptNo, b.Connection = 2, a.Connection
	b.ID = ids.New[ids.BuildAttempt]()
	_, err := build.CreateObjects(ctx, w, b)
	var refused build.Refused
	if err == nil || !errors.As(errors.Unwrap(err), &refused) && !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token: %v", err)
	}
}

func TestASyncReadsTheHeadThroughItsReader(t *testing.T) {
	ctx := t.Context()
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	branch := check(source.ParseBranchName("main")).must(t)
	conns := &connections{}
	w := build.NewWorker(build.Deps{ID: "w", Connections: conns, Logger: discard()})
	binding := store.SourceBinding{
		Provider: store.GitProviderGitLab, Connection: opt.Some(ids.New[ids.GitConnection]()),
		Repository: repo, Branch: branch,
	}
	got := check(build.ReadHead(ctx, w, binding)).must(t)
	if got.Commit.String() != head || got.RepositoryID != 9 || conns.heads != 1 {
		t.Errorf("head %+v, %d reads", got, conns.heads)
	}
	binding.PullRequest = opt.Some(uint64(3))
	var notFound build.NotFound
	if _, err := build.ReadHead(ctx, w, binding); !errors.As(err, &notFound) {
		t.Errorf("a preview through a connection: %v", err)
	}

	// Without the App, an installation's source is refused; without a
	// keyring, a connection's is unavailable.
	bare := build.NewWorker(build.Deps{ID: "w", Logger: discard()})
	var refused build.Refused
	if _, err := build.ReadHead(ctx, bare, store.SourceBinding{InstallationID: 7, Repository: repo, Branch: branch}); !errors.As(err, &refused) {
		t.Errorf("no App: %v", err)
	}
	binding.PullRequest = opt.None[uint64]()
	var unavailable build.Unavailable
	if _, err := build.ReadHead(ctx, bare, binding); !errors.As(err, &unavailable) {
		t.Errorf("no connections: %v", err)
	}
}

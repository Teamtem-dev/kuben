package build_test

// The worker end to end on PostgreSQL, with client-go's fake clients for
// the cluster and fakes of the provider and the registry: a sync queues a
// build, the build gets its objects, a finished Job is verified, scanned
// and completed, and the objects are cleaned up. Rust had no such test.

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

const head = "0123456789abcdef0123456789abcdef01234567"

// provider is a Git provider whose head is always head.
type provider struct {
	mu      sync.Mutex
	revoked []string
}

func (p *provider) Head(context.Context, uint64, source.RepoName, source.BranchName) (build.Head, error) {
	sha, err := source.ParseCommitSha(head)
	return build.Head{Commit: sha, RepositoryID: 42}, err
}

func (p *provider) PullHead(context.Context, uint64, source.RepoName, uint64) (build.Head, error) {
	return build.Head{}, build.NotFound{What: "no pulls"}
}

func (p *provider) FetchToken(context.Context, uint64, source.RepoName) (build.FetchToken, error) {
	return build.FetchToken{Token: "ghs_x", ExpiresAt: 1}, nil
}

func (p *provider) Revoke(_ context.Context, token build.FetchToken) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = append(p.revoked, token.Token)
	return nil
}

func (p *provider) CloneURL(repository source.RepoName) string {
	return "https://github.com/" + repository.String() + ".git"
}

// registry holds the digests in it.
type registry map[string]bool

func (r registry) Verify(_ context.Context, repository string, digest artifact.Digest) error {
	if !r[digest.String()] {
		return build.ManifestMissing{What: repository + "@" + digest.String()}
	}
	return nil
}

type flow struct {
	store    *store.Store
	raw      *pgxpool.Pool
	org      ids.OrgID
	target   ids.TargetID
	provider *provider
	client   *fake.Clientset
	dynamic  *dynamicfake.FakeDynamicClient
	worker   *build.Worker
	settings build.Settings
	held     registry
}

func newFlow(t *testing.T, held registry) *flow {
	t.Helper()
	ctx := t.Context()
	pc := pgtest.Schema(t)
	st := check(store.ConnectConfig(ctx, pc.Copy(), 4)).must(t)
	t.Cleanup(st.Close)
	raw := check(pgxpool.NewWithConfig(ctx, pc.Copy())).must(t)
	t.Cleanup(raw.Close)
	f := &flow{store: st, raw: raw, provider: &provider{}, settings: settings(), held: held}
	f.org, f.target = seed(t, st)
	f.client = fake.NewClientset()
	f.dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource): "BuildRunList"})
	f.worker = f.newWorker(clock.System{})
	return f
}

func (f *flow) newWorker(c clock.Clock) *build.Worker {
	return build.NewWorker(build.Deps{
		Store: f.store, Client: f.client, Dynamic: f.dynamic, ID: "w", Provider: f.provider,
		Verifier: f.held, Settings: f.settings, Limits: store.SlotLimits{Total: 4, PerOrg: 2},
		Clock: c, Logger: discard(),
	})
}

// seed is an organization with a target bound to acme/shop@main, and a
// sync of it requested.
func seed(t *testing.T, st *store.Store) (ids.OrgID, ids.TargetID) {
	t.Helper()
	ctx := t.Context()
	o := check(st.CreateOrg(ctx, "acme", "Acme")).must(t).ID
	tn := check(st.Tenant(ctx, o)).must(t)
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	project := check(tn.CreateProject(ctx, "shop", "Shop")).must(t)
	env := check(tn.CreateEnvironment(ctx, project, "production", "Production", true)).must(t)
	cluster := check(tn.CreateCluster(ctx, "eu-1")).must(t)
	placement := check(tn.CreatePlacement(ctx, project, env, cluster, "acme-shop")).must(t)
	app := check(tn.CreateApplication(ctx, project, "web", "Web")).must(t)
	tgt := check(tn.CreateTarget(ctx, project, app, placement)).must(t)
	if _, ok, err := tn.CreateConfigRevision(ctx, project, tgt, map[string]any{"replicas": 1}, "user:alice"); err != nil || !ok {
		t.Fatalf("revision: %v, %v", ok, err)
	}
	if !check(tn.LinkInstallation(ctx, 7, "acme")).must(t) {
		t.Fatal("link refused")
	}
	a := attempt(t, source.BuildRecipe{Strategy: source.Auto})
	bound := check(tn.BindSource(ctx, project, tgt, store.NewBinding{
		InstallationID: 7, Repository: a.Repository, Branch: a.Branch, Recipe: a.Recipe, ImageRepository: a.ImageRepository,
	})).must(t)
	if _, ok := bound.(store.BoundCreated); !ok {
		t.Fatalf("not bound: %#v", bound)
	}
	binding, ok, err := tn.BindingOfTarget(ctx, tgt)
	if err != nil || !ok {
		t.Fatalf("binding: %v, %v", ok, err)
	}
	check(tn.RequestSync(ctx, binding, "test", store.NewAudit{})).must(t)
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return o, tgt
}

// work runs one claim, which must find an operation.
func (f *flow) work(t *testing.T) {
	t.Helper()
	if check(f.worker.WorkOnce(t.Context())).must(t).IsNone() {
		t.Fatal("nothing was due")
	}
}

// due makes every waiting operation due now.
func (f *flow) due(t *testing.T) {
	t.Helper()
	if _, err := f.raw.Exec(t.Context(), "UPDATE operations SET next_attempt_at = 0 WHERE NOT done"); err != nil {
		t.Fatal(err)
	}
}

func (f *flow) attempt(t *testing.T) store.BuildAttempt {
	t.Helper()
	tn := check(f.store.Tenant(t.Context(), f.org)).must(t)
	defer tn.Rollback(t.Context()) //nolint:errcheck // read only
	builds := check(tn.BuildsOfTarget(t.Context(), f.target, 10)).must(t)
	if len(builds) != 1 {
		t.Fatalf("%d builds", len(builds))
	}
	return builds[0]
}

// finish makes the attempt's Job complete with a pod that reported digest
// and a clean scan.
func (f *flow) finish(t *testing.T, a store.BuildAttempt, digest string) {
	t.Helper()
	ctx := t.Context()
	jobs := f.client.BatchV1().Jobs(f.settings.Namespace)
	job := check(jobs.Get(ctx, build.Name(a), metav1.GetOptions{})).must(t)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	check(jobs.UpdateStatus(ctx, job, metav1.UpdateOptions{})).must(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: build.Name(a) + "-x", Namespace: f.settings.Namespace,
			Labels: map[string]string{build.AttemptLabel: a.ID.String()},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "build", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason: "Completed", Message: `{"digest":"` + digest + `","strategy":"dockerfile"}`,
				},
			}}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "scan", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason: "Completed", Message: `{"status":"ok","scanner":"trivy 0.74.0","counts":{"critical":0,"high":1,"medium":0,"low":0,"unknown":0},"findings":["HIGH:CVE-2026-2"]}`,
				},
			}}},
		},
	}
	check(f.client.CoreV1().Pods(f.settings.Namespace).Create(ctx, pod, metav1.CreateOptions{})).must(t)
}

func TestASyncedHeadIsBuiltVerifiedAndCleanedUp(t *testing.T) {
	f := newFlow(t, registry{good: true})
	ctx := t.Context()
	f.work(t) // the sync queues a build
	f.work(t) // the build is admitted
	a := f.attempt(t)
	if a.Phase != opbuild.Preparing || a.Commit.String() != head {
		t.Fatalf("attempt %s at %s", a.Phase, a.Commit)
	}
	ns := f.settings.Namespace
	secret := check(f.client.CoreV1().Secrets(ns).Get(ctx, build.SecretName(a), metav1.GetOptions{})).must(t)
	if string(secret.Data[build.TokenKey]) != "ghs_x" {
		t.Errorf("token %q", secret.Data[build.TokenKey])
	}
	check(f.client.BatchV1().Jobs(ns).Get(ctx, build.Name(a), metav1.GetOptions{})).must(t)
	runs := f.dynamic.Resource(v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource)).Namespace(ns)
	run := check(runs.Get(ctx, build.Name(a), metav1.GetOptions{})).must(t)
	if phase, _, _ := unstructured.NestedString(run.Object, "status", "phase"); phase != "Running" {
		t.Errorf("BuildRun phase %q", phase)
	}

	f.finish(t, a, good)
	f.due(t)
	f.work(t) // observed, verified, scanned and completed
	a = f.attempt(t)
	if a.Phase != opbuild.Succeeded || a.Digest.IsNone() {
		t.Fatalf("attempt %s, digest %v, failure %v %v", a.Phase, a.Digest, a.Failure, a.FailureDetail)
	}
	if _, err := f.client.BatchV1().Jobs(ns).Get(ctx, build.Name(a), metav1.GetOptions{}); !build.IsNotFound(err) {
		t.Errorf("the Job is left: %v", err)
	}
	if _, err := f.client.CoreV1().Secrets(ns).Get(ctx, build.SecretName(a), metav1.GetOptions{}); !build.IsNotFound(err) {
		t.Errorf("the Secret is left: %v", err)
	}
	if !slices.Contains(f.provider.revoked, "ghs_x") {
		t.Errorf("revoked %v", f.provider.revoked)
	}
	tn := check(f.store.Tenant(ctx, f.org)).must(t)
	defer tn.Rollback(ctx) //nolint:errcheck // read only
	scans := check(tn.LatestScans(ctx, []string{good})).must(t)
	if s, ok := scans[good]; !ok || s.Status != scan.StatusOK || s.Counts.High != 1 {
		t.Errorf("scans %+v", scans)
	}
	run = check(runs.Get(ctx, build.Name(a), metav1.GetOptions{})).must(t)
	if digest, _, _ := unstructured.NestedString(run.Object, "status", "imageDigest"); digest != good {
		t.Errorf("BuildRun digest %q", digest)
	}

	// A day later the finished BuildRun is swept.
	later := f.newWorker(clock.Fixed(clock.System{}.NowMs() + 25*3600*1000))
	if n := check(later.Sweep(ctx)).must(t); n != 1 {
		t.Errorf("swept %d", n)
	}
	if n := check(f.worker.Sweep(ctx)).must(t); n != 0 {
		t.Errorf("swept %d again", n)
	}
}

func TestADigestTheRegistryLacksFailsTheBuild(t *testing.T) {
	f := newFlow(t, registry{})
	f.work(t)
	f.work(t)
	a := f.attempt(t)
	f.finish(t, a, forged)
	f.due(t)
	f.work(t)
	a = f.attempt(t)
	if a.Phase != opbuild.Failed || a.Failure != opt.Some("OutputRejected") {
		t.Errorf("attempt %s, failure %v", a.Phase, a.Failure)
	}
	if _, err := f.client.BatchV1().Jobs(f.settings.Namespace).Get(t.Context(), build.Name(a), metav1.GetOptions{}); !build.IsNotFound(err) {
		t.Errorf("the Job is left: %v", err)
	}
}

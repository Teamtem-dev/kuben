package build_test

// The worker's cluster half with client-go's fake clients: what runs here
// without a database or an API server (envtest repeats it against a real
// one, flow_test.go on PostgreSQL).

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
)

type fakeCluster struct {
	client   *fake.Clientset
	dynamic  *dynamicfake.FakeDynamicClient
	provider *provider
	settings build.Settings
}

func newFakeCluster(objects ...runtime.Object) *fakeCluster {
	return &fakeCluster{
		client: fake.NewClientset(objects...),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource): "BuildRunList"}),
		provider: &provider{},
		settings: settings(),
	}
}

func (c *fakeCluster) worker(now int64) *build.Worker {
	return build.NewWorker(build.Deps{
		Client: c.client, Dynamic: c.dynamic, ID: "w", Provider: c.provider, Verifier: registry{},
		Settings: c.settings, Clock: clock.Fixed(now), Logger: discard(),
	})
}

func (c *fakeCluster) run(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	runs := c.dynamic.Resource(v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource)).Namespace(c.settings.Namespace)
	return check(runs.Get(t.Context(), name, metav1.GetOptions{})).must(t)
}

func TestTheObjectsOfABuildAreCreatedOnce(t *testing.T) {
	c := newFakeCluster()
	ctx := t.Context()
	a := attempt(t, source.BuildRecipe{Strategy: source.Dockerfile})
	w := c.worker(ms(t, "2026-09-17T12:00:00Z"))
	name := check(build.CreateObjects(ctx, w, a)).must(t)
	if name != build.Name(a) {
		t.Errorf("name %s", name)
	}
	if spec, _, _ := unstructured.NestedString(c.run(t, name).Object, "spec", "gitRef"); spec != a.Commit.String() {
		t.Errorf("BuildRun gitRef %q", spec)
	}
	ns := c.settings.Namespace
	secret := check(c.client.CoreV1().Secrets(ns).Get(ctx, build.SecretName(a), metav1.GetOptions{})).must(t)
	if string(secret.Data[build.TokenKey]) != "ghs_x" || secret.Immutable == nil || !*secret.Immutable {
		t.Errorf("secret %+v", secret)
	}
	check(c.client.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})).must(t)

	// A second admission finds the Job and asks for no token.
	before := len(c.client.Actions())
	if again := check(build.CreateObjects(ctx, w, a)).must(t); again != name {
		t.Errorf("name %s", again)
	}
	for _, action := range c.client.Actions()[before:] {
		if action.GetVerb() == "create" {
			t.Errorf("created again: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestASecretLeftByAnInterruptedAdmissionIsReplaced(t *testing.T) {
	a := attempt(t, source.BuildRecipe{})
	s := settings()
	left := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: build.SecretName(a), Namespace: s.Namespace},
		Data:       map[string][]byte{build.TokenKey: []byte("ghs_old")},
	}
	c := newFakeCluster(left)
	ctx := t.Context()
	check(build.CreateObjects(ctx, c.worker(0), a)).must(t)
	if !slices.Equal(c.provider.revoked, []string{"ghs_old"}) {
		t.Errorf("revoked %v", c.provider.revoked)
	}
	secret := check(c.client.CoreV1().Secrets(s.Namespace).Get(ctx, build.SecretName(a), metav1.GetOptions{})).must(t)
	if string(secret.Data[build.TokenKey]) != "ghs_x" {
		t.Errorf("token %q", secret.Data[build.TokenKey])
	}
}

func TestABudgetThatIsNoQuantityIsRefusedBeforeTheJob(t *testing.T) {
	c := newFakeCluster()
	c.settings.Memory = "a lot"
	_, err := build.CreateObjects(t.Context(), c.worker(0), attempt(t, source.BuildRecipe{}))
	if err == nil {
		t.Fatal("a Job with an invalid budget")
	}
	if jobs := check(c.client.BatchV1().Jobs(c.settings.Namespace).List(t.Context(), metav1.ListOptions{})).must(t); len(jobs.Items) != 0 {
		t.Errorf("%d Jobs", len(jobs.Items))
	}
}

func TestTheBuildRunMirrorsTheAttemptAndIsSweptADayLater(t *testing.T) {
	c := newFakeCluster()
	ctx := t.Context()
	a := attempt(t, source.BuildRecipe{})
	finished := ms(t, "2026-09-17T12:00:00Z")
	w := c.worker(finished)
	name := check(build.CreateObjects(ctx, w, a)).must(t)

	build.Mirror(ctx, w, a, opbuild.Preparing, opt.None[string]())
	run := c.run(t, name).Object
	if phase, _, _ := unstructured.NestedString(run, "status", "phase"); phase != "Running" {
		t.Errorf("phase %q", phase)
	}
	build.Mirror(ctx, w, a, opbuild.Succeeded, opt.Some(stepDigest))
	run = c.run(t, name).Object
	want := map[string]string{
		"phase": "Succeeded", "imageDigest": stepDigest, "jobName": name,
		"startedAt": "2026-09-17T12:00:00Z", "finishedAt": "2026-09-17T12:00:00Z",
	}
	for field, value := range want {
		if got, _, _ := unstructured.NestedString(run, "status", field); got != value {
			t.Errorf("status.%s %q, want %q", field, got, value)
		}
	}

	if n := check(c.worker(finished + 24*3600*1000).Sweep(ctx)).must(t); n != 0 {
		t.Errorf("swept %d after exactly a day", n)
	}
	if n := check(c.worker(finished + 24*3600*1000 + 1000).Sweep(ctx)).must(t); n != 1 {
		t.Errorf("swept %d after a day and a second", n)
	}
	runs := check(c.dynamic.Resource(v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource)).
		Namespace(c.settings.Namespace).List(ctx, metav1.ListOptions{})).must(t)
	if len(runs.Items) != 0 {
		t.Errorf("%d BuildRuns left", len(runs.Items))
	}
}

func TestAMirrorOfAMissingBuildRunIsNoError(t *testing.T) {
	c := newFakeCluster()
	// Best effort: nothing to patch, nothing panics, nothing is created.
	build.Mirror(t.Context(), c.worker(0), attempt(t, source.BuildRecipe{}), opbuild.Failed, opt.None[string]())
	runs := check(c.dynamic.Resource(v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource)).
		Namespace(c.settings.Namespace).List(t.Context(), metav1.ListOptions{})).must(t)
	if len(runs.Items) != 0 {
		t.Errorf("%d BuildRuns", len(runs.Items))
	}
}

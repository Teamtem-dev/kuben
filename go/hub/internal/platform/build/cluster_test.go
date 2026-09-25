package build_test

// The build objects against a real API server (envtest): the server
// validates the Job, the Secret, the BuildRun and its status as it would
// in a cluster, which Rust checked only by deserializing into k8s-openapi
// types.

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	opbuild "github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/kubetest"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

var apiserver kubetest.Server

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &apiserver)) }

func TestTheAPIServerAcceptsTheBuildObjects(t *testing.T) {
	c := apiserver.Connect(t)
	ctx := t.Context()
	s := settings()
	s.Namespace = kubetest.Namespace(t, c, "kuben-builds")
	p := &provider{}
	w := build.NewWorker(build.Deps{
		Client: c.Typed, Dynamic: c.Dynamic, ID: "w", Provider: p, Verifier: registry{},
		Settings: s, Clock: clock.System{}, Logger: discard(),
	})
	a := attempt(t, source.BuildRecipe{Strategy: source.Dockerfile})
	name := check(build.CreateObjects(ctx, w, a)).must(t)
	job := check(c.Typed.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{})).must(t)
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != v1alpha1.BuildRunKind || job.OwnerReferences[0].UID == "" {
		t.Errorf("owner %+v", job.OwnerReferences)
	}
	secret := check(c.Typed.CoreV1().Secrets(s.Namespace).Get(ctx, build.SecretName(a), metav1.GetOptions{})).must(t)
	if string(secret.Data[build.TokenKey]) != "ghs_x" || secret.OwnerReferences[0].UID != job.OwnerReferences[0].UID {
		t.Errorf("secret %+v", secret.ObjectMeta)
	}
	// A second admission of the same attempt changes nothing.
	if again := check(build.CreateObjects(ctx, w, a)).must(t); again != name {
		t.Errorf("name %s", again)
	}

	runs := c.Dynamic.Resource(v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource)).Namespace(s.Namespace)
	build.Mirror(ctx, w, a, opbuild.Preparing, opt.None[string]())
	build.Mirror(ctx, w, a, opbuild.Succeeded, opt.Some(stepDigest))
	run := check(runs.Get(ctx, name, metav1.GetOptions{})).must(t)
	for field, want := range map[string]string{"phase": "Succeeded", "imageDigest": stepDigest, "jobName": name} {
		if got, _, _ := unstructured.NestedString(run.Object, "status", field); got != want {
			t.Errorf("status.%s %q, want %q", field, got, want)
		}
	}
	for _, field := range []string{"startedAt", "finishedAt"} {
		if got, _, _ := unstructured.NestedString(run.Object, "status", field); got == "" {
			t.Errorf("no status.%s", field)
		}
	}

	rescan := check(build.RescanJob(s, "registry.local/acme/shop", stepDigest)).must(t)
	check(c.Typed.BatchV1().Jobs(s.Namespace).Create(ctx, rescan, metav1.CreateOptions{})).must(t)
}

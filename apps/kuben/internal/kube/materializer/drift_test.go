package materializer_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

func driftApp(image string, generation int64, annotation opt.Val[string]) *v1alpha1.App {
	app := &v1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "kb-shop-production", Generation: generation},
		Spec: v1alpha1.AppSpec{
			Source: v1alpha1.SourceFromImage(image),
			Runtime: v1alpha1.Runtime{Processes: map[string]v1alpha1.Process{
				"web": {Port: ptr(uint16(8080)), Size: v1alpha1.DefaultProcessSize, Replicas: v1alpha1.Replicas{Min: 1, Max: 1}},
			}},
		},
	}
	if v, ok := annotation.Get(); ok {
		app.Annotations = map[string]string{v1alpha1.AnnotationGeneration: v}
	}
	return app
}

func ptr[T any](v T) *T { return &v }

func writtenAt(resourceGeneration int64) store.Materialized {
	return store.Materialized{
		Org: ids.New[ids.Org](), Project: ids.New[ids.Project](), Target: ids.New[ids.Target](),
		Generation: target.Generation(3), Operation: ids.New[ids.Operation](),
		ResourceUID: "uid-1", ResourceGeneration: resourceGeneration,
	}
}

func TestAnObjectAsWrittenNeedsNoRender(t *testing.T) {
	if !materializer.Untouched(driftApp("a@sha256:1", 4, opt.Some("3")), writtenAt(4)) {
		t.Fatal("as written")
	}
	for why, app := range map[string]*v1alpha1.App{
		"spec edited":        driftApp("a@sha256:1", 5, opt.Some("3")),
		"annotation edited":  driftApp("a@sha256:1", 4, opt.Some("9")),
		"annotation removed": driftApp("a@sha256:1", 4, opt.None[string]()),
	} {
		if materializer.Untouched(app, writtenAt(4)) {
			t.Error(why)
		}
	}
}

func entry(manager, subresource string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Subresource: subresource}
}

func TestDriftIsWhatDiffersFromTheRendering(t *testing.T) {
	desired := driftApp("a@sha256:1", 1, opt.Some("3"))
	if got := materializer.Compare(opt.Some(driftApp("a@sha256:1", 7, opt.Some("3"))), desired); got != (materializer.Clean{}) {
		t.Fatalf("a newer metadata.generation with the same content: %#v", got)
	}

	edited := driftApp("evil:latest", 8, opt.Some("3"))
	edited.ManagedFields = []metav1.ManagedFieldsEntry{
		entry(materializer.FieldManager, ""), entry("kubectl-edit", ""), entry("kuben", "status"), entry("kubectl-edit", ""),
	}
	want := materializer.Drift{SpecChanged: true, Managers: []string{"kubectl-edit"}}
	if diff := cmp.Diff(materializer.Finding(want), materializer.Compare(opt.Some(edited), desired)); diff != "" {
		t.Fatal(diff)
	}

	relabelled, ok := materializer.Compare(opt.Some(driftApp("a@sha256:1", 1, opt.Some("99"))), desired).(materializer.Drift)
	if !ok || !relabelled.AnnotationChanged || relabelled.SpecChanged {
		t.Fatalf("a forged annotation is drift: %#v", relabelled)
	}

	deleted, ok := materializer.Compare(opt.None[*v1alpha1.App](), desired).(materializer.Drift)
	if !ok || !deleted.Deleted || deleted.Describe()["deleted"] != true {
		t.Fatalf("a deleted object is drift: %#v", deleted)
	}
}

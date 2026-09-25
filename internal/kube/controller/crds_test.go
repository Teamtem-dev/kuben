package controller_test

import (
	"context"
	"log/slog"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Teamtem-dev/kuben/internal/kube/controller"
)

func TestEveryCRDIsAppliedIdempotently(t *testing.T) {
	crds, err := controller.CRDs()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, crd := range crds {
		names = append(names, crd.GetName())
	}
	want := []string{
		"kubenconfigs.kuben.dev", "projects.kuben.dev", "environments.kuben.dev", "apps.kuben.dev",
		"releases.kuben.dev", "buildruns.kuben.dev", "applicationruntimes.kuben.dev", "executiontasks.kuben.dev",
	}
	if len(names) != len(want) {
		t.Fatalf("crds: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("crds: %v", names)
		}
	}
	c := newCluster(t, interceptor.Funcs{})
	for range 2 {
		if err := controller.EnsureCRDs(context.Background(), c, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
	}
	gvk := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	for _, name := range want {
		crd, ok := get(t, c, gvk, "", name)
		if !ok {
			t.Fatalf("%s not applied", name)
		}
		if group, _, _ := unstructuredString(crd.Object, "spec", "group"); group != "kuben.dev" {
			t.Fatalf("%s: group %q", name, group)
		}
	}
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	var v any = obj
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return "", false, nil
		}
		v = m[p]
	}
	s, ok := v.(string)
	return s, ok, nil
}

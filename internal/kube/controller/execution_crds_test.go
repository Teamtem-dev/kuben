package controller_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/kube/envtest"
)

// The execution CRDs' rules are enforced by the API server itself
// (ADR-027), which also proves the rules fit its CEL cost budget
// (crates/kuben-platform/tests/execution_crds.rs).

var apiserver envtest.Server

func TestMain(m *testing.M) { os.Exit(envtest.Main(m, &apiserver)) }

func hash(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// resource is the namespaced client of a kuben.dev resource in ns.
func resource(c envtest.Clients, plural, ns string) dynamic.ResourceInterface {
	return c.Dynamic.Resource(schema.GroupVersionResource{
		Group: v1alpha1.SchemeGroupVersion.Group, Version: v1alpha1.SchemeGroupVersion.Version, Resource: plural,
	}).Namespace(ns)
}

// patcher merge-patches one object, its spec or its status.
type patcher struct {
	t    *testing.T
	ri   dynamic.ResourceInterface
	name string
}

func (p patcher) merge(patch map[string]any, subresources ...string) error {
	p.t.Helper()
	body, err := json.Marshal(patch)
	if err != nil {
		p.t.Fatal(err)
	}
	_, err = p.ri.Patch(p.t.Context(), p.name, types.MergePatchType, body, metav1.PatchOptions{}, subresources...)
	return err //nolint:wrapcheck // compared by the test
}

func (p patcher) refused(what string, patch map[string]any, subresources ...string) {
	p.t.Helper()
	if err := p.merge(patch, subresources...); !envtest.IsInvalid(err) {
		p.t.Errorf("%s: expected 422, got %v", what, err)
	}
}

func (p patcher) accepted(what string, patch map[string]any, subresources ...string) {
	p.t.Helper()
	if err := p.merge(patch, subresources...); err != nil {
		p.t.Fatalf("%s: %v", what, err)
	}
}

func spec(fields map[string]any) map[string]any   { return map[string]any{"spec": fields} }
func status(fields map[string]any) map[string]any { return map[string]any{"status": fields} }

func TestTheApiserverFencesApplicationRuntimes(t *testing.T) {
	c := apiserver.Connect(t)
	ns := envtest.Namespace(t, c, "kuben-m1-exec")
	ri := resource(c, v1alpha1.ApplicationRuntimeResource, ns)
	runtime := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": v1alpha1.ApplicationRuntimeKind,
		"metadata": map[string]any{"name": "web"},
		"spec": map[string]any{
			"targetId": "0199a0c0-0000-7000-8000-000000000001", "lifecycleUid": "0199a0c0-0000-7000-8000-000000000002",
			"controlEpoch": 1, "generation": 5, "inputHash": hash("a"),
			"releaseId": "0199a0c0-0000-7000-8000-000000000003",
			"plan": map[string]any{
				"id": "0199a0c0-0000-7000-8000-000000000004", "rendererVersion": "kuben-renderer/1",
				"digest": hash("e"), "resources": "[]",
			},
		},
	}}
	if _, err := ri.Create(t.Context(), runtime, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	p := patcher{t: t, ri: ri, name: "web"}
	p.refused("a lower generation", spec(map[string]any{"generation": 4, "inputHash": hash("b")}))
	p.refused("the same generation with another input hash", spec(map[string]any{"inputHash": hash("b")}))
	p.refused("a changed target", spec(map[string]any{"targetId": "0199a0c0-0000-7000-8000-00000000000f"}))
	p.refused("a lower control epoch", spec(map[string]any{"controlEpoch": 0}))
	p.accepted("the same envelope again", spec(map[string]any{"generation": 5, "inputHash": hash("a")}))
	p.accepted("a higher generation", spec(map[string]any{"generation": 6, "inputHash": hash("b")}))
	moved, err := ri.Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g, _, _ := unstructured.NestedInt64(moved.Object, "spec", "generation"); g != 6 {
		t.Fatalf("generation: %d", g)
	}
	p.accepted("observed", status(map[string]any{"observedGeneration": 6}), "status")
	p.refused("a lower observed generation", status(map[string]any{"observedGeneration": 5}), "status")
}

func TestTheApiserverKeepsTaskInputsCancelsAndReceipts(t *testing.T) {
	c := apiserver.Connect(t)
	ns := envtest.Namespace(t, c, "kuben-m1-exec")
	ri := resource(c, v1alpha1.ExecutionTaskResource, ns)
	task := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": v1alpha1.ExecutionTaskKind,
		"metadata": map[string]any{"name": "build-1"},
		"spec": map[string]any{
			"kind": "Build", "operationId": "0199a0c0-0000-7000-8000-000000000001", "attempt": 1,
			"input": `{"source":"git"}`, "inputHash": hash("a"), "deadline": "2026-09-15T12:00:00Z",
		},
	}}
	if _, err := ri.Create(t.Context(), task, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	p := patcher{t: t, ri: ri, name: "build-1"}
	p.refused("a changed input", spec(map[string]any{"input": `{"source":"other"}`}))
	p.refused("a changed kind", spec(map[string]any{"kind": "Backup"}))
	p.accepted("a cancel request is added", spec(map[string]any{
		"cancel": map[string]any{"requestedAt": "2026-09-15T11:00:00Z", "reason": "user"},
	}))
	p.refused("a changed cancel request", spec(map[string]any{"cancel": map[string]any{"reason": "someone else"}}))
	p.refused("a withdrawn cancel request", spec(map[string]any{"cancel": nil}))
	p.accepted("the first receipt", status(map[string]any{
		"phase": "Cancelled", "receipt": map[string]any{"outcome": "Cancelled", "finishedAt": "2026-09-15T11:30:00Z"},
	}), "status")
	p.refused("a rewritten receipt", status(map[string]any{"receipt": map[string]any{"outcome": "Succeeded"}}), "status")
	p.refused("a removed receipt", status(map[string]any{"receipt": nil}), "status")
}

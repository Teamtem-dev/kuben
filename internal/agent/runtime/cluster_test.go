package runtime_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/agent/runtime"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// Ported from crates/kuben-agent/tests/runtime.rs (M1.9, ADR-027): the
// agent's executor against a real apiserver. It writes the
// ApplicationRuntime, applies the plan's resources owned by it, reports
// Ready, prunes what a newer plan dropped, and is refused by the apiserver
// for a lower generation; an envelope whose digest does not match or that
// holds a kind the agent does not apply is refused before anything is
// written.
//
// They need a cluster with its controllers (the Deployments must roll
// out), as the Rust tests did (`#[ignore]`, run with `--ignored` against
// the current kube context in the kind job): they run when
// KUBEN_TEST_KUBE=1, each in its own throwaway namespace. The Deployments
// have no replicas, so nothing waits for an image.

// kubeVar turns the cluster tests on.
const kubeVar = "KUBEN_TEST_KUBE"

var (
	crds       = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	namespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	deploys    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	services   = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	runtimes   = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ApplicationRuntimeResource)
)

type testNS struct {
	client dynamic.Interface
	name   string
}

// runtimeCRD is the ApplicationRuntime CRD of the frozen manifest.
func runtimeCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "charts", "kuben", "crds", "kuben.dev_all.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var doc map[string]any
		if err := d.Decode(&doc); errors.Is(err, io.EOF) {
			t.Fatal("no ApplicationRuntime CRD in the manifest")
		} else if err != nil {
			t.Fatal(err)
		}
		crd := &unstructured.Unstructured{Object: doc}
		if crd.GetName() == "applicationruntimes.kuben.dev" {
			return crd
		}
	}
}

func newTestNS(t *testing.T) testNS {
	t.Helper()
	if os.Getenv(kubeVar) != "1" {
		t.Skipf("needs a Kubernetes cluster with its controllers: set %s=1 (the kind job)", kubeVar)
	}
	ctx := t.Context()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	crd := runtimeCRD(t)
	body, err := json.Marshal(crd.Object)
	if err != nil {
		t.Fatal(err)
	}
	force := true
	if _, err := client.Resource(crds).Patch(ctx, crd.GetName(), types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: v1alpha1.FieldManager, Force: &force}); err != nil {
		t.Fatal("install the ApplicationRuntime CRD:", err)
	}
	for range 60 {
		live, err := client.Resource(crds).Get(ctx, crd.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		conditions, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
		if slices.ContainsFunc(conditions, func(c any) bool {
			m, _ := c.(map[string]any)
			return m["type"] == "Established" && m["status"] == "True"
		}) {
			break
		}
		time.Sleep(time.Second)
	}
	name := "kuben-m1-agent-" + strings.ToLower(rand.Text())
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}}
	if _, err := client.Resource(namespaces).Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatal("create namespace:", err)
	}
	t.Cleanup(func() {
		background := metav1.DeletePropagationBackground
		_ = client.Resource(namespaces).Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &background})
	})
	return testNS{client: client, name: name}
}

func (n testNS) deployment(name string) map[string]any {
	labels := map[string]any{"app.kubernetes.io/managed-by": "kuben", "kuben.dev/app": "web"}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": n.name, "labels": labels},
		"spec": map[string]any{
			"replicas": 0,
			"selector": map[string]any{"matchLabels": map[string]any{"kuben.dev/app": "web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"kuben.dev/app": "web"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "web", "image": "registry.invalid/web:none"}}},
			},
		},
	}
}

func (n testNS) service() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "namespace": n.name, "labels": map[string]any{"app.kubernetes.io/managed-by": "kuben", "kuben.dev/app": "web"}},
		"spec":     map[string]any{"selector": map[string]any{"kuben.dev/app": "web"}, "ports": []any{map[string]any{"port": 80, "targetPort": 8080}}},
	}
}

func (n testNS) envelope(t *testing.T, generation int64, resources []any, digest string) protocol.Apply {
	t.Helper()
	text, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		digest = protocol.HexDigest(text)
	}
	spec, err := json.Marshal(v1alpha1.ApplicationRuntimeSpec{
		TargetID: "0199a0c0-0000-7000-8000-000000000001", LifecycleUID: "0199a0c0-0000-7000-8000-000000000002",
		Generation: generation, InputHash: protocol.HexDigest(fmt.Appendf(nil, "input-%d", generation)),
		ReleaseID: "0199a0c0-0000-7000-8000-000000000003",
		Plan: v1alpha1.PlanEnvelope{
			ID: fmt.Sprintf("plan-%d", generation), RendererVersion: "kuben-renderer/1", Digest: digest, Resources: string(text),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Apply{Target: "0199a0c0-0000-7000-8000-000000000001", Namespace: n.name, Name: "web", Spec: string(spec)}
}

// carry carries apply out and returns the observations, in order.
func carry(t *testing.T, e *runtime.KubeExecutor, apply protocol.Apply) []protocol.Observation {
	t.Helper()
	reports := make(chan protocol.Observation, 16)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	e.Carry(ctx, apply, reports)
	if ctx.Err() != nil {
		t.Fatal("not carried out in time")
	}
	close(reports)
	var seen []protocol.Observation
	for o := range reports {
		seen = append(seen, o)
	}
	return seen
}

func phases(seen []protocol.Observation) []protocol.RuntimePhase {
	out := make([]protocol.RuntimePhase, 0, len(seen))
	for _, o := range seen {
		out = append(out, o.Phase)
	}
	return out
}

func lastPhase(seen []protocol.Observation) protocol.RuntimePhase {
	if len(seen) == 0 {
		return ""
	}
	return seen[len(seen)-1].Phase
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTheAgentCarriesAnEnvelopeOutAndPrunesWhatANewerPlanDropped(t *testing.T) {
	ns := newTestNS(t)
	ctx := t.Context()
	e := runtime.NewKubeExecutor(ns.client, quietLogger()).WithVerifyDeadline(time.Minute, time.Second)
	first := carry(t, e, ns.envelope(t, 1, []any{ns.deployment("web-web"), ns.service()}, ""))
	want := []protocol.RuntimePhase{protocol.RuntimePhaseAccepted, protocol.RuntimePhaseApplying, protocol.RuntimePhaseReady}
	if !slices.Equal(phases(first), want) {
		t.Fatalf("%+v", first)
	}
	rt, err := ns.client.Resource(runtimes).Namespace(ns.name).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	generation, _, _ := unstructured.NestedInt64(rt.Object, "spec", "generation")
	observed, _, _ := unstructured.NestedInt64(rt.Object, "status", "observedGeneration")
	effective, _, _ := unstructured.NestedInt64(rt.Object, "status", "effectiveGeneration")
	inventory, _, _ := unstructured.NestedSlice(rt.Object, "status", "inventory")
	if generation != 1 || observed != 1 || effective != 1 || len(inventory) != 2 {
		t.Fatalf("%v", rt.Object)
	}
	web, err := ns.client.Resource(deploys).Namespace(ns.name).Get(ctx, "web-web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owners := web.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "ApplicationRuntime" || owners[0].Controller == nil || !*owners[0].Controller ||
		owners[0].UID != rt.GetUID() {
		t.Fatalf("%+v", owners)
	}

	// A newer plan without the Service: it is pruned, the Deployment stays.
	second := carry(t, e, ns.envelope(t, 2, []any{ns.deployment("web-web")}, ""))
	if lastPhase(second) != protocol.RuntimePhaseReady {
		t.Fatalf("%+v", second)
	}
	gone := false
	for range 30 {
		if _, err := ns.client.Resource(services).Namespace(ns.name).Get(ctx, "web", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(time.Second)
	}
	if !gone {
		t.Fatal("the dropped Service is pruned")
	}
	if _, err := ns.client.Resource(deploys).Namespace(ns.name).Get(ctx, "web-web", metav1.GetOptions{}); err != nil {
		t.Fatal("the Deployment stays:", err)
	}

	// A lower generation: the apiserver refuses the envelope.
	stale := carry(t, e, ns.envelope(t, 1, []any{ns.deployment("web-web")}, ""))
	if !slices.Equal(phases(stale), []protocol.RuntimePhase{protocol.RuntimePhaseRejected}) ||
		len(stale) != 1 || stale[0].Reason == nil || *stale[0].Reason != "EnvelopeRefused" {
		t.Fatalf("%+v", stale)
	}
}

// A report that never reached the hub (plan §18.1 crash/ACK replay, I17):
// the hub sends the same envelope again, and carrying it out again changes
// nothing: the same objects, the same generations, Ready again.
func TestTheSameEnvelopeCarriedAgainChangesNothing(t *testing.T) {
	ns := newTestNS(t)
	ctx := t.Context()
	e := runtime.NewKubeExecutor(ns.client, quietLogger()).WithVerifyDeadline(time.Minute, time.Second)
	envelope := ns.envelope(t, 1, []any{ns.deployment("web-web"), ns.service()}, "")
	if first := carry(t, e, envelope); lastPhase(first) != protocol.RuntimePhaseReady {
		t.Fatalf("%+v", first)
	}
	identity := func(gvr schema.GroupVersionResource, name string) [2]any {
		o, err := ns.client.Resource(gvr).Namespace(ns.name).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return [2]any{o.GetUID(), o.GetGeneration()}
	}
	deploymentBefore, runtimeBefore := identity(deploys, "web-web"), identity(runtimes, "web")
	again := carry(t, e, envelope)
	if lastPhase(again) != protocol.RuntimePhaseReady || slices.Contains(phases(again), protocol.RuntimePhaseRejected) {
		t.Fatalf("the apiserver takes the same envelope again: %+v", again)
	}
	if identity(deploys, "web-web") != deploymentBefore {
		t.Fatal("the same Deployment, not rolled again")
	}
	if identity(runtimes, "web") != runtimeBefore {
		t.Fatal("the same runtime")
	}
}

func TestTheAgentRefusesAWrongDigestOrKindBeforeWritingAnything(t *testing.T) {
	ns := newTestNS(t)
	e := runtime.NewKubeExecutor(ns.client, quietLogger())
	forged := carry(t, e, ns.envelope(t, 1, []any{ns.deployment("web-web")}, protocol.HexDigest([]byte("something else"))))
	if len(forged) == 0 || forged[0].Reason == nil || *forged[0].Reason != "DigestMismatch" {
		t.Fatalf("%+v", forged)
	}
	secret := []any{map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "x", "namespace": ns.name}}}
	refused := carry(t, e, ns.envelope(t, 1, secret, ""))
	if len(refused) == 0 || refused[0].Reason == nil || *refused[0].Reason != "KindNotAllowed" {
		t.Fatalf("%+v", refused)
	}
	if _, err := ns.client.Resource(runtimes).Namespace(ns.name).Get(t.Context(), "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("nothing written:", err)
	}
}

package materializer_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/materializer"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The Rust test ran the agent's own check (kuben_agent::runtime::check);
// until the agent is ported, the same checks are made here: the spec
// decodes, the resources hash to the digest, every resource is named and in
// the envelope's namespace.
func TestTheEnvelopePassesTheAgentsChecks(t *testing.T) {
	m := sample(t)
	m.Namespace, m.Delivery = "kb-shop-prod", store.DeliveryAgent
	plan := store.RunPlan{
		ID: ids.New[ids.RenderPlan](), RendererVersion: render.RendererVersion, CapabilitySnapshot: map[string]any{},
		// Keys in another order than canonical JSON has them.
		Resources: config(t, `[{"metadata":{"namespace":"kb-shop-prod","name":"web-web"},"kind":"Deployment","apiVersion":"apps/v1"}]`),
	}
	apply, err := materializer.Envelope(&m, plan)
	if err != nil {
		t.Fatal(err)
	}
	if apply.Namespace != "kb-shop-prod" || apply.Name != "web" || apply.Target != m.Target.String() {
		t.Fatalf("%+v", apply)
	}
	var spec v1alpha1.ApplicationRuntimeSpec
	if err := json.Unmarshal([]byte(apply.Spec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Generation != 3 || spec.ReleaseID != m.Release.String() || spec.Plan.ID != plan.ID.String() {
		t.Fatalf("%+v", spec)
	}
	if wire.SHA256(spec.Plan.Resources) != spec.Plan.Digest {
		t.Fatal("digest mismatch")
	}
	var resources []map[string]any
	if err := json.Unmarshal([]byte(spec.Plan.Resources), &resources); err != nil || len(resources) != 1 {
		t.Fatalf("%v %v", resources, err)
	}
	meta, _ := resources[0]["metadata"].(map[string]any)
	if meta["name"] != "web-web" || meta["namespace"] != apply.Namespace {
		t.Fatal(meta)
	}
	if spec.Plan.Resources != `[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web-web","namespace":"kb-shop-prod"}}]` {
		t.Fatalf("canonical: %s", spec.Plan.Resources)
	}
}

func observedAgent(generation int64, phase string, reason opt.Val[string]) opt.Val[store.RuntimeObservation] {
	return opt.Some(store.RuntimeObservation{Generation: generation, Phase: phase, Reason: reason})
}

func TestObservationsMoveOnlyTheRunOfTheirGeneration(t *testing.T) {
	none := opt.None[string]()
	cases := []struct {
		o    opt.Val[store.RuntimeObservation]
		want materializer.Heard
		why  string
	}{
		{opt.None[store.RuntimeObservation](), materializer.HeardNothing{}, "nothing"},
		{observedAgent(2, "ready", none), materializer.HeardNothing{}, "an older run"},
		{observedAgent(4, "accepted", none), materializer.HeardNewer{}, "newer"},
		{observedAgent(3, "accepted", none), materializer.HeardAccepted{}, "accepted"},
		{observedAgent(3, "applying", none), materializer.HeardApplying{}, "applying"},
		{observedAgent(3, "ready", none), materializer.HeardReady{}, "ready"},
		{observedAgent(3, "failed", opt.Some("RolloutFailed")), materializer.HeardFailed{Reason: "RolloutFailed"}, "failed"},
		{observedAgent(3, "rejected", none), materializer.HeardFailed{Reason: "EnvelopeRejected"}, "rejected"},
		{observedAgent(3, "unknown", none), materializer.HeardNothing{}, "unknown"},
	}
	for _, c := range cases {
		if got := materializer.HeardOf(c.o, 3); got != c.want {
			t.Errorf("%s: %#v", c.why, got)
		}
	}
}

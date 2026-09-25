package evidence_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/internal/evidence"
)

func node(layer evidence.Layer, status doctor.Status) evidence.Node {
	return evidence.NewNode(layer, status, string(layer)).Fact(string(layer) + " observed")
}

func healthy() []evidence.Node {
	var out []evidence.Node
	for _, l := range []evidence.Layer{
		evidence.Release, evidence.Deployment, evidence.Pods, evidence.Service, evidence.Endpoints,
		evidence.Gateway, evidence.Route, evidence.DNS, evidence.TLS,
	} {
		out = append(out, node(l, doctor.StatusOK))
	}
	return out
}

func with(nodes []evidence.Node, layer evidence.Layer, status doctor.Status) []evidence.Node {
	nodes = slices.Clone(nodes)
	for i := range nodes {
		if nodes[i].Layer == layer {
			nodes[i].Status = status
		}
	}
	return nodes
}

func TestEdgesConnectPresentLayersOnly(t *testing.T) {
	g := evidence.Of([]evidence.Node{node(evidence.Pods, doctor.StatusOK), node(evidence.Endpoints, doctor.StatusOK)})
	if diff := cmp.Diff([]evidence.Edge{{From: evidence.Pods, To: evidence.Endpoints}}, g.Edges); diff != "" {
		t.Fatal(diff)
	}
	if f := evidence.Diagnose(evidence.Of(healthy())); len(f) != 0 {
		t.Fatalf("nothing to report: %v", f)
	}
}

func TestAFailingCauseExplainsItsSymptoms(t *testing.T) {
	nodes := with(healthy(), evidence.Pods, doctor.StatusFail)
	nodes = with(nodes, evidence.Endpoints, doctor.StatusFail)
	nodes = with(nodes, evidence.Route, doctor.StatusFail)
	f := evidence.Diagnose(evidence.Of(nodes))
	if len(f) != 3 || f[0].Kind != evidence.RootCause || f[0].Layer != evidence.Pods || f[0].Confidence != evidence.High ||
		!slices.Equal(f[0].Related, []evidence.Layer{evidence.Endpoints, evidence.Route}) {
		t.Fatalf("%+v", f)
	}
	for _, s := range f[1:] {
		if s.Kind != evidence.Symptom || !slices.Contains(s.Related, evidence.Pods) {
			t.Fatalf("symptom: %+v", s)
		}
	}
}

func TestUnobservedCausesLowerConfidence(t *testing.T) {
	nodes := with(healthy(), evidence.Deployment, doctor.StatusUnknown)
	nodes = with(nodes, evidence.Pods, doctor.StatusFail)
	f := evidence.Diagnose(evidence.Of(nodes))
	if f[0].Kind != evidence.PossibleCause || f[0].Confidence != evidence.Low ||
		!slices.Equal(f[0].Related, []evidence.Layer{evidence.Deployment}) {
		t.Fatalf("%+v", f[0])
	}
}

func TestNothingObservedIsUnknownNotFine(t *testing.T) {
	f := evidence.Diagnose(evidence.Of(with(healthy(), evidence.DNS, doctor.StatusUnknown)))
	if len(f) != 1 || f[0].Kind != evidence.Unknown || f[0].Status != doctor.StatusUnknown {
		t.Fatalf("%+v", f)
	}
}

func TestIndependentFailuresAreBothRootCauses(t *testing.T) {
	nodes := with(healthy(), evidence.DNS, doctor.StatusFail)
	nodes = with(nodes, evidence.Pods, doctor.StatusFail)
	nodes = with(nodes, evidence.TLS, doctor.StatusFail)
	f := evidence.Diagnose(evidence.Of(nodes))
	var roots []evidence.Layer
	for _, x := range f {
		if x.Kind == evidence.RootCause {
			roots = append(roots, x.Layer)
		}
	}
	if !slices.Equal(roots, []evidence.Layer{evidence.Pods, evidence.DNS}) || f[len(f)-1].Layer != evidence.TLS {
		t.Fatalf("%+v", f)
	}
	if f := evidence.Diagnose(evidence.Of(with(healthy(), evidence.Gateway, doctor.StatusWarn))); f[0].Kind != evidence.RootCause {
		t.Fatalf("a warning is a root cause: %+v", f)
	}
}

// The wire form serde gave the Rust types.
func TestTheGraphAndFindingsSerializeAsInRust(t *testing.T) {
	g := evidence.Of([]evidence.Node{
		evidence.NewNode(evidence.Pods, doctor.StatusFail, "Pods of web").Fact("0/1 ready").WithAction("look at the logs").At(7),
		evidence.NewNode(evidence.Endpoints, doctor.StatusOK, "Endpoints web"),
	})
	data, err := json.Marshal(struct {
		Graph    evidence.Graph     `json:"graph"`
		Findings []evidence.Finding `json:"findings"`
	}{g, evidence.Diagnose(g)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"graph":{"nodes":[{"layer":"pods","status":"fail","subject":"Pods of web","evidence":["0/1 ready"],"observedAt":7,"action":"look at the logs"},` +
		`{"layer":"endpoints","status":"ok","subject":"Endpoints web","evidence":[],"observedAt":null,"action":null}],"edges":[{"from":"pods","to":"endpoints"}]},` +
		`"findings":[{"kind":"rootCause","layer":"pods","status":"fail","confidence":"high","summary":"Pods of web","evidence":["0/1 ready"],"related":[],"action":"look at the logs"}]}`
	if string(data) != want {
		t.Fatalf("got  %s\nwant %s", data, want)
	}
	empty, err := json.Marshal(evidence.Of(nil))
	if err != nil || string(empty) != `{"nodes":[],"edges":[]}` {
		t.Fatalf("an empty graph: %s %v", empty, err)
	}
}

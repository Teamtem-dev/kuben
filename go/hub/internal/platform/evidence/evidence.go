// Package evidence is the Doctor's evidence graph (M5.5;
// crates/kuben-platform/src/evidence.rs).
//
// Every layer between an app's source and a visitor is a node, with what
// was observed of it: build → release → deployment → pods → endpoints
// (with the Service) → route (with the Gateway) → reachable, and DNS and
// the Gateway → TLS → reachable.
//
// An edge points from a cause to what it affects. [Diagnose] turns the
// graph into ranked findings:
//   - A failing node whose causes are all fine is a root cause, with high
//     confidence.
//   - A failing node below another failing node is a symptom of it.
//   - A failing node below a node that could not be observed is a possible
//     root cause, with low confidence: nothing unobserved is assumed fine.
//   - With no failure but unobserved nodes, the answer is unknown, not
//     healthy.
package evidence

import (
	"cmp"
	"slices"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
)

// Layer is a layer of the path; its wire form is the camelCase name.
type Layer string

// The layers, in the Rust enum's order (which ranks them upstream first).
const (
	Build      Layer = "build"
	Release    Layer = "release"
	Deployment Layer = "deployment"
	Pods       Layer = "pods"
	Service    Layer = "service"
	Endpoints  Layer = "endpoints"
	Gateway    Layer = "gateway"
	Route      Layer = "route"
	DNS        Layer = "dns"
	TLS        Layer = "tls"
)

// order is the Rust enum's Ord.
func (l Layer) order() int {
	switch l {
	case Build:
		return 0
	case Release:
		return 1
	case Deployment:
		return 2
	case Pods:
		return 3
	case Service:
		return 4
	case Endpoints:
		return 5
	case Gateway:
		return 6
	case Route:
		return 7
	case DNS:
		return 8
	case TLS:
		return 9
	}
	return 10
}

// Causes are the layers l depends on.
func (l Layer) Causes() []Layer {
	switch l {
	case Build, Gateway, DNS, Service:
		return nil
	case Release:
		return []Layer{Build}
	case Deployment:
		return []Layer{Release}
	case Pods:
		return []Layer{Deployment}
	case Endpoints:
		return []Layer{Pods, Service}
	case Route:
		return []Layer{Endpoints, Gateway}
	case TLS:
		return []Layer{DNS, Gateway}
	}
	return nil
}

func byOrder(a, b Layer) int { return cmp.Compare(a.order(), b.order()) }

// Node is what was observed of one layer.
type Node struct {
	Layer  Layer         `json:"layer"`
	Status doctor.Status `json:"status"`
	// Subject is one line on what the layer is (`Deployment web-web`).
	Subject string `json:"subject"`
	// Evidence is what was seen, one fact per entry.
	Evidence []string `json:"evidence"`
	// ObservedAt is the observation's unix milliseconds, when known.
	ObservedAt opt.Val[int64] `json:"observedAt"`
	// Action is what to do when it is not fine.
	Action opt.Val[string] `json:"action"`
}

// NewNode is a node without facts.
func NewNode(layer Layer, status doctor.Status, subject string) Node {
	return Node{Layer: layer, Status: status, Subject: subject, Evidence: []string{}}
}

// Fact is n with one more fact.
func (n Node) Fact(fact string) Node {
	n.Evidence = append(slices.Clone(n.Evidence), fact)
	return n
}

// WithAction is n with what to do.
func (n Node) WithAction(action string) Node {
	n.Action = opt.Some(action)
	return n
}

// At is n observed at unix milliseconds observedAt.
func (n Node) At(observedAt int64) Node {
	n.ObservedAt = opt.Some(observedAt)
	return n
}

// Edge points from a cause to what it affects.
type Edge struct {
	From Layer `json:"from"`
	To   Layer `json:"to"`
}

// Graph is the observed path; layers that do not apply (no build for an
// image app, no route for a worker) are left out.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Of is the graph of nodes (one per layer, the first kept), with the edges
// between those present.
func Of(nodes []Node) Graph {
	nodes = slices.Clone(nodes)
	slices.SortStableFunc(nodes, func(a, b Node) int { return byOrder(a.Layer, b.Layer) })
	nodes = slices.CompactFunc(nodes, func(a, b Node) bool { return a.Layer == b.Layer })
	edges := []Edge{}
	for _, n := range nodes {
		for _, c := range n.Layer.Causes() {
			if slices.ContainsFunc(nodes, func(m Node) bool { return m.Layer == c }) {
				edges = append(edges, Edge{From: c, To: n.Layer})
			}
		}
	}
	if nodes == nil {
		nodes = []Node{}
	}
	return Graph{Nodes: nodes, Edges: edges}
}

func (g Graph) node(layer Layer) (Node, bool) {
	i := slices.IndexFunc(g.Nodes, func(n Node) bool { return n.Layer == layer })
	if i < 0 {
		return Node{}, false
	}
	return g.Nodes[i], true
}

// ancestors is every layer layer depends on, directly or not, in layer
// order.
func (g Graph) ancestors(layer Layer) []Layer {
	var seen []Layer
	todo := []Layer{layer}
	for len(todo) > 0 {
		current := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, e := range g.Edges {
			if e.To == current && !slices.Contains(seen, e.From) {
				seen = append(seen, e.From)
				todo = append(todo, e.From)
			}
		}
	}
	slices.SortFunc(seen, byOrder)
	return seen
}

// Confidence is how sure a finding is.
type Confidence string

// The confidences, weakest first.
const (
	Low    Confidence = "low"
	Medium Confidence = "medium"
	High   Confidence = "high"
)

// Kind is what a finding says of its layer.
type Kind string

// The kinds of finding, in the order they are reported.
const (
	RootCause     Kind = "rootCause"
	PossibleCause Kind = "possibleCause"
	Symptom       Kind = "symptom"
	Unknown       Kind = "unknown"
)

func (k Kind) rank() int {
	switch k {
	case RootCause:
		return 0
	case PossibleCause:
		return 1
	case Symptom:
		return 2
	case Unknown:
		return 3
	}
	return 3
}

// Finding is one conclusion of the Doctor.
type Finding struct {
	Kind       Kind          `json:"kind"`
	Layer      Layer         `json:"layer"`
	Status     doctor.Status `json:"status"`
	Confidence Confidence    `json:"confidence"`
	Summary    string        `json:"summary"`
	Evidence   []string      `json:"evidence"`
	// Related are the failing layers this one explains, or that may
	// explain it.
	Related []Layer         `json:"related"`
	Action  opt.Val[string] `json:"action"`
}

func failing(s doctor.Status) bool { return s == doctor.StatusFail || s == doctor.StatusWarn }

// Diagnose is the ranked findings of g: root causes first, then possible
// causes, symptoms and unknowns; worse before milder; upstream before
// downstream.
func Diagnose(g Graph) []Finding {
	findings := []Finding{}
	explained := map[Layer][]Layer{}
	withStatus := func(layers []Layer, status doctor.Status) []Layer {
		out := []Layer{}
		for _, l := range layers {
			if n, ok := g.node(l); ok && n.Status == status {
				out = append(out, l)
			}
		}
		return out
	}
	for _, n := range g.Nodes {
		if !failing(n.Status) {
			continue
		}
		ancestors := g.ancestors(n.Layer)
		failingCauses := withStatus(ancestors, doctor.StatusFail)
		unknownCauses := withStatus(ancestors, doctor.StatusUnknown)
		f := Finding{
			Layer: n.Layer, Status: n.Status, Summary: n.Subject, Evidence: n.Evidence, Related: []Layer{}, Action: n.Action,
		}
		switch {
		case len(failingCauses) > 0 && n.Status == doctor.StatusFail:
			for _, cause := range failingCauses {
				explained[cause] = append(explained[cause], n.Layer)
			}
			f.Kind, f.Confidence, f.Related = Symptom, Medium, failingCauses
		case len(unknownCauses) > 0:
			f.Kind, f.Confidence, f.Related = PossibleCause, Low, unknownCauses
		default:
			f.Kind, f.Confidence = RootCause, High
			if len(n.Evidence) == 0 {
				f.Confidence = Medium
			}
		}
		findings = append(findings, f)
	}
	for i, f := range findings {
		if effects, ok := explained[f.Layer]; ok && f.Kind == RootCause {
			findings[i].Related = effects
		}
	}
	if len(findings) == 0 {
		for _, n := range g.Nodes {
			if n.Status == doctor.StatusUnknown {
				findings = append(findings, Finding{
					Kind: Unknown, Layer: n.Layer, Status: doctor.StatusUnknown, Confidence: Low,
					Summary: n.Subject, Evidence: n.Evidence, Related: []Layer{}, Action: n.Action,
				})
			}
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Kind.rank(), b.Kind.rank()),
			cmp.Compare(b.Status.Rank(), a.Status.Rank()),
			byOrder(a.Layer, b.Layer),
		)
	})
	return findings
}

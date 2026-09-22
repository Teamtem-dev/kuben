// Package render is the renderer (WS-G, M1.9c, ADR-026): an App's spec and
// the cluster's capabilities become a RenderPlan — the normalized resources
// the App controller would build for it, their inventory and digests —
// frozen once per deployment run and never rendered again for that
// generation. It replaces crates/kuben-platform/src/render/mod.rs and
// carries the part of controller::resources (and the listener names of
// controller::gateway) the plan is built from.
//
// The plan is a content-addressed contract: its canonical JSON and its
// `sha256:` digests are stored, so the Go renderer produces the Rust bytes
// for the same input (pinned by the golden snapshots of the Rust tests).
// Normalization:
//
//   - owner references are left out: the object that owns the children
//     (the ApplicationRuntime, M1.9) exists only at apply time, and the
//     agent adds it there;
//   - status and null fields are dropped;
//   - objects are ordered by kind (volumes, workloads, autoscalers, cron
//     jobs, service, grant, route) and then by name;
//   - the JSON is canonical (wire.Canonical): object keys sorted, no
//     whitespace.
//
// A plan larger than the envelope bound (v1alpha1.MaxEnvelopeBytes) is
// refused.
package render

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// RendererVersion is recorded with every plan; a renderer change that
// alters output is a new version, and older plans are never rendered again
// with it.
const RendererVersion = "kuben-renderer/2"

// Plan is a rendered, normalized plan, ready to freeze and to carry in an
// ApplicationRuntime envelope.
type Plan struct {
	Capabilities Capabilities
	// resources is the canonical JSON of the resources: an array of
	// objects, in apply order.
	resources string
	Inventory []v1alpha1.InventoryItem
	// Digest is `sha256:` of the canonical resources.
	Digest string
	// InventoryHash is `sha256:` of the canonical inventory (kinds and
	// names only).
	InventoryHash string
}

// RendererVersion is the version of the renderer that made the plan.
func (Plan) RendererVersion() string { return RendererVersion }

// ResourcesJSON is the resources as canonical JSON.
func (p Plan) ResourcesJSON() string { return p.resources }

// Resources is the resources as a JSON value (for the SQL row), numbers
// kept as their text.
func (p Plan) Resources() (any, error) {
	v, err := wire.DecodeAny([]byte(p.resources))
	if err != nil {
		return nil, fmt.Errorf("plan resources: %w", err)
	}
	return v, nil
}

// CapabilitiesJSON is the capability snapshot as JSON (for the SQL row).
func (p Plan) CapabilitiesJSON() (json.RawMessage, error) {
	data, err := json.Marshal(p.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("plan capabilities: %w", err)
	}
	return data, nil
}

// Envelope is the envelope an ApplicationRuntime carries for this plan,
// frozen as planID.
func (p Plan) Envelope(planID string) v1alpha1.PlanEnvelope {
	return v1alpha1.PlanEnvelope{
		ID:              planID,
		RendererVersion: RendererVersion,
		Digest:          p.Digest,
		Resources:       p.resources,
	}
}

// Render renders app against capabilities. Errors are a *BuildError (the
// controller would refuse the spec too), a *TooLargeError, an
// *IncompleteError, or a serialization failure.
func Render(app *v1alpha1.App, capabilities Capabilities) (Plan, error) {
	desired, err := Build(app, capabilities.Platform(), unowned(app))
	if err != nil {
		return Plan{}, err
	}
	var objects []object
	objects = append(objects, desired.Volumes...)
	objects = append(objects, desired.Deployments...)
	objects = append(objects, desired.Autoscalers...)
	objects = append(objects, desired.CronJobs...)
	for _, o := range []interface{ Get() (object, bool) }{desired.Service, desired.Grant, desired.Route} {
		if v, ok := o.Get(); ok {
			objects = append(objects, v)
		}
	}
	type entry struct {
		item   v1alpha1.InventoryItem
		object object
	}
	entries := make([]entry, 0, len(objects))
	for _, o := range objects {
		normalize(o)
		it, err := item(o)
		if err != nil {
			return Plan{}, err
		}
		entries = append(entries, entry{it, o})
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		if ra, rb := rank(a.item.Kind), rank(b.item.Kind); ra != rb {
			return ra - rb
		}
		switch {
		case a.item.Name < b.item.Name:
			return -1
		case a.item.Name > b.item.Name:
			return 1
		}
		return 0
	})
	inventory := make([]v1alpha1.InventoryItem, 0, len(entries))
	sorted := make([]any, 0, len(entries))
	for _, e := range entries {
		inventory = append(inventory, e.item)
		sorted = append(sorted, e.object)
	}
	resources, err := wire.CanonicalValue(sorted)
	if err != nil {
		return Plan{}, fmt.Errorf("a rendered object does not serialize: %w", err)
	}
	if len(resources) > v1alpha1.MaxEnvelopeBytes {
		return Plan{}, &TooLargeError{Bytes: len(resources), Max: v1alpha1.MaxEnvelopeBytes}
	}
	inventoryText, err := wire.CanonicalValue(inventory)
	if err != nil {
		return Plan{}, fmt.Errorf("a rendered object does not serialize: %w", err)
	}
	return Plan{
		Capabilities:  capabilities,
		resources:     resources,
		Inventory:     inventory,
		Digest:        wire.SHA256(resources),
		InventoryHash: wire.SHA256(inventoryText),
	}, nil
}

// unowned is the owner the builder wants; the plan leaves it out
// (normalize).
func unowned(app *v1alpha1.App) OwnerReference {
	return OwnerReference{
		APIVersion:         v1alpha1.SchemeGroupVersion.String(),
		Kind:               v1alpha1.AppKind,
		Name:               app.Name,
		Controller:         true,
		BlockOwnerDeletion: true,
	}
}

func normalize(o object) {
	delete(o, "status")
	if meta, ok := o["metadata"].(object); ok {
		// The metadata fields the API server owns.
		for _, key := range []string{
			"ownerReferences", "uid", "resourceVersion", "generation", "managedFields", "creationTimestamp",
		} {
			delete(meta, key)
		}
	}
	dropNulls(o)
}

func dropNulls(v any) {
	switch x := v.(type) {
	case object:
		for k, item := range x {
			if item == nil {
				delete(x, k)
				continue
			}
			dropNulls(item)
		}
	case []any:
		for _, item := range x {
			dropNulls(item)
		}
	}
}

func item(o object) (v1alpha1.InventoryItem, error) {
	field := func(what string, path ...string) (string, error) {
		var cur any = o
		for _, key := range path {
			m, ok := cur.(object)
			if !ok {
				return "", &IncompleteError{What: what}
			}
			cur = m[key]
		}
		s, ok := cur.(string)
		if !ok {
			return "", &IncompleteError{What: what}
		}
		return s, nil
	}
	apiVersion, err := field("apiVersion", "apiVersion")
	if err != nil {
		return v1alpha1.InventoryItem{}, err
	}
	kind, err := field("kind", "kind")
	if err != nil {
		return v1alpha1.InventoryItem{}, err
	}
	name, err := field("metadata.name", "metadata", "name")
	if err != nil {
		return v1alpha1.InventoryItem{}, err
	}
	return v1alpha1.InventoryItem{APIVersion: apiVersion, Kind: kind, Name: name}, nil
}

// rank is the apply order: storage before the workloads that mount it,
// workloads before what points at them.
func rank(kind string) int {
	switch kind {
	case "PersistentVolumeClaim":
		return 0
	case "Deployment":
		return 1
	case "HorizontalPodAutoscaler":
		return 2
	case "CronJob":
		return 3
	case "Service":
		return 4
	case "ReferenceGrant":
		// The grant first: a listener can read the app's certificate once
		// the route arrives.
		return 5
	case "HTTPRoute":
		return 6
	}
	return 9
}

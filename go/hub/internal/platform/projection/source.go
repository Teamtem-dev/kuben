package projection

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/stream"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/metrics"
)

// Visibility is the per-connection tenant filter of the event stream
// (Invariant I-1): the snapshot and the deltas are filtered to the
// caller's orgs, and deletes are only forwarded for objects the connection
// has seen, so not even names of other tenants' objects leak.
type Visibility struct {
	orgs map[string]struct{}
	seen map[string]struct{}
}

// NewVisibility is the filter of a caller in orgs that has seen nothing.
func NewVisibility(orgs []string) *Visibility {
	v := &Visibility{orgs: make(map[string]struct{}, len(orgs)), seen: map[string]struct{}{}}
	for _, o := range orgs {
		v.orgs[o] = struct{}{}
	}
	return v
}

func (v *Visibility) allowed(org opt.Val[string]) bool {
	o, ok := org.Get()
	if !ok {
		return false
	}
	_, in := v.orgs[o]
	return in
}

// inOrgs is the snapshot's rule: an absent org counts as "" (the Rust
// unwrap_or_default), which no real org id is.
func (v *Visibility) inOrgs(org opt.Val[string]) bool {
	_, in := v.orgs[org.Or("")]
	return in
}

func (v *Visibility) track(key string, org opt.Val[string]) bool {
	if v.allowed(org) {
		v.seen[key] = struct{}{}
		return true
	}
	delete(v.seen, key)
	return false
}

func (v *Visibility) forget(key string) bool {
	_, ok := v.seen[key]
	delete(v.seen, key)
	return ok
}

func retain[V any](items []*V, keep func(*V) bool) []*V {
	out := make([]*V, 0, len(items))
	for _, item := range items {
		if keep(item) {
			out = append(out, item)
		}
	}
	return out
}

// Snapshot filters s to the caller's orgs and remembers what was sent.
func (v *Visibility) Snapshot(s Snapshot) Snapshot {
	s.Pods = retain(s.Pods, func(p *PodView) bool { return v.inOrgs(p.Org) })
	s.Projects = retain(s.Projects, func(p *ProjectView) bool { return v.inOrgs(p.Org) })
	s.Environments = retain(s.Environments, func(e *EnvironmentView) bool { return v.inOrgs(e.Org) })
	s.Apps = retain(s.Apps, func(a *AppView) bool { return v.inOrgs(a.Org) })
	for _, p := range s.Pods {
		v.seen["pod:"+p.Key] = struct{}{}
	}
	for _, p := range s.Projects {
		v.seen["project:"+p.Name] = struct{}{}
	}
	for _, e := range s.Environments {
		v.seen["environment:"+e.Name] = struct{}{}
	}
	for _, a := range s.Apps {
		v.seen["app:"+a.Key] = struct{}{}
	}
	return s
}

// Admit reports whether d may be sent on the connection.
func (v *Visibility) Admit(d Delta) bool {
	switch d := d.(type) {
	case PodUpsert:
		return d.Pod != nil && v.track("pod:"+d.Pod.Key, d.Pod.Org)
	case PodDelete:
		return v.forget("pod:" + d.Key)
	case ProjectUpsert:
		return d.Project != nil && v.track("project:"+d.Project.Name, d.Project.Org)
	case ProjectDelete:
		return v.forget("project:" + d.Key)
	case EnvironmentUpsert:
		return d.Environment != nil && v.track("environment:"+d.Environment.Name, d.Environment.Org)
	case EnvironmentDelete:
		return v.forget("environment:" + d.Key)
	case AppUpsert:
		return d.App != nil && v.track("app:"+d.App.Key, d.App.Org)
	case AppDelete:
		return v.forget("app:" + d.Key)
	case ExposureChanged:
		// The key is an app's; only a client that sees the app hears it.
		_, ok := v.seen["app:"+d.Key]
		return ok
	case Resync:
		return true
	}
	return false
}

// Source is the event stream's source (stream.Source) on the projections.
type Source struct {
	p       *Projections
	metrics *metrics.Metrics
}

// NewSource is the stream source reading p; missed deltas are counted in m
// (`kuben_sse_lagged_total`; nil counts nothing).
func NewSource(p *Projections, m *metrics.Metrics) Source { return Source{p: p, metrics: m} }

func raw[V any](items []*V) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		data, err := encode(item)
		if err != nil {
			continue // views are plain data: they always encode
		}
		out = append(out, data)
	}
	return out
}

// Snapshot is everything orgs may see, at the current sequence.
func (s Source) Snapshot(orgs []string) stream.Snapshot {
	snap := NewVisibility(orgs).Snapshot(s.p.Snapshot())
	return stream.Snapshot{
		Seq:          snap.Seq,
		Pods:         raw(snap.Pods),
		Projects:     raw(snap.Projects),
		Environments: raw(snap.Environments),
		Apps:         raw(snap.Apps),
	}
}

// Subscribe streams the deltas orgs may see until ctx ends, then closes
// the channel. The stream takes the snapshot after subscribing (so no
// delta falls between the two); the connection's filter starts from its
// own snapshot, taken here right after subscribing: whatever changes in
// between arrives as a delta and is tracked by it. A connection that fell
// behind gets a `resync` event carrying the number of missed deltas and no
// id, as the Rust stream sent on a broadcast lag.
func (s Source) Subscribe(ctx context.Context, orgs []string) <-chan stream.Delta {
	sub := s.p.Subscribe()
	visibility := NewVisibility(orgs)
	visibility.Snapshot(s.p.Snapshot())
	out := make(chan stream.Delta)
	// Owned by the request: it ends with ctx and closes out.
	go func() { //nolint:forbidigo // bounded by ctx, like stream.Empty
		defer close(out)
		defer sub.Close()
		for {
			got, ok := sub.Recv(ctx)
			if !ok {
				return
			}
			if got.Lagged > 0 {
				s.metrics.SSELagged(got.Lagged)
			}
			event, send := toEvent(got, visibility)
			if !send {
				continue
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// toEvent is the stream event of got, if the connection may see it.
func toEvent(got Received, visibility *Visibility) (stream.Delta, bool) {
	if got.Lagged > 0 {
		return stream.Delta{Name: "resync", Data: []byte(strconv.FormatUint(got.Lagged, 10))}, true
	}
	d := got.Delta
	if d == nil || !visibility.Admit(d) {
		return stream.Delta{}, false
	}
	data, err := d.MarshalJSON()
	if err != nil {
		return stream.Delta{Name: "error", Seq: d.Sequence(), Data: []byte("serialize")}, true
	}
	name := "delta"
	if _, ok := d.(Resync); ok {
		name = "resync"
	}
	return stream.Delta{Name: name, Seq: d.Sequence(), Data: data}, true
}

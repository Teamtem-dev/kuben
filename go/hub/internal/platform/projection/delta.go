package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
)

// Delta is a change notification. Every delta carries the global sequence
// so clients can resume with Last-Event-ID. Its JSON is the Rust enum's
// (serde tag "kind", snake_case): `{"kind":"pod_upsert","seq":1,"pod":…}`.
//
//sumtype:decl
type Delta interface {
	json.Marshaler
	// Sequence is the delta's global sequence number.
	Sequence() uint64
	isDelta()
}

// PodUpsert: a pod appeared or changed.
type PodUpsert struct {
	Seq uint64
	Pod *PodView
}

// PodDelete: the pod Key (`namespace/name`) is gone.
type PodDelete struct {
	Seq uint64
	Key string
}

// ProjectUpsert: a project appeared or changed.
type ProjectUpsert struct {
	Seq     uint64
	Project *ProjectView
}

// ProjectDelete: the project Key is gone.
type ProjectDelete struct {
	Seq uint64
	Key string
}

// EnvironmentUpsert: an environment appeared or changed.
type EnvironmentUpsert struct {
	Seq         uint64
	Environment *EnvironmentView
}

// EnvironmentDelete: the environment Key is gone.
type EnvironmentDelete struct {
	Seq uint64
	Key string
}

// AppUpsert: an app appeared or changed.
type AppUpsert struct {
	Seq uint64
	App *AppView
}

// AppDelete: the app Key (`namespace/name`) is gone.
type AppDelete struct {
	Seq uint64
	Key string
}

// ExposureChanged: the route of the app Key, or one of its certificates,
// changed; so did its exposure.
type ExposureChanged struct {
	Seq uint64
	Key string
}

// Resync: a watch was re-listed; clients must refetch the snapshot.
type Resync struct {
	Seq uint64
}

func (d PodUpsert) Sequence() uint64         { return d.Seq }
func (d PodDelete) Sequence() uint64         { return d.Seq }
func (d ProjectUpsert) Sequence() uint64     { return d.Seq }
func (d ProjectDelete) Sequence() uint64     { return d.Seq }
func (d EnvironmentUpsert) Sequence() uint64 { return d.Seq }
func (d EnvironmentDelete) Sequence() uint64 { return d.Seq }
func (d AppUpsert) Sequence() uint64         { return d.Seq }
func (d AppDelete) Sequence() uint64         { return d.Seq }
func (d ExposureChanged) Sequence() uint64   { return d.Seq }
func (d Resync) Sequence() uint64            { return d.Seq }

func (PodUpsert) isDelta()         {}
func (PodDelete) isDelta()         {}
func (ProjectUpsert) isDelta()     {}
func (ProjectDelete) isDelta()     {}
func (EnvironmentUpsert) isDelta() {}
func (EnvironmentDelete) isDelta() {}
func (AppUpsert) isDelta()         {}
func (AppDelete) isDelta()         {}
func (ExposureChanged) isDelta()   {}
func (Resync) isDelta()            {}

// keyed is the JSON of a delete or an exposure change.
type keyed struct {
	Kind string `json:"kind"`
	Seq  uint64 `json:"seq"`
	Key  string `json:"key"`
}

// MarshalJSON writes the tagged form.
func (d PodUpsert) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Kind string   `json:"kind"`
		Seq  uint64   `json:"seq"`
		Pod  *PodView `json:"pod"`
	}{"pod_upsert", d.Seq, d.Pod})
}

// MarshalJSON writes the tagged form.
func (d PodDelete) MarshalJSON() ([]byte, error) { return encode(keyed{"pod_delete", d.Seq, d.Key}) }

// MarshalJSON writes the tagged form.
func (d ProjectUpsert) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Kind    string       `json:"kind"`
		Seq     uint64       `json:"seq"`
		Project *ProjectView `json:"project"`
	}{"project_upsert", d.Seq, d.Project})
}

// MarshalJSON writes the tagged form.
func (d ProjectDelete) MarshalJSON() ([]byte, error) {
	return encode(keyed{"project_delete", d.Seq, d.Key})
}

// MarshalJSON writes the tagged form.
func (d EnvironmentUpsert) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Kind        string           `json:"kind"`
		Seq         uint64           `json:"seq"`
		Environment *EnvironmentView `json:"environment"`
	}{"environment_upsert", d.Seq, d.Environment})
}

// MarshalJSON writes the tagged form.
func (d EnvironmentDelete) MarshalJSON() ([]byte, error) {
	return encode(keyed{"environment_delete", d.Seq, d.Key})
}

// MarshalJSON writes the tagged form.
func (d AppUpsert) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Kind string   `json:"kind"`
		Seq  uint64   `json:"seq"`
		App  *AppView `json:"app"`
	}{"app_upsert", d.Seq, d.App})
}

// MarshalJSON writes the tagged form.
func (d AppDelete) MarshalJSON() ([]byte, error) { return encode(keyed{"app_delete", d.Seq, d.Key}) }

// MarshalJSON writes the tagged form.
func (d ExposureChanged) MarshalJSON() ([]byte, error) {
	return encode(keyed{"exposure_changed", d.Seq, d.Key})
}

// MarshalJSON writes the tagged form.
func (d Resync) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Kind string `json:"kind"`
		Seq  uint64 `json:"seq"`
	}{"resync", d.Seq})
}

// encode is JSON without HTML escaping (serde does not escape <, > or &)
// and without the encoder's trailing newline.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Subscription receives the deltas published after it was made, in order,
// with at most DeltaCapacity waiting. A subscriber that falls further
// behind loses its backlog and is told how many deltas it missed (the Rust
// broadcast channel's Lagged), so memory never grows with a slow client.
type Subscription struct {
	owner  *Projections
	deltas chan Delta

	mu     sync.Mutex // guards lagged
	lagged uint64
	// wake has room for one signal: a lag happened.
	wake chan struct{}
}

// Subscribe starts a subscription; Close ends it.
func (p *Projections) Subscribe() *Subscription {
	s := &Subscription{owner: p, deltas: make(chan Delta, DeltaCapacity), wake: make(chan struct{}, 1)}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscribers[s] = struct{}{}
	return s
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	delete(s.owner.subscribers, s)
}

// offer queues d, or counts it as missed when the backlog is full. Called
// with the Projections mutex held; never blocks.
func (s *Subscription) offer(d Delta) {
	select {
	case s.deltas <- d:
		return
	default:
	}
	s.mu.Lock()
	s.lagged++
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Received is one result of Recv: a delta, or the number of deltas missed.
type Received struct {
	Delta Delta
	// Lagged, when non-zero, is how many deltas were missed; Delta is nil.
	Lagged uint64
}

// takeLag returns and resets the missed count, dropping the backlog (it is
// older than what a refetch returns) and counting it as missed too.
func (s *Subscription) takeLag() uint64 {
	s.mu.Lock()
	n := s.lagged
	s.lagged = 0
	s.mu.Unlock()
	if n == 0 {
		return 0
	}
	for {
		select {
		case <-s.deltas:
			n++
		default:
			return n
		}
	}
}

// Recv waits for the next delta or lag notice; false when ctx ended.
func (s *Subscription) Recv(ctx context.Context) (Received, bool) {
	if n := s.takeLag(); n > 0 {
		return Received{Lagged: n}, true
	}
	select {
	case <-ctx.Done():
		return Received{}, false
	case d := <-s.deltas:
		return Received{Delta: d}, true
	case <-s.wake:
		if n := s.takeLag(); n > 0 {
			return Received{Lagged: n}, true
		}
		return s.Recv(ctx)
	}
}

// TryRecv is Recv without waiting; false when nothing is pending.
func (s *Subscription) TryRecv() (Received, bool) {
	if n := s.takeLag(); n > 0 {
		return Received{Lagged: n}, true
	}
	select {
	case d := <-s.deltas:
		return Received{Delta: d}, true
	default:
		return Received{}, false
	}
}

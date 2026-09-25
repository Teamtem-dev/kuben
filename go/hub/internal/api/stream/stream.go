// Package stream is the one server-sent event stream per browser tab
// (crates/kuben-api/src/stream.rs, ADR-014): a `snapshot` event, then
// `delta` events, each with the global sequence as its id so EventSource
// resumes with Last-Event-ID, `resync` when a slow client fell behind, and
// a `ping` comment every 15 seconds. The stream is filtered to the caller's
// organizations (Invariant I-1).
//
// The cluster projections that fill it arrive with slice S1; until then a
// server without a cluster streams an empty snapshot, as the Rust server
// does without one.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Snapshot is everything the caller may see, at sequence Seq. The element
// types are the projection views (slice S1); they are opaque here.
type Snapshot struct {
	Seq          uint64            `json:"seq"`
	Pods         []json.RawMessage `json:"pods"`
	Projects     []json.RawMessage `json:"projects"`
	Environments []json.RawMessage `json:"environments"`
	Apps         []json.RawMessage `json:"apps"`
}

// Delta is one change, already serialized, with its event name (`delta` or
// `resync`).
type Delta struct {
	Name string
	Seq  uint64
	Data []byte
}

// Source is what the stream reads: a snapshot filtered to orgs, and the
// deltas after it that orgs may see. The channel closes when ctx ends.
type Source interface {
	Snapshot(orgs []string) Snapshot
	Subscribe(ctx context.Context, orgs []string) <-chan Delta
}

// Empty is the source of a server without a cluster: nothing, ever.
type Empty struct{}

// Snapshot is empty at sequence 0.
func (Empty) Snapshot([]string) Snapshot {
	return Snapshot{Pods: []json.RawMessage{}, Projects: []json.RawMessage{}, Environments: []json.RawMessage{}, Apps: []json.RawMessage{}}
}

// Subscribe never sends.
func (Empty) Subscribe(ctx context.Context, _ []string) <-chan Delta {
	ch := make(chan Delta)
	go func() { <-ctx.Done(); close(ch) }() //nolint:forbidigo // owned: ends with the request
	return ch
}

// KeepAlive is how often an idle stream sends a ping comment.
const KeepAlive = 15 * time.Second

// Serve streams src to w for a caller in orgs, until the request ends.
func Serve(w http.ResponseWriter, r *http.Request, src Source, orgs []string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("event stream: the response cannot be flushed")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	deltas := src.Subscribe(ctx, orgs)
	snap := src.Snapshot(orgs)
	// A reconnecting client that is still current only needs deltas.
	if last, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err != nil || last != snap.Seq {
		data, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("event stream: snapshot: %w", err)
		}
		if err := writeEvent(w, "snapshot", strconv.FormatUint(snap.Seq, 10), data); err != nil {
			return err
		}
		flusher.Flush()
	}
	// axum's KeepAlive: a comment after KeepAlive without an event.
	idle := time.NewTimer(KeepAlive)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, open := <-deltas:
			if !open {
				return nil
			}
			id := strconv.FormatUint(d.Seq, 10)
			if d.Name == "resync" && d.Seq == 0 {
				id = "" // a lag notice carries the count, not a sequence
			}
			if err := writeEvent(w, d.Name, id, d.Data); err != nil {
				return err
			}
			flusher.Flush()
			idle.Reset(KeepAlive)
		case <-idle.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return fmt.Errorf("event stream: %w", err)
			}
			flusher.Flush()
			idle.Reset(KeepAlive)
		}
	}
}

func writeEvent(w http.ResponseWriter, name, id string, data []byte) error {
	var b bytes.Buffer
	b.WriteString("event: " + name + "\n")
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := w.Write(b.Bytes()); err != nil {
		return fmt.Errorf("event stream: %w", err)
	}
	return nil
}

package discovery

import (
	"sync"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Watch holds the latest facts for any number of concurrent readers; it
// stands in for Rust's tokio watch channel. The zero Watch is ready to use
// and holds no facts until the first discovery.
//
// A reader that reacts to changes takes [Watch.Changed] before [Watch.Get]:
//
//	for {
//		changed := w.Changed()
//		facts, ok := w.Get()
//		// ... act on facts ...
//		select {
//		case <-ctx.Done():
//			return
//		case <-changed:
//		}
//	}
//
// A change published between the two calls, or while the reader is busy,
// has already closed the channel it holds, so no change is missed; changes
// that happen while the reader is busy are coalesced into one, as with the
// Rust channel. The facts are copied in and out, so no reader can change
// what another sees.
type Watch struct {
	mu sync.Mutex
	// facts is the latest facts, absent until the first discovery.
	facts opt.Val[ClusterFacts]
	// changed is closed by the next Publish that changes facts; nil until a
	// reader asks for it.
	changed chan struct{}
}

// Get is the facts now, and false before the first discovery.
func (w *Watch) Get() (ClusterFacts, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, ok := w.facts.Get()
	if !ok {
		return ClusterFacts{}, false
	}
	return f.clone(), true
}

// Changed is a channel closed when the facts next change.
func (w *Watch) Changed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	return w.changed
}

// Publish makes f the facts and wakes the readers, unless they equal the
// facts held already. It reports whether they changed.
func (w *Watch) Publish(f ClusterFacts) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.facts.Get(); ok && cur.Equal(f) {
		return false
	}
	w.facts = opt.Some(f.clone())
	if w.changed != nil {
		close(w.changed)
		w.changed = nil
	}
	return true
}

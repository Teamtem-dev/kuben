package projection

// Build changes on the event stream (2.1). Builds live in SQL, not in the
// cluster: the server's build feed reads their changes and publishes them
// here, where they get their sequence like every other delta.

// PublishBuild publishes that a build changed; d's Seq is set here. With
// nobody subscribed nothing is published and the sequence stays.
func (p *Projections) PublishBuild(d BuildChanged) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.subscribers) == 0 {
		return
	}
	p.publish(func(seq uint64) Delta {
		d.Seq = seq
		return d
	})
}

// Subscribed reports whether any stream is subscribed now.
func (p *Projections) Subscribed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subscribers) > 0
}

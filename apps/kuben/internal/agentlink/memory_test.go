package agentlink_test

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/agentlink"
	protocol "github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// memoryRegistry is a registry in memory (hub.rs MemoryRegistry).
type memoryRegistry struct {
	mu sync.Mutex
	// devices is cluster → its agent device and whether it is revoked.
	devices map[string]registeredDevice
	// observations is cluster → what its agent reported, oldest first.
	observations map[string][]protocol.Observation
	// certifyDelay is how long recording a certificate takes: none, or a
	// slow database.
	certifyDelay time.Duration
}

type registeredDevice struct {
	device  string
	revoked bool
}

func newMemoryRegistry(certifyDelay time.Duration) *memoryRegistry {
	return &memoryRegistry{devices: map[string]registeredDevice{}, observations: map[string][]protocol.Observation{}, certifyDelay: certifyDelay}
}

// observed is what the agent of cluster reported, oldest first.
func (r *memoryRegistry) observed(cluster string) []protocol.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.observations[cluster])
}

// revoke revokes the current device of cluster.
func (r *memoryRegistry) revoke(cluster string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[cluster]; ok {
		d.revoked = true
		r.devices[cluster] = d
	}
}

func (r *memoryRegistry) Admits(_ context.Context, cluster, device string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[cluster]
	return ok && d.device == device && !d.revoked
}

func (r *memoryRegistry) Certified(ctx context.Context, cluster, device string, _ time.Time) {
	if r.certifyDelay > 0 {
		select {
		case <-time.After(r.certifyDelay):
		case <-ctx.Done():
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The same device keeps its revocation; another one replaces it.
	if d, ok := r.devices[cluster]; !ok || d.device != device {
		r.devices[cluster] = registeredDevice{device: device}
	}
}

func (*memoryRegistry) Linked(context.Context, string, string, agentlink.SessionInfo) {}

func (*memoryRegistry) Heard(context.Context, string, string) {}

func (r *memoryRegistry) Observed(_ context.Context, cluster, _ string, o protocol.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations[cluster] = append(r.observations[cluster], o)
}

// memoryTokens are bootstrap tokens in memory (enroll.rs MemoryTokens).
type memoryTokens struct {
	mu     sync.Mutex
	tokens map[[32]byte]*tokenRecord
}

type tokenRecord struct {
	cluster   string
	expiresAt time.Time
	device    string
}

func newMemoryTokens() *memoryTokens {
	return &memoryTokens{tokens: map[[32]byte]*tokenRecord{}}
}

// issue is a token for cluster, valid for ttl from now; only its hash is
// kept.
func (m *memoryTokens) issue(cluster string, ttl time.Duration, now time.Time) string {
	token, err := agentlink.NewToken()
	if err != nil {
		panic(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[agentlink.TokenHash(token)] = &tokenRecord{cluster: cluster, expiresAt: now.Add(ttl)}
	return token
}

func (m *memoryTokens) Redeem(_ context.Context, hash [32]byte, cluster, device string, now time.Time) (agentlink.Redeemed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tokens[hash]
	switch {
	case !ok:
		return 0, agentlink.TokenUnknown
	case r.cluster != cluster:
		return 0, agentlink.TokenOtherCluster
	case r.device == device:
		if !now.Before(r.expiresAt.Add(agentlink.ResumeGrace)) {
			return 0, agentlink.TokenExpired
		}
		return agentlink.RedeemedResumed, nil
	case r.device != "":
		return 0, agentlink.TokenOtherDevice
	case !now.Before(r.expiresAt):
		return 0, agentlink.TokenExpired
	}
	r.device = device
	return agentlink.RedeemedFirst, nil
}

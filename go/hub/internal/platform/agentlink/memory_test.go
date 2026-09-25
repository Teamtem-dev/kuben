package agentlink_test

import (
	"context"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/agentlink"
)

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

package leader_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/leader"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// Two replicas: one leads while the other waits; when the leader shuts
// down it releases the Lease and the other takes over without waiting for
// it to expire.
func TestOneLeaderAndAFastHandOver(t *testing.T) {
	client := fake.NewClientset()
	var running atomic.Int32
	work := func(ctx context.Context) error {
		running.Add(1)
		defer running.Add(-1)
		<-ctx.Done()
		return nil
	}
	ctxA, stopA := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	go func() {
		doneA <- leader.Run(ctxA, client, leader.Election{Namespace: "kuben", Identity: "a"}, health.New(clock.System{}), quiet, work)
	}()
	waitFor(t, func() bool { return running.Load() == 1 })

	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()
	doneB := make(chan error, 1)
	go func() {
		doneB <- leader.Run(ctxB, client, leader.Election{Namespace: "kuben", Identity: "b"}, health.New(clock.System{}), quiet, work)
	}()
	time.Sleep(3 * time.Second)
	if running.Load() != 1 {
		t.Fatalf("two leaders: %d", running.Load())
	}

	stopA()
	if err := <-doneA; err != nil {
		t.Fatalf("a shutdown is not an error: %v", err)
	}
	start := time.Now()
	waitFor(t, func() bool { return running.Load() == 1 })
	if time.Since(start) > leader.LeaseDuration {
		t.Fatalf("the hand-over waited for expiry: %v", time.Since(start))
	}
	stopB()
	<-doneB
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

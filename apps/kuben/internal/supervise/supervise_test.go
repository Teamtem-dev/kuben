package supervise_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/supervise"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRestartsAfterFailureThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	h := health.New(clock.System{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	supervise.Run(ctx, "test", h, quiet, func(context.Context) error {
		if attempts.Add(1) <= 2 {
			return errors.New("transient")
		}
		return nil
	})
	if attempts.Load() != 3 || h.AnyDegraded() {
		t.Fatalf("attempts %d, degraded %v", attempts.Load(), h.AnyDegraded())
	}
}

func TestAPanicIsContained(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	supervise.Run(ctx, "panicky", health.New(clock.System{}), quiet, func(context.Context) error {
		if attempts.Add(1) == 1 {
			panic("boom")
		}
		return nil
	})
	if attempts.Load() != 2 {
		t.Fatalf("attempts %d", attempts.Load())
	}
}

func TestCancellationStopsTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := supervise.Go(ctx, "forever", health.New(clock.System{}), quiet, func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("cancelled")
	})
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

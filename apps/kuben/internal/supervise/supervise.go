// Package supervise runs a subsystem and restarts it with jittered
// exponential backoff when it fails or panics (crates/kuben-platform/src/
// supervise.rs). Every background goroutine of the server runs under it,
// so one failing part degrades /healthz/details instead of the process.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
)

// Backoff bounds: from half a second up to a minute, jittered. A subsystem
// that ran a minute before failing starts again at the shortest delay; only
// a crash loop backs off to the longest.
const (
	MinDelay   = 500 * time.Millisecond
	MaxDelay   = time.Minute
	resetAfter = time.Minute
)

// Run runs work until it succeeds or ctx ends, restarting it after an error
// or a panic. It returns when work returned nil or ctx is done.
func Run(ctx context.Context, name string, h *health.Health, logger *slog.Logger, work func(context.Context) error) {
	h.Starting(name)
	delay := MinDelay
	for {
		started := time.Now() //nolint:forbidigo // a duration measurement for the backoff, not a decision
		err := runOnce(ctx, work)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			h.OK(name)
			return
		}
		var p panicked
		if errors.As(err, &p) {
			h.Degrade(name, "panic")
			h.Metrics().SubsystemPanicked(name)
			logger.Error("subsystem panicked; restarting", "subsystem", name, "panic", p.value, "stack", p.stack)
		} else {
			h.Degrade(name, err.Error())
			h.Metrics().SubsystemFailed(name)
			logger.Error("subsystem failed; restarting", "subsystem", name, "error", err)
		}
		if time.Since(started) >= resetAfter {
			delay = MinDelay
		}
		wait := jitter(delay)
		delay = min(delay*2, MaxDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// runOnce runs work, turning a panic into an error.
func runOnce(ctx context.Context, work func(context.Context) error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = panicked{value: fmt.Sprint(v), stack: string(debug.Stack())}
		}
	}()
	return work(ctx)
}

// panicked is a panic of the work, recovered: counted and reported apart
// from errors, as the Rust supervisor did with a panicked task.
type panicked struct{ value, stack string }

func (p panicked) Error() string { return "panic: " + p.value }

// jitter is d scaled by a random factor in [0.5, 1.5).
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.5 + rand.Float64())) //nolint:gosec // jitter, not security
}

// Go starts Run in a goroutine and returns a channel closed when it ended,
// so the owner can wait for its subsystems at shutdown.
func Go(ctx context.Context, name string, h *health.Health, logger *slog.Logger, work func(context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	go func() { //nolint:forbidigo // this is the owner every background goroutine goes through
		defer close(done)
		Run(ctx, name, h, logger, work)
	}()
	return done
}

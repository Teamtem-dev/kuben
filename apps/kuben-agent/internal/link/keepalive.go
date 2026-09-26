package link

import (
	"context"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// frame is one read of the link.
type frame struct {
	message agentlink.Message
	open    bool
	err     error
}

// work is the envelopes a link is carrying out, and their observations.
type work struct {
	observations chan agentlink.Observation
	tasks        sync.WaitGroup
	// running cancels the task still carrying out a target's envelope.
	running map[string]context.CancelFunc
}

func newWork() *work {
	return &work{observations: make(chan agentlink.Observation, 64), running: map[string]context.CancelFunc{}}
}

// start hands apply to the executor in place of an older envelope of the
// same target; the observation to send at once when the agent cannot take
// it.
func (w *work) start(ctx context.Context, cfg Config, session Session, apply agentlink.Apply) (agentlink.Observation, bool) {
	rejected := func(reason string) (agentlink.Observation, bool) {
		return agentlink.Observation{Target: apply.Target, Phase: agentlink.RuntimePhaseRejected, Reason: &reason}, true
	}
	if session.Negotiated.Require(agentlink.ApplicationRuntime) != nil {
		return rejected("UnsupportedCapability")
	}
	if cfg.Executor == nil {
		return rejected("NoExecutor")
	}
	if older, ok := w.running[apply.Target]; ok {
		older()
	}
	ctx, cancel := context.WithCancel(ctx)
	w.running[apply.Target] = cancel
	executor := cfg.Executor
	w.tasks.Go(func() { executor.Apply(ctx, apply, w.observations) })
	return agentlink.Observation{}, false
}

// stop cancels every task and waits for them.
func (w *work) stop() {
	for _, cancel := range w.running {
		cancel()
	}
	w.tasks.Wait()
}

// KeepAlive keeps an established link alive until ctx ends (nil) or the
// link fails. Frames are read by a goroutine of their own, so a frame is
// never lost halfway through while a heartbeat goes out.
func KeepAlive(ctx context.Context, conn net.Conn, session Session, cfg Config) error {
	frames := make(chan frame, 16)
	stop := make(chan struct{})
	var reading sync.WaitGroup
	reading.Go(func() {
		for {
			m, open, err := agentlink.ReadFrame(conn)
			select {
			case frames <- frame{message: m, open: open, err: err}:
			case <-stop:
				return
			}
			if err != nil || !open {
				return
			}
		}
	})
	w := newWork()
	defer func() {
		w.stop()
		close(stop)
		conn.Close() //nolint:errcheck,gosec // unblocks the reader; the link is over
		reading.Wait()
	}()
	// The envelopes' tasks end with the link.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return heartbeats(ctx, conn, frames, session, cfg, w)
}

// beat is the state of the heartbeat loop.
type beat struct {
	seq        uint64
	lastAnswer time.Time
	jitter     float64
	renewing   bool
}

func heartbeats(ctx context.Context, conn net.Conn, frames <-chan frame, session Session, cfg Config, w *work) error {
	deadAfter := 3 * session.Heartbeat
	tick := time.NewTicker(session.Heartbeat)
	defer tick.Stop()
	b := beat{lastAnswer: time.Now(), jitter: rand.Float64()} //nolint:forbidigo,gosec // a duration measurement; jitter, not a secret
	// tokio's interval ticks at once: the first heartbeat goes out now.
	if err := b.heartbeat(conn, cfg, deadAfter); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case o := <-w.observations:
			if err := agentlink.WriteFrame(conn, o); err != nil {
				return err //nolint:wrapcheck // a frame error says what failed
			}
		case <-tick.C:
			if err := b.heartbeat(conn, cfg, deadAfter); err != nil {
				return err
			}
		case f := <-frames:
			if err := b.received(ctx, conn, f, session, cfg, w); err != nil {
				return err
			}
		}
	}
}

// heartbeat sends the next heartbeat, and a renewal request when one is
// due; an error when the hub stopped answering.
func (b *beat) heartbeat(conn net.Conn, cfg Config, deadAfter time.Duration) error {
	if time.Since(b.lastAnswer) > deadAfter {
		return HeartbeatTimeoutError{After: deadAfter}
	}
	b.seq++
	if err := agentlink.WriteFrame(conn, agentlink.Heartbeat{Seq: b.seq}); err != nil {
		return err //nolint:wrapcheck // a frame error says what failed
	}
	if b.renewing {
		return nil
	}
	renewal, due := renewalDue(cfg, b.jitter)
	if !due {
		return nil
	}
	csr, err := renewal.Key.CSR(cfg.ClusterID)
	if err != nil {
		cfg.Logger.Warn("cannot make a renewal request", "error", err)
		return nil
	}
	if err := agentlink.WriteFrame(conn, agentlink.Renew{CSR: csr}); err != nil {
		return err //nolint:wrapcheck // a frame error says what failed
	}
	b.renewing = true
	return nil
}

// received handles one read of the link.
func (b *beat) received(ctx context.Context, conn net.Conn, f frame, session Session, cfg Config, w *work) error {
	switch {
	case f.err != nil:
		return f.err
	case !f.open:
		return ErrClosed
	}
	switch m := f.message.(type) {
	case agentlink.HeartbeatAck:
		b.lastAnswer = time.Now() //nolint:forbidigo // a duration measurement, as tokio's Instant
	case agentlink.Enrolled:
		if b.renewing {
			b.renewing = false
			if err := adopt(cfg, m.Certificate); err != nil {
				cfg.Logger.Warn("cannot use the renewed certificate", "error", err)
			}
		}
	case agentlink.Apply:
		if rejected, ok := w.start(ctx, cfg, session, m); ok {
			return agentlink.WriteFrame(conn, rejected) //nolint:wrapcheck // a frame error says what failed
		}
	case agentlink.Refused:
		return RefusedError(m)
	case agentlink.Hello, agentlink.Welcome, agentlink.Heartbeat, agentlink.Enroll, agentlink.Renew,
		agentlink.Observation, agentlink.Unknown:
		// Messages of later steps, and ones this build does not know.
	}
	return nil
}

// renewalDue is the renewal to run now, if one is set up and due.
func renewalDue(cfg Config, jitter float64) (*Renewal, bool) {
	if cfg.Renewal == nil {
		return nil, false
	}
	lifetime, known := cfg.Credentials.Lifetime()
	if !known {
		return nil, false
	}
	return cfg.Renewal, !cfg.Now().Before(RenewAt(lifetime, jitter))
}

// adopt stores a renewed certificate, then dials with it from now on.
func adopt(cfg Config, pem string) error {
	renewal := cfg.Renewal
	if renewal == nil {
		return errNoRenewal
	}
	cert, err := agentlink.ParseCertificatePEM(pem)
	if err != nil {
		return err //nolint:wrapcheck // says what failed
	}
	if err := renewal.Store(pem); err != nil {
		return err
	}
	identity, err := renewal.Key.Identity(pem)
	if err != nil {
		return err //nolint:wrapcheck // says what failed
	}
	if err := cfg.Credentials.Replace(identity, Lifetime{NotBefore: cert.NotBefore, NotAfter: cert.NotAfter}); err != nil {
		return err
	}
	cfg.Logger.Info("certificate renewed", "not_after", cert.NotAfter)
	return nil
}

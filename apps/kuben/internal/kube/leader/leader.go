// Package leader elects the one replica that runs the reconcilers
// (crates/kuben-platform/src/leader.rs, ADR-023). Every replica serves the
// API and keeps its projections; only the holder of the
// coordination.k8s.io/v1 Lease reconciles.
//
// client-go's leader election implements the same protocol as the Rust
// code: every write is a compare-and-swap on the Lease's resourceVersion,
// and expiry is judged by how long this replica has seen the record
// unchanged, never by comparing renewTime with the local clock, so clock
// skew cannot make two leaders. On shutdown the Lease is released so a
// standby takes over at once.
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
)

// The Lease and its timing, as in Rust.
const (
	LeaseName     = "kuben-controller"
	LeaseDuration = 15 * time.Second
	RenewDeadline = 10 * time.Second
	RetryPeriod   = 2 * time.Second
	// Subsystem is the health entry of the controllers.
	Subsystem = "controllers"
)

// Election is where the Lease lives and who campaigns.
type Election struct {
	Namespace string
	// Identity is unique per process, e.g. `<host>_<random>`.
	Identity string
}

// Identity is this process among the replicas: the host name and a random
// suffix, for the Lease and the materializer's claims.
func Identity() string {
	host := os.Getenv("HOSTNAME")
	if host == "" {
		host = "kuben"
	}
	var b [4]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails (Go ≥ 1.24)
	return host + "_" + hex.EncodeToString(b[:])
}

// ErrLost is returned when this replica stopped leading before shutdown.
var ErrLost = errors.New("lost the controller lease")

// Run campaigns for the Lease and runs work while holding it. It returns
// nil once ctx ends (after releasing the Lease), and ErrLost or work's
// error when leadership ends otherwise, so the supervisor campaigns again.
// work is always stopped before Run returns.
func Run(ctx context.Context, client kubernetes.Interface, e Election, h *health.Health, logger *slog.Logger,
	work func(context.Context) error,
) error {
	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, e.Namespace, LeaseName,
		client.CoreV1(), client.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: e.Identity})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}
	h.Standby(Subsystem)
	logger.Info("waiting for the controller lease", "identity", e.Identity, "namespace", e.Namespace, "lease", LeaseName)
	workErr := make(chan error, 1)
	var leading atomic.Bool
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   LeaseDuration,
		RenewDeadline:   RenewDeadline,
		RetryPeriod:     RetryPeriod,
		ReleaseOnCancel: true,
		Name:            LeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				leading.Store(true)
				h.Metrics().Leading(true)
				logger.Info("acquired the controller lease; starting controllers", "identity", e.Identity)
				workErr <- work(lctx)
			},
			OnStoppedLeading: func() { h.Metrics().Leading(false) },
		},
	})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { //nolint:forbidigo // owned: Run waits for it through workErr or ctx
		elector.Run(runCtx)
		stop()
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-workErr:
		stop()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("controllers: %w", err)
		}
		return ErrLost
	case <-runCtx.Done():
		if ctx.Err() != nil || !leading.Load() {
			return nil
		}
		return ErrLost
	}
}

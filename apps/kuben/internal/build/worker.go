package build

// The build worker (ADR-028; worker.rs): claims `source.sync` and `build`
// operations.
//
//   - A sync reads the branch head from the provider and records it
//     (store.Tenant.ObserveHead); a new head queues a build.
//   - A build takes a slot, creates its BuildRun, fetch-token Secret and Job
//     (job.go), follows the Job (steps.go) and, when the pod reports a
//     digest that the registry confirms, completes the attempt: release
//     and, if the target still wants it, a deployment run.
//   - A stop deletes the Job and waits until it is gone. Every final attempt
//     revokes its fetch token and deletes its objects before its operation
//     is settled, so a crash in between only repeats the cleanup.
//
// Claims are fenced in SQL, so any number of replicas may run workers.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// Subsystem is the health entry of the worker loop.
const Subsystem = "builds"

// Timing of the worker (the Rust constants).
const (
	// Lease is how long a claim is held.
	Lease = 2 * time.Minute
	idle  = 2 * time.Second
	// poll is the pause between reads of a running build.
	poll = 5 * time.Second
	// blockedWait is the pause before a blocked build asks for a slot again.
	blockedWait = 15 * time.Second
	// maxSyncAttempts is the claims of one sync before it gives up on the
	// provider.
	maxSyncAttempts = 10
	// giveUpAfter is how long past its deadline a build may keep failing on
	// infrastructure.
	giveUpAfter = time.Hour
	// fieldManager writes the BuildRun status.
	fieldManager = "kuben-builds"
	// Retention is how long a finished BuildRun shows its result.
	Retention      = 24 * time.Hour
	retentionSecs  = 24 * 3600
	sweepEvery     = 10 * time.Minute
	settledFailed  = "failed"
	settledSuccess = "succeeded"
)

// kinds are the operations a worker claims.
func kinds() []string { return []string{store.SourceSyncKind, store.BuildKind} }

// result is how the work on a claim ended.
//
//sumtype:decl
type result interface{ result() }

type (
	// done settles the operation in Phase.
	done struct {
		Phase string
		Code  opt.Val[string]
	}
	// wait looks again after After; waiting is not a failure.
	wait struct {
		After time.Duration
		Code  string
	}
	// retry: something outside failed; try again with backoff.
	retry struct {
		Code   string
		Detail string
	}
	// fenced: another worker holds the claim now.
	fenced struct{}
)

func (done) result()   {}
func (wait) result()   {}
func (retry) result()  {}
func (fenced) result() {}

func storeRetry(err error) result { return retry{Code: "StoreError", Detail: err.Error()} }

func kubeRetry(err error) result { return retry{Code: "KubernetesError", Detail: err.Error()} }

func failedWith(code string) result { return done{Phase: settledFailed, Code: opt.Some(code)} }

// Deps is what a worker works with.
type Deps struct {
	Store *store.Store
	// Client reads and writes the Jobs, Secrets and Pods of builds.
	Client kubernetes.Interface
	// Dynamic reads and writes the BuildRuns.
	Dynamic dynamic.Interface
	// ID is unique per process.
	ID       string
	Provider SourceProvider
	Verifier OutputVerifier
	Settings Settings
	Limits   store.SlotLimits
	Clock    clock.Clock
	Logger   *slog.Logger
}

// Worker is the build worker of one process.
type Worker struct {
	d Deps
}

// NewWorker is a worker on d.
func NewWorker(d Deps) *Worker {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	return &Worker{d: d}
}

// Run runs w until ctx ends. A database outage returns an error, for the
// supervisor to restart the loop with backoff.
func Run(ctx context.Context, w *Worker, h *health.Health) error {
	h.OK(Subsystem)
	nextSweep := w.d.Clock.NowMs()
	for ctx.Err() == nil {
		if w.d.Clock.NowMs() >= nextSweep {
			nextSweep += sweepEvery.Milliseconds()
			switch n, err := w.Sweep(ctx); {
			case err != nil:
				w.d.Logger.Warn("finished BuildRuns were not swept", "error", err)
			case n > 0:
				w.d.Logger.Info("finished BuildRuns swept", "removed", n)
			}
		}
		worked, err := w.WorkOnce(ctx)
		if err != nil {
			return err
		}
		if worked.IsNone() && !sleep(ctx, idle) {
			return nil
		}
	}
	return nil
}

// sleep waits d, and false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// WorkOnce claims one due operation and carries it as far as it goes now;
// none when nothing was due.
func (w *Worker) WorkOnce(ctx context.Context) (opt.Val[ids.OperationID], error) {
	claim, ok, err := w.d.Store.ClaimOperation(ctx, w.d.ID, kinds(), Lease)
	if err != nil || !ok {
		return opt.None[ids.OperationID](), err //nolint:wrapcheck // a store error naming its operation
	}
	var r result
	if claim.Kind == store.SourceSyncKind {
		r = w.sync(ctx, claim)
	} else {
		r = w.build(ctx, claim)
	}
	if err := w.settle(ctx, claim, r); err != nil {
		return opt.None[ids.OperationID](), err
	}
	return opt.Some(claim.ID), nil
}

func (w *Worker) settle(ctx context.Context, claim store.Claim, r result) error {
	var err error
	switch r := r.(type) {
	case done:
		w.d.Logger.Info("build operation settled", "operation", claim.ID.String(), "kind", claim.Kind,
			"phase", r.Phase, "code", r.Code.Or(""))
		_, err = w.d.Store.FinishOperation(ctx, claim, r.Phase, r.Code)
	case wait:
		_, err = w.d.Store.RetryOperation(ctx, claim, r.After, r.Code)
	case retry:
		failures := uint32(1<<32 - 1)
		if claim.Attempt >= 0 {
			failures = uint32(claim.Attempt)
		}
		delay := controller.Backoff(failures)
		w.d.Logger.Warn("build operation will be retried", "operation", claim.ID.String(), "kind", claim.Kind,
			"code", r.Code, "detail", r.Detail, "retry_in_s", int64(delay/time.Second))
		_, err = w.d.Store.RetryOperation(ctx, claim, delay, r.Code)
	case fenced:
	}
	return err //nolint:wrapcheck // a store error naming its operation
}

// ---- source sync ----

func (w *Worker) sync(ctx context.Context, claim store.Claim) result {
	binding, ok, err := w.syncBinding(ctx, claim)
	if err != nil {
		return storeRetry(err)
	}
	if !ok {
		return failedWith("BindingMissing")
	}
	if link, found, err := w.d.Store.GitInstallationOrg(ctx, binding.InstallationID); err == nil && found && link.Suspended {
		return failedWith(string(outcome.CredentialsRefused))
	}
	var head Head
	if number, ok := binding.PullRequest.Get(); ok {
		head, err = w.d.Provider.PullHead(ctx, binding.InstallationID, binding.Repository, number)
	} else {
		head, err = w.d.Provider.Head(ctx, binding.InstallationID, binding.Repository, binding.Branch)
	}
	if err != nil {
		return w.syncFailed(claim, binding, err)
	}
	observed, err := w.observeHead(ctx, claim, binding, head)
	if err != nil {
		return storeRetry(err)
	}
	switch o := observed.(type) {
	case store.HeadUnchanged:
		return done{Phase: settledSuccess, Code: opt.Some("Unchanged")}
	case store.HeadQueued:
		w.d.Logger.Info("build queued", "binding", binding.ID.String(), "attempt", o.Attempt.String(),
			"commit", head.Commit.String(), "epoch", uint64(o.Epoch))
		return done{Phase: settledSuccess}
	case store.HeadRepositoryChanged:
		return failedWith("RepositoryChanged")
	case store.HeadGone:
		return failedWith("TargetGone")
	case store.HeadFenced:
		return fenced{}
	}
	return fenced{}
}

// syncFailed is the result of a provider that did not answer a sync.
func (w *Worker) syncFailed(claim store.Claim, binding store.SourceBinding, err error) result {
	var pe ProviderError
	if !errors.As(err, &pe) {
		pe = Unavailable{Reason: err.Error()}
	}
	switch e := pe.(type) {
	case NotFound:
		w.d.Logger.Warn("the source is gone", "binding", binding.ID.String(), "what", e.What)
		return failedWith("SourceNotFound")
	case Refused:
		w.d.Logger.Warn("the provider refused the sync", "binding", binding.ID.String(), "why", e.Reason)
		return failedWith(string(outcome.CredentialsRefused))
	case Unavailable:
		if claim.Attempt >= maxSyncAttempts {
			w.d.Logger.Warn("giving up on the provider", "binding", binding.ID.String(), "why", e.Reason)
			return failedWith("ProviderUnavailable")
		}
		return retry{Code: "ProviderUnavailable", Detail: e.Reason}
	}
	return retry{Code: "ProviderUnavailable", Detail: err.Error()}
}

func (w *Worker) syncBinding(ctx context.Context, claim store.Claim) (store.SourceBinding, bool, error) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return store.SourceBinding{}, false, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx)            //nolint:errcheck // read only
	return t.SyncBinding(ctx, claim) //nolint:wrapcheck // a store error naming its operation
}

func (w *Worker) observeHead(
	ctx context.Context, claim store.Claim, binding store.SourceBinding, head Head,
) (store.HeadObserved, error) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	observed, err := t.ObserveHead(ctx, claim, binding, head.Commit, head.RepositoryID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	return observed, t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
}

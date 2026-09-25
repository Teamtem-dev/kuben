package materializer

// The materializer's worker (ADR-032): claims `deployment` operations and
// carries each run through delivery and verification, and claims the
// lifecycle operations of lifecycle.go.
//
// Delivery renders the run's objects, freezes the run's RenderPlan before
// the first write (never again on a retry), and writes them, the App under
// the generation fence, then records the write and moves the run to
// `acceptedByCluster`. Verification follows the App controller's status
// (progress.go) to `succeeded` or `failed`. Claims are fenced in SQL, so any
// number of replicas may run workers; none needs the leader lease.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/controller"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Subsystem is the health entry of the worker loop.
const Subsystem = "materializer"

// kinds is every operation kind a worker claims.
func kinds() []string {
	return []string{
		store.RunKind, store.ProjectApply, store.EnvironmentApply,
		store.TargetDelete, store.EnvironmentDelete, store.ProjectDelete,
	}
}

// Deps is what a worker works with.
type Deps struct {
	Store *store.Store
	// Cluster is the primary cluster, where the objects are written.
	Cluster registry.Cluster
	// ID is unique per process (leader.Identity).
	ID     string
	Logger *slog.Logger
	Clock  clock.Clock
	// Facts is where discovery publishes the cluster's capabilities; plans
	// wait for them. Without it (tests), plans follow KubenConfig alone.
	Facts opt.Val[*discovery.Watch]
	// Agents hands envelopes to cluster agents; agent-delivered targets
	// wait without it.
	Agents opt.Val[AgentDispatch]
	// VerifyDeadline is how long verification waits for the App to become
	// ready; VerifyDeadline when zero.
	VerifyDeadline time.Duration
	// DeletionCheck is how long an environment deletion waits before it
	// checks again; DeletionCheck when zero.
	DeletionCheck time.Duration
	// Keyring opens the secret revisions runs are bound to; runs bound to
	// any fail without it.
	Keyring opt.Val[*secrets.Keyring]
}

// Worker is the materializer of one process. It holds no state of its own
// between claims, so its loop and the drift watch share it.
type Worker struct {
	d Deps
}

// New is a worker on d.
func New(d Deps) *Worker {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.VerifyDeadline == 0 {
		d.VerifyDeadline = VerifyDeadline
	}
	if d.DeletionCheck == 0 {
		d.DeletionCheck = DeletionCheck
	}
	return &Worker{d: d}
}

// Run runs w until ctx ends. A database outage returns an error, for the
// supervisor to restart the loop with backoff.
func Run(ctx context.Context, w *Worker, h *health.Health) error {
	h.OK(Subsystem)
	for ctx.Err() == nil {
		worked, err := w.WorkOnce(ctx)
		if err != nil {
			return err
		}
		if worked.IsNone() {
			if !sleep(ctx, idle) {
				return nil
			}
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

// pause is the wait between reads while waiting on the cluster.
func pause(ctx context.Context) stop {
	if !sleep(ctx, poll) {
		return stopShutdown{}
	}
	return nil
}

// resource is the client of resource gvr in namespace ("" for cluster
// scope).
func (w *Worker) resource(gvr schema.GroupVersionResource, namespace string) dynamic.ResourceInterface {
	r := w.d.Cluster.Dynamic.Resource(gvr)
	if namespace == "" {
		return r
	}
	return r.Namespace(namespace)
}

// WorkOnce claims one due operation and carries it as far as it goes; it
// is the operation it worked on, none when nothing was due.
func (w *Worker) WorkOnce(ctx context.Context) (opt.Val[ids.OperationID], error) {
	claim, ok, err := w.d.Store.ClaimOperation(ctx, w.d.ID, kinds(), Lease)
	if err != nil {
		return opt.None[ids.OperationID](), storeError(err)
	}
	if !ok {
		return opt.None[ids.OperationID](), nil
	}
	var st stop
	if claim.Kind == store.RunKind {
		st = w.carry(ctx, claim)
	} else {
		st = w.carryLifecycle(ctx, claim)
	}
	return opt.Some(claim.ID), w.settle(ctx, claim, st)
}

func (w *Worker) carry(ctx context.Context, claim store.Claim) stop {
	m, found, err := w.materialization(ctx, claim.Org, claim.ID)
	switch {
	case err != nil:
		return retryStore(err)
	case !found:
		return settled(run.Failed, "RunMissing")
	}
	st := w.drive(ctx, claim, &m)
	// A succeeded run bound to secrets collects its environment's unused
	// revision objects (secrets.go).
	if s, ok := st.(stopSettled); ok && s.phase == run.Succeeded && len(m.Secrets) > 0 {
		if _, err := w.collectSecrets(ctx, &m); err != nil {
			w.d.Logger.Warn("unused secret revisions stay for now", "run", m.Run.String(), "error", err.Error())
		}
	}
	switch s := st.(type) {
	case stopRefused:
		return w.end(ctx, claim, &m, run.EventFailed, s.code)
	case stopSuperseded:
		return w.end(ctx, claim, &m, run.EventSuperseded, "")
	case stopRetry:
		if claim.Attempt >= maxAttempts {
			w.d.Logger.Warn(fmt.Sprintf("giving up after %d attempts", maxAttempts), "run", m.Run, "error", s.err)
			return w.end(ctx, claim, &m, run.EventFailed, "RetriesExhausted")
		}
	case stopFenced, stopShutdown, stopSettled, stopWait:
	}
	return st
}

func (w *Worker) materialization(ctx context.Context, org ids.OrgID, op ids.OperationID) (store.Materialization, bool, error) {
	t, err := w.d.Store.Tenant(ctx, org)
	if err != nil {
		return store.Materialization{}, false, err //nolint:wrapcheck // said as a store error
	}
	defer t.Rollback(ctx)             //nolint:errcheck // read only
	return t.Materialization(ctx, op) //nolint:wrapcheck // said as a store error
}

func (w *Worker) settle(ctx context.Context, claim store.Claim, st stop) error {
	var phase string
	code := opt.None[string]()
	switch s := st.(type) {
	case stopFenced, stopShutdown:
		return nil
	case stopRetry:
		failures := uint32(math.MaxUint32) // Rust: u32::try_from(attempt).unwrap_or(u32::MAX)
		if claim.Attempt >= 0 {
			failures = uint32(claim.Attempt)
		}
		delay := controller.Backoff(failures)
		w.d.Logger.Warn("materialization will be retried", "operation", claim.ID, "error", s.err,
			"retry_in_s", int64(delay.Seconds()))
		_, err := w.d.Store.RetryOperation(ctx, claim, delay, string(s.err.Code))
		return storeErr(err)
	case stopWait:
		_, err := w.d.Store.RetryOperation(ctx, claim, s.after, s.code)
		return storeErr(err)
	case stopSettled:
		phase, code = string(s.phase), s.code
	case stopRefused:
		// carry turns these into a run event; settle the operation anyway.
		phase, code = string(run.Failed), opt.Some(s.code)
	case stopSuperseded:
		phase = string(run.Superseded)
	}
	w.d.Logger.Info("materialization settled", "operation", claim.ID, "phase", phase, "code", code.Or(""))
	_, err := w.d.Store.FinishOperation(ctx, claim, phase, code)
	return storeErr(err)
}

// storeErr is err as a store failure; nil stays nil.
func storeErr(err error) error {
	if err == nil {
		return nil
	}
	return storeError(err)
}

// end moves the run with a last event and settles in the resulting phase.
func (w *Worker) end(ctx context.Context, claim store.Claim, m *store.Materialization, event run.Event, code string) stop {
	phase, st := w.advance(ctx, claim, m, event)
	if st != nil {
		return st
	}
	return settled(phase, code)
}

// deliveredPhases are the phases after delivery, before a verdict.
var deliveredPhases = []run.Phase{run.AcceptedByCluster, run.Preflight, run.Applying, run.Verifying}

func (w *Worker) drive(ctx context.Context, claim store.Claim, m *store.Materialization) stop {
	if m.Phase.IsFinal() || m.Phase == run.Failed {
		return settled(m.Phase, "")
	}
	if m.Deleting {
		return refused("TargetDeleting")
	}
	if m.Phase == run.AwaitingApproval {
		return w.awaitApproval(ctx, claim, m)
	}
	if m.Phase == run.Planned || m.Phase == run.PendingDelivery {
		if st := w.holdIfPaused(ctx, m); st != nil {
			return st
		}
	}
	if m.Delivery == store.DeliveryAgent {
		return w.driveAgent(ctx, claim, m)
	}
	phase := m.Phase
	if phase == run.Planned {
		var st stop
		if phase, st = w.advance(ctx, claim, m, run.EventReadyForDelivery); st != nil {
			return st
		}
	}
	var written int64
	var st stop
	switch {
	case phase == run.PendingDelivery:
		if written, st = w.deliver(ctx, claim, m); st != nil {
			return st
		}
		if phase, st = w.advance(ctx, claim, m, run.EventAcceptedByCluster); st != nil {
			return st
		}
	case slices.Contains(deliveredPhases, phase):
		if written, st = w.writtenGeneration(ctx, m); st != nil {
			return st
		}
	default:
		return stopRetry{err: unsupported(phase)}
	}
	return w.verify(ctx, claim, m, phase, written)
}

// deliver renders and writes the run's objects; the metadata.generation of
// the App object written.
func (w *Worker) deliver(ctx context.Context, claim store.Claim, m *store.Materialization) (int64, stop) {
	rendered, rerr := Render(m)
	if rerr != nil {
		return 0, refused(rerr.Code)
	}
	if st := w.prepare(ctx, claim, m, &rendered); st != nil {
		return 0, st
	}
	app, st := w.writeApp(ctx, m, &rendered.App)
	if st != nil {
		return 0, st
	}
	if app.UID == "" || app.Generation == 0 {
		return 0, stopRetry{err: incomplete("App/" + m.ApplicationSlug)}
	}
	recorded, err := w.d.Store.RecordMaterialization(ctx, claim, *m, string(app.UID), app.Generation)
	switch {
	case err != nil:
		return 0, retryStore(err)
	case !recorded:
		// Fenced off, or a newer generation is recorded already.
		return 0, stopSuperseded{}
	}
	return app.Generation, nil
}

// prepare is what a run writes before its App or envelope: the frozen
// plan, the project and environment objects, the namespace and the
// secrets.
func (w *Worker) prepare(ctx context.Context, claim store.Claim, m *store.Materialization, rendered *Rendered) stop {
	if m.RenderPlan.IsNone() {
		if st := w.freeze(ctx, claim, m, &rendered.App); st != nil {
			return st
		}
	}
	project, st := ensure(ctx, w, projectsGVR, "", v1alpha1.ProjectKind, &rendered.Project, m.Org)
	if st != nil {
		return st
	}
	environment := rendered.Environment
	if !SetOwner(&environment, &project) {
		return stopRetry{err: incomplete("Project/" + m.ProjectSlug)}
	}
	if _, st := ensure(ctx, w, environmentsGVR, "", v1alpha1.EnvironmentKind, &environment, m.Org); st != nil {
		return st
	}
	if st := w.waitNamespace(ctx, m.Namespace); st != nil {
		return st
	}
	return w.writeSecrets(ctx, m)
}

// ensure writes a project or environment object, which many targets
// share: only an object of the same organization is taken over.
func ensure[T any, PT kubenObject[T]](
	ctx context.Context, w *Worker, gvr schema.GroupVersionResource, namespace, kind string, desired PT, org ids.OrgID,
) (T, stop) {
	var zero T
	ri := w.resource(gvr, namespace)
	name := desired.GetName()
	for range maxConflicts {
		live, found, err := get[T, PT](ctx, ri, name)
		if err != nil {
			return zero, retryKube(err)
		}
		var current PT
		if found {
			current = &live
			if !BelongsTo(current.GetLabels(), org) {
				return zero, refused("NameTaken")
			}
		}
		written, err := put[T, PT](ctx, ri, kind, desired, current)
		switch {
		case err == nil:
			return written, nil
		case !isConflict(err):
			return zero, retryKube(err)
		}
	}
	return zero, stopRetry{err: contended(name)}
}

// holdIfPaused holds the run of a paused target before it writes anything
// (M4.9); an emergency rollback is never held. Checked again at the write,
// so a pause observed before it stops the write.
func (w *Worker) holdIfPaused(ctx context.Context, m *store.Materialization) stop {
	if m.Emergency {
		return nil
	}
	paused, err := w.paused(ctx, m)
	switch {
	case err != nil:
		return retryStore(err)
	case paused:
		return stopWait{after: pauseRecheck, code: "Paused"}
	}
	return nil
}

func (w *Worker) paused(ctx context.Context, m *store.Materialization) (bool, error) {
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return false, err //nolint:wrapcheck // said as a store error
	}
	defer t.Rollback(ctx)                //nolint:errcheck // read only
	return t.TargetPaused(ctx, m.Target) //nolint:wrapcheck // said as a store error
}

// writeApp writes the App object under the generation fence.
func (w *Worker) writeApp(ctx context.Context, m *store.Materialization, desired *v1alpha1.App) (v1alpha1.App, stop) {
	ri := w.resource(appsGVR, m.Namespace)
	for range maxConflicts {
		live, found, err := get[v1alpha1.App](ctx, ri, m.ApplicationSlug)
		if err != nil {
			return v1alpha1.App{}, retryKube(err)
		}
		if found && !BelongsTo(live.Labels, m.Org) {
			return v1alpha1.App{}, refused("NameTaken")
		}
		// SQL is read after the object: see fence.go.
		current, st := w.currentGeneration(ctx, m)
		if st != nil {
			return v1alpha1.App{}, st
		}
		switch f := FenceFor(m.Generation, current, live.Annotations).(type) {
		case FenceSuperseded:
			return v1alpha1.App{}, stopSuperseded{}
		case FenceForged:
			w.d.Logger.Warn("replacing a generation annotation Kuben never wrote",
				"target", m.Target, "live", f.Live, "ours", uint64(m.Generation))
		case FenceWrite:
		}
		if st := w.holdIfPaused(ctx, m); st != nil {
			return v1alpha1.App{}, st
		}
		var liveApp *v1alpha1.App
		if found {
			liveApp = &live
		}
		written, err := put(ctx, ri, v1alpha1.AppKind, desired, liveApp)
		switch {
		case err == nil:
			return written, nil
		case !isConflict(err):
			return v1alpha1.App{}, retryKube(err)
		}
	}
	return v1alpha1.App{}, stopRetry{err: contended(m.ApplicationSlug)}
}

func (w *Worker) currentGeneration(ctx context.Context, m *store.Materialization) (target.Generation, stop) {
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return 0, retryStore(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	state, found, err := t.TargetState(ctx, m.Target)
	switch {
	case err != nil:
		return 0, retryStore(err)
	case !found:
		return 0, refused("TargetMissing")
	}
	return state.DesiredGeneration, nil
}

// writtenGeneration is the written App's metadata.generation, when a run
// resumes after its delivery.
func (w *Worker) writtenGeneration(ctx context.Context, m *store.Materialization) (int64, stop) {
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return 0, retryStore(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	written, found, err := t.Materialized(ctx, m.Target)
	switch {
	case err != nil:
		return 0, retryStore(err)
	case !found || written.Generation != m.Generation:
		return 0, refused("NotDelivered")
	}
	return written.ResourceGeneration, nil
}

func (w *Worker) waitNamespace(ctx context.Context, name string) stop {
	deadline := w.d.Clock.NowMs() + namespaceWait.Milliseconds()
	for {
		_, err := w.d.Cluster.Typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			return nil
		case !apierrors.IsNotFound(err):
			return retryKube(err)
		}
		if w.d.Clock.NowMs() >= deadline {
			return stopRetry{err: namespacePending(name)}
		}
		if st := pause(ctx); st != nil {
			return st
		}
	}
}

// verify follows the App controller until the written generation is
// ready, failed, or the deadline passes.
func (w *Worker) verify(ctx context.Context, claim store.Claim, m *store.Materialization, phase run.Phase, written int64) stop {
	ri := w.resource(appsGVR, m.Namespace)
	deadline := w.d.Clock.NowMs() + w.d.VerifyDeadline.Milliseconds()
	for {
		renewed, err := w.d.Store.RenewLease(ctx, claim, Lease)
		switch {
		case err != nil:
			return retryStore(err)
		case !renewed:
			return stopFenced{}
		}
		app, found, err := get[v1alpha1.App](ctx, ri, m.ApplicationSlug)
		switch {
		case err != nil:
			return retryKube(err)
		case !found:
			return refused("AppDeleted")
		}
		var st stop
		switch p := ProgressOf(&app, written).(type) {
		case ProgressPending:
		case ProgressApplied:
			phase, st = w.toVerifying(ctx, claim, m, phase)
		case ProgressReady:
			if _, st = w.toVerifying(ctx, claim, m, phase); st != nil {
				return st
			}
			done, st := w.advance(ctx, claim, m, run.EventVerified)
			if st != nil {
				return st
			}
			return settled(done, "")
		case ProgressFailed:
			return refused(p.Reason)
		}
		if st != nil {
			return st
		}
		if w.d.Clock.NowMs() >= deadline {
			return refused("VerifyTimeout")
		}
		if st := pause(ctx); st != nil {
			return st
		}
	}
}

// toVerifying: the controller applied the written generation, so the run
// is past its preflight and applying, and is verifying.
func (w *Worker) toVerifying(ctx context.Context, claim store.Claim, m *store.Materialization, phase run.Phase) (run.Phase, stop) {
	//exhaustive:ignore // a partial table: only these phases step towards verifying
	steps := map[run.Phase]run.Event{
		run.AcceptedByCluster: run.EventPreflightStarted,
		run.Preflight:         run.EventPreflightPassed,
		run.Applying:          run.EventApplied,
	}
	for {
		event, ok := steps[phase]
		if !ok {
			return phase, nil
		}
		var st stop
		if phase, st = w.advance(ctx, claim, m, event); st != nil {
			return phase, st
		}
	}
}

func (w *Worker) advance(ctx context.Context, claim store.Claim, m *store.Materialization, event run.Event) (run.Phase, stop) {
	a, err := w.d.Store.AdvanceRun(ctx, claim, m.Run, event)
	if err != nil {
		return "", retryStore(err)
	}
	switch a := a.(type) {
	case store.AdvanceMoved:
		return a.Phase, nil
	case store.AdvanceFenced:
		return "", stopFenced{}
	case store.AdvanceIllegal:
		if a.Err == nil {
			return "", stopRetry{err: illegal(fmt.Errorf("illegal transition"))}
		}
		if a.Err.Terminal {
			// Settled meanwhile, e.g. superseded by a newer run.
			from, perr := run.ParsePhase(a.Err.From)
			if perr != nil {
				from = run.Failed
			}
			return "", settled(from, "")
		}
		return "", stopRetry{err: illegal(a.Err)}
	}
	return "", stopRetry{err: illegal(fmt.Errorf("unknown advance %T", a))}
}

// awaitApproval: a run waiting for approval (M4.1) is cancelled once its
// window has closed, otherwise looked at again then. A decision wakes it
// sooner.
func (w *Worker) awaitApproval(ctx context.Context, claim store.Claim, m *store.Materialization) stop {
	if wait, ok := ApprovalWait(m.ApprovalExpiresAt, w.d.Clock.NowMs()).Get(); ok {
		return stopWait{after: wait, code: "AwaitingApproval"}
	}
	phase, st := w.advance(ctx, claim, m, run.EventRejected)
	if st != nil {
		return st
	}
	return settled(phase, "ApprovalExpired")
}

// ApprovalWait is how long a run waiting for approval sleeps at now: until
// its window closes, at most an hour, at least one poll. None once it has
// closed or when the run has no window.
func ApprovalWait(expiresAt opt.Val[int64], now int64) opt.Val[time.Duration] {
	expires, ok := expiresAt.Get()
	if !ok {
		return opt.None[time.Duration]()
	}
	if (now > 0 && expires < math.MinInt64+now) || (now < 0 && expires > math.MaxInt64+now) {
		return opt.None[time.Duration]() // Rust: checked_sub overflowed
	}
	left := expires - now
	if left <= 0 {
		return opt.None[time.Duration]()
	}
	if left >= approvalRecheck.Milliseconds() {
		return opt.Some(approvalRecheck)
	}
	return opt.Some(max(time.Duration(left)*time.Millisecond, poll))
}

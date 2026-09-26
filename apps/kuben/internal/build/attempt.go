package build

// The build half of the worker (worker.rs, `// ---- builds ----`): one
// claim of a `build` operation carries its attempt as far as it goes now.

import (
	"context"
	"errors"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

func (w *Worker) build(ctx context.Context, claim store.Claim) result {
	attempt, ok, err := w.attemptOf(ctx, claim)
	if err != nil {
		return storeRetry(err)
	}
	if !ok {
		return failedWith("BuildMissing")
	}
	var r result
	switch NextFor(attempt.Phase, attempt.CancelRequested) {
	case NextSettle:
		r = w.settleFinal(ctx, attempt)
	case NextCancelQueued:
		r = w.cancelQueued(ctx, claim, attempt)
	case NextAdmit:
		r = w.admit(ctx, claim, attempt)
	case NextObserve:
		r = w.observe(ctx, claim, attempt)
	}
	if again, ok := r.(retry); ok && w.overdue(attempt) {
		w.d.Logger.Warn("giving up on the build", "build", attempt.ID.String(), "code", again.Code, "detail", again.Detail)
		return w.fail(ctx, claim, attempt, nil, "RetriesExhausted", again.Detail)
	}
	return r
}

func (w *Worker) attemptOf(ctx context.Context, claim store.Claim) (store.BuildAttempt, bool, error) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return store.BuildAttempt{}, false, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx)                    //nolint:errcheck // read only
	return t.BuildOfOperation(ctx, claim.ID) //nolint:wrapcheck // a store error naming its operation
}

// overdue: the attempt is older than its deadline and an hour.
func (w *Worker) overdue(attempt store.BuildAttempt) bool {
	limitMs := clock.SaturatingAdd(clock.Seconds(w.d.Settings.DeadlineSecs).Milliseconds(), giveUpAfter.Milliseconds())
	age := clock.SaturatingSub(w.d.Clock.NowMs(), attempt.CreatedAt)
	return age > limitMs
}

// settleFinal: a final attempt: revoke, delete, then settle its operation.
func (w *Worker) settleFinal(ctx context.Context, attempt store.BuildAttempt) result {
	code := attempt.Failure
	if code.IsNone() {
		code = attempt.DeployDecision
	}
	return w.finish(ctx, attempt, attempt.Phase, code, attempt.Digest)
}

// record records events in order, with progress on each; the phase they
// led to, or the result when they could not be recorded.
func (w *Worker) record(
	ctx context.Context, claim store.Claim, attempt store.BuildAttempt, events []opbuild.Event, progress store.BuildProgress,
) (opbuild.Phase, result) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return "", storeRetry(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	phase, r := apply(ctx, t, claim, attempt, events, progress)
	if r != nil {
		return "", r
	}
	if err := t.Commit(ctx); err != nil {
		return "", storeRetry(err)
	}
	return phase, nil
}

// apply applies events to attempt in t, in order.
func apply(
	ctx context.Context, t *store.Tenant, claim store.Claim, attempt store.BuildAttempt, events []opbuild.Event,
	progress store.BuildProgress,
) (opbuild.Phase, result) {
	phase := attempt.Phase
	for _, event := range events {
		advanced, err := t.AdvanceBuild(ctx, claim, attempt.ID, event, progress)
		if err != nil {
			return "", storeRetry(err)
		}
		switch a := advanced.(type) {
		case store.BuildMoved:
			phase = a.Phase
		case store.BuildFenced:
			return "", fenced{}
		case store.BuildIllegal:
			return "", retry{Code: "IllegalTransition", Detail: a.Err.Error()}
		}
	}
	return phase, nil
}

func (w *Worker) cancelQueued(ctx context.Context, claim store.Claim, attempt store.BuildAttempt) result {
	if _, r := w.record(ctx, claim, attempt, []opbuild.Event{opbuild.EventCancelRequested}, store.BuildProgress{}); r != nil {
		return r
	}
	return w.finish(ctx, attempt, opbuild.Cancelled, opt.None[string](), opt.None[artifact.Digest]())
}

func (w *Worker) admit(ctx context.Context, claim store.Claim, attempt store.BuildAttempt) result {
	granted, live, err := w.claimSlot(ctx, claim, attempt)
	switch {
	case err != nil:
		return storeRetry(err)
	case !live:
		return fenced{}
	case !granted:
		if attempt.Phase != opbuild.Blocked {
			blocked := store.BuildProgress{BlockedReason: opt.Some("NoBuildCapacity")}
			if _, r := w.record(ctx, claim, attempt, []opbuild.Event{opbuild.EventBlocked}, blocked); r != nil {
				return r
			}
		}
		return wait{After: blockedWait, Code: "NoBuildCapacity"}
	}
	name, cerr := w.createObjects(ctx, attempt)
	if cerr != nil {
		return w.notCreated(ctx, claim, attempt, cerr)
	}
	events := []opbuild.Event{}
	if attempt.Phase == opbuild.Blocked {
		events = append(events, opbuild.EventUnblocked)
	}
	events = append(events, opbuild.EventStarted)
	phase, r := w.record(ctx, claim, attempt, events, store.BuildProgress{JobName: opt.Some(name)})
	if r != nil {
		return r
	}
	w.mirror(ctx, attempt, phase, opt.None[string]())
	return wait{After: poll, Code: "Preparing"}
}

func (w *Worker) claimSlot(ctx context.Context, claim store.Claim, attempt store.BuildAttempt) (granted, live bool, err error) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return false, false, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	granted, live, err = t.ClaimBuildSlot(ctx, claim, attempt.ID, w.d.Limits)
	if err != nil {
		return false, false, err //nolint:wrapcheck // a store error naming its operation
	}
	return granted, live, t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
}

// notCreated is the result of objects that could not be created.
func (w *Worker) notCreated(ctx context.Context, claim store.Claim, attempt store.BuildAttempt, cerr createError) result {
	switch e := cerr.(type) {
	case providerFailed:
		switch p := e.err.(type) {
		case Unavailable:
			return retry{Code: "ProviderUnavailable", Detail: p.Reason}
		case Refused:
			return w.fail(ctx, claim, attempt, nil, string(outcome.CredentialsRefused), p.Reason)
		case NotFound:
			return w.fail(ctx, claim, attempt, nil, string(outcome.SourceUnavailable), p.What)
		}
	case kubeFailed:
		if isInvalid(e.err) {
			return w.fail(ctx, claim, attempt, nil, string(outcome.InvalidBudget), e.err.Error())
		}
		return kubeRetry(e.err)
	case renderFailed:
		return w.fail(ctx, claim, attempt, nil, string(outcome.InvalidBudget), e.reason)
	}
	return kubeRetry(cerr)
}

func (w *Worker) observe(ctx context.Context, claim store.Claim, attempt store.BuildAttempt) result {
	seen, err := w.observeJob(ctx, attempt)
	if err != nil {
		return kubeRetry(err)
	}
	plan := PlanFor(attempt.Phase, attempt.CancelRequested, outcome.Classify(seen), attempt.ReportedDigest)
	switch p := plan.(type) {
	case PlanWait:
		phase, r := w.record(ctx, claim, attempt, p.Events, store.BuildProgress{})
		if r != nil {
			return r
		}
		if len(p.Events) > 0 {
			w.mirror(ctx, attempt, phase, opt.None[string]())
		}
		return wait{After: poll, Code: "Building"}
	case PlanVerify:
		reported := store.BuildProgress{ReportedDigest: opt.Some(p.Digest)}
		if _, r := w.record(ctx, claim, attempt, p.Events, reported); r != nil {
			return r
		}
		return w.verify(ctx, claim, attempt, p.Digest)
	case PlanFail:
		return w.fail(ctx, claim, attempt, p.Events, string(p.Failure), p.Detail)
	case PlanStop:
		if err := w.deleteJob(ctx, Name(attempt)); err != nil {
			return kubeRetry(err)
		}
		if _, r := w.record(ctx, claim, attempt, p.Events, store.BuildProgress{}); r != nil {
			return r
		}
		return wait{After: poll, Code: "Stopping"}
	case PlanCancelled:
		if _, r := w.record(ctx, claim, attempt, p.Events, store.BuildProgress{}); r != nil {
			return r
		}
		return w.finish(ctx, attempt, opbuild.Cancelled, opt.None[string](), opt.None[artifact.Digest]())
	}
	return wait{After: poll, Code: "Building"}
}

// verify checks digest in the registry and completes the attempt.
func (w *Worker) verify(ctx context.Context, claim store.Claim, attempt store.BuildAttempt, digest artifact.Digest) result {
	if err := w.d.Verifier.Verify(ctx, attempt.ImageRepository, digest); err != nil {
		var missing ManifestMissing
		if errors.As(err, &missing) {
			return w.fail(ctx, claim, attempt, nil, string(outcome.OutputRejected), missing.What)
		}
		var down RegistryUnavailable
		if errors.As(err, &down) {
			return retry{Code: "RegistryUnavailable", Detail: down.Reason}
		}
		return retry{Code: "RegistryUnavailable", Detail: err.Error()}
	}
	if r := w.recordScan(ctx, claim, attempt, digest); r != nil {
		return r
	}
	completed, err := w.complete(ctx, claim, attempt, digest)
	if err != nil {
		return storeRetry(err)
	}
	switch c := completed.(type) {
	case store.CompletedFenced:
		return fenced{}
	case store.CompletedIllegal:
		w.d.Logger.Warn("the build cannot complete from its phase", "build", attempt.ID.String(), "illegal", c.Err.Error())
		return retry{Code: "IllegalTransition", Detail: c.Err.Error()}
	case store.CompletedDeployed:
		w.d.Logger.Info("build deployed", "build", attempt.ID.String(), "release", c.Release.String(),
			"run", c.Run.String(), "generation", uint64(c.Generation), "digest", digest.String())
		return w.finish(ctx, attempt, opbuild.Succeeded, opt.None[string](), opt.Some(digest))
	case store.CompletedKept:
		w.d.Logger.Info("build kept without a deploy", "build", attempt.ID.String(), "release", c.Release.String(),
			"decision", c.Decision, "digest", digest.String())
		return w.finish(ctx, attempt, opbuild.Succeeded, opt.Some(c.Decision), opt.Some(digest))
	}
	return fenced{}
}

func (w *Worker) complete(
	ctx context.Context, claim store.Claim, attempt store.BuildAttempt, digest artifact.Digest,
) (store.Completed, error) {
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	completed, err := t.CompleteBuild(ctx, claim, attempt, digest)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	switch completed.(type) {
	case store.CompletedDeployed, store.CompletedKept:
		if err := t.Commit(ctx); err != nil {
			return nil, err //nolint:wrapcheck // a store error naming its operation
		}
	case store.CompletedIllegal, store.CompletedFenced:
	}
	return completed, nil
}

// recordScan keeps the scan and SBOM the pod left for digest (M4.6), before
// the attempt completes and the scan gate judges its release. A scan that
// did not run is recorded as unavailable.
func (w *Worker) recordScan(ctx context.Context, claim store.Claim, attempt store.BuildAttempt, digest artifact.Digest) result {
	report, sbom := scan.Unavailable("scanning is not configured (build.scanner_image)"), opt.None[[]byte]()
	if w.d.Settings.ScannerImage.IsSome() {
		var err error
		pods := w.d.Client.CoreV1().Pods(w.d.Settings.Namespace)
		report, sbom, err = Collect(ctx, pods, AttemptLabel+"="+attempt.ID.String(), w.d.Logger)
		if err != nil {
			return kubeRetry(err)
		}
	}
	scanned := store.NewScan{
		Repository:   attempt.ImageRepository,
		Digest:       digest.String(),
		Summary:      SummaryOf(report, w.d.Clock.NowMs()),
		Detail:       detailOf(report),
		BuildAttempt: opt.Some(attempt.ID),
	}
	if err := keepScan(ctx, w.d.Store, claim.Org, scanned, sbom); err != nil {
		return storeRetry(err)
	}
	w.d.Logger.Info("image scanned", "build", attempt.ID.String(), "digest", digest.String(),
		"status", string(scanned.Summary.Status), "critical", scanned.Summary.Counts.Critical,
		"high", scanned.Summary.Counts.High, "sbom", sbom.IsSome())
	return nil
}

// fail fails the attempt with code, queues a retry for a lost worker, and
// cleans up.
func (w *Worker) fail(
	ctx context.Context, claim store.Claim, attempt store.BuildAttempt, events []opbuild.Event, code, detail string,
) result {
	failed := store.BuildProgress{Failure: opt.Some(store.BuildFailureReport{Code: code, Detail: detail})}
	all := append(append([]opbuild.Event{}, events...), opbuild.EventFailed)
	t, err := w.d.Store.Tenant(ctx, claim.Org)
	if err != nil {
		return storeRetry(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if _, r := apply(ctx, t, claim, attempt, all, failed); r != nil {
		return r
	}
	if code == string(outcome.LostWorker) {
		next, retried, err := t.RetryBuild(ctx, attempt)
		if err != nil {
			return storeRetry(err)
		}
		if retried {
			w.d.Logger.Info("a lost build is retried", "build", attempt.ID.String(), "next", next.String())
		}
	}
	if err := t.Commit(ctx); err != nil {
		return storeRetry(err)
	}
	w.d.Logger.Info("build failed", "build", attempt.ID.String(), "code", code, "detail", detail)
	return w.finish(ctx, attempt, opbuild.Failed, opt.Some(code), opt.None[artifact.Digest]())
}

// finish cleans up a final attempt and settles its operation.
func (w *Worker) finish(
	ctx context.Context, attempt store.BuildAttempt, phase opbuild.Phase, code opt.Val[string],
	digest opt.Val[artifact.Digest],
) result {
	shown := attempt
	if c, ok := code.Get(); ok && phase == opbuild.Failed {
		shown.Failure = opt.Some(c)
	}
	text := opt.None[string]()
	if d, ok := digest.Get(); ok {
		text = opt.Some(d.String())
	}
	if err := w.cleanup(ctx, shown, phase, text); err != nil {
		// The attempt is final; the next claim repeats the cleanup.
		return kubeRetry(err)
	}
	return done{Phase: operationPhase(phase), Code: code}
}

// operationPhase is the operation phase of a final attempt.
func operationPhase(phase opbuild.Phase) string {
	switch phase {
	case opbuild.Succeeded:
		return settledSuccess
	case opbuild.Cancelled:
		return "cancelled"
	case opbuild.Queued, opbuild.Blocked, opbuild.Preparing, opbuild.Running, opbuild.Publishing,
		opbuild.VerifyingOutput, opbuild.CancelRequested, opbuild.Cancelling, opbuild.Failed:
	}
	return settledFailed
}

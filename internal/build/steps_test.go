package build_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Ported from steps.rs.

const stepDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func digest(t *testing.T, text string) artifact.Digest {
	t.Helper()
	return check(artifact.ParseDigest(text)).must(t)
}

func finishedVerdict(t *testing.T) outcome.JobVerdict {
	t.Helper()
	return outcome.Finished{Report: outcome.BuildReport{Digest: digest(t, stepDigest), Strategy: "dockerfile"}}
}

func failedVerdict(failure outcome.BuildFailure) outcome.JobVerdict {
	return outcome.Failed{Failure: failure, Detail: failure.Explain()}
}

// replay applies events from phase; every event must be legal.
func replay(t *testing.T, phase opbuild.Phase, events []opbuild.Event) opbuild.Phase {
	t.Helper()
	for _, e := range events {
		next, err := phase.Apply(e)
		if err != nil {
			t.Fatalf("%s + %s: %v", phase, e, err)
		}
		phase = next
	}
	return phase
}

func observed() []opbuild.Phase {
	return []opbuild.Phase{
		opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.VerifyingOutput,
		opbuild.CancelRequested, opbuild.Cancelling,
	}
}

var (
	noDigest = opt.None[artifact.Digest]()
	none     = []opbuild.Event{}
)

func TestWorkFollowsThePhase(t *testing.T) {
	cases := []struct {
		phase  opbuild.Phase
		cancel bool
		want   build.Next
	}{
		{opbuild.Queued, false, build.NextAdmit},
		{opbuild.Blocked, false, build.NextAdmit},
		{opbuild.Blocked, true, build.NextCancelQueued},
		{opbuild.Running, true, build.NextObserve},
		{opbuild.Succeeded, true, build.NextSettle},
		{opbuild.Failed, true, build.NextSettle},
		{opbuild.Cancelled, true, build.NextSettle},
	}
	for _, c := range cases {
		if got := build.NextFor(c.phase, c.cancel); got != c.want {
			t.Errorf("%s, cancel %t: %s, want %s", c.phase, c.cancel, got, c.want)
		}
	}
	for _, n := range []build.Next{build.NextSettle, build.NextCancelQueued, build.NextAdmit, build.NextObserve} {
		if got, err := build.ParseNext(string(n)); err != nil || got != n {
			t.Errorf("%s: %s, %v", n, got, err)
		}
	}
	if _, err := build.ParseNext("sleep"); err == nil {
		t.Error("an unknown kind of work")
	}
}

func TestAFinishedJobIsVerifiedFromAnyRunningPhase(t *testing.T) {
	for _, phase := range []opbuild.Phase{opbuild.Preparing, opbuild.Running, opbuild.Publishing} {
		v, ok := build.PlanFor(phase, false, finishedVerdict(t), noDigest).(build.PlanVerify)
		if !ok {
			t.Fatalf("%s: not verified", phase)
		}
		if got := replay(t, phase, v.Events); got != opbuild.VerifyingOutput {
			t.Errorf("%s: %s", phase, got)
		}
		if v.Digest != digest(t, stepDigest) {
			t.Errorf("%s: digest %s", phase, v.Digest)
		}
		if got := replay(t, opbuild.VerifyingOutput, []opbuild.Event{opbuild.EventVerified}); got != opbuild.Succeeded {
			t.Errorf("verified: %s", got)
		}
	}
}

func TestVerificationResumesFromTheRecordedDigest(t *testing.T) {
	gone := build.PlanFor(opbuild.VerifyingOutput, false, outcome.Gone{}, opt.Some(digest(t, stepDigest)))
	if diff := cmp.Diff(build.Plan(build.PlanVerify{Events: none, Digest: digest(t, stepDigest)}), gone,
		cmp.Comparer(func(a, b artifact.Digest) bool { return a == b })); diff != "" {
		t.Errorf("the Job may be gone after the report (-want +got):\n%s", diff)
	}
	f, ok := build.PlanFor(opbuild.VerifyingOutput, false, outcome.Gone{}, noDigest).(build.PlanFail)
	if !ok || f.Failure != outcome.OutputRejected {
		t.Errorf("no digest: %#v", f)
	}
}

func TestProgressOnlyMovesForward(t *testing.T) {
	cases := []struct {
		phase   opbuild.Phase
		verdict outcome.JobVerdict
		want    []opbuild.Event
	}{
		{opbuild.Preparing, outcome.Pending{}, none},
		{opbuild.Preparing, outcome.Fetching{}, none},
		{opbuild.Preparing, outcome.Building{}, []opbuild.Event{opbuild.EventBuilding}},
		{opbuild.Running, outcome.Building{}, none},
	}
	for _, c := range cases {
		got := build.PlanFor(c.phase, false, c.verdict, noDigest)
		if diff := cmp.Diff(build.Plan(build.PlanWait{Events: c.want}), got); diff != "" {
			t.Errorf("%s %T (-want +got):\n%s", c.phase, c.verdict, diff)
		}
	}
}

func TestFailuresKeepTheirClass(t *testing.T) {
	failures := []outcome.BuildFailure{
		outcome.OutOfMemory, outcome.DiskFull, outcome.DeadlineExceeded, outcome.BuildError,
		outcome.SourceUnavailable, outcome.Unschedulable,
	}
	for _, failure := range failures {
		for _, phase := range []opbuild.Phase{opbuild.Preparing, opbuild.Running, opbuild.Publishing} {
			f, ok := build.PlanFor(phase, false, failedVerdict(failure), noDigest).(build.PlanFail)
			if !ok || f.Failure != failure {
				t.Fatalf("%s in %s: %#v", failure, phase, f)
			}
			if got := replay(t, replay(t, phase, f.Events), []opbuild.Event{opbuild.EventFailed}); got != opbuild.Failed {
				t.Errorf("%s in %s: %s", failure, phase, got)
			}
		}
	}
	lost, ok := build.PlanFor(opbuild.Running, false, outcome.Gone{}, noDigest).(build.PlanFail)
	if !ok || lost.Failure != outcome.LostWorker {
		t.Errorf("a vanished Job: %#v", lost)
	}
}

func TestAStopDeletesTheJobThenWaitsForItToGo(t *testing.T) {
	for _, phase := range []opbuild.Phase{opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.CancelRequested} {
		stop, ok := build.PlanFor(phase, true, outcome.Building{}, noDigest).(build.PlanStop)
		if !ok {
			t.Fatalf("%s: not stopped", phase)
		}
		if got := replay(t, phase, stop.Events); got != opbuild.Cancelling {
			t.Errorf("%s: %s", phase, got)
		}
	}
	if diff := cmp.Diff(build.Plan(build.PlanStop{Events: none}), build.PlanFor(opbuild.Cancelling, true, outcome.Building{}, noDigest)); diff != "" {
		t.Errorf("cancelling (-want +got):\n%s", diff)
	}
	for _, phase := range observed() {
		if phase == opbuild.VerifyingOutput {
			continue
		}
		c, ok := build.PlanFor(phase, true, outcome.Gone{}, noDigest).(build.PlanCancelled)
		if !ok {
			t.Fatalf("%s: not cancelled", phase)
		}
		if got := replay(t, phase, c.Events); got != opbuild.Cancelled {
			t.Errorf("%s: %s", phase, got)
		}
	}
}

func TestAJobKilledByTheStopIsCancelledNotFailed(t *testing.T) {
	cases := []struct {
		phase   opbuild.Phase
		verdict outcome.JobVerdict
	}{
		{opbuild.Cancelling, failedVerdict(outcome.LostWorker)},
		{opbuild.Running, failedVerdict(outcome.OutOfMemory)},
	}
	for _, c := range cases {
		cancelled, ok := build.PlanFor(c.phase, true, c.verdict, noDigest).(build.PlanCancelled)
		if !ok {
			t.Fatalf("%s: not cancelled", c.phase)
		}
		if got := replay(t, c.phase, cancelled.Events); got != opbuild.Cancelled {
			t.Errorf("%s: %s", c.phase, got)
		}
	}
}

func TestOutputFinishedBeforeTheStopStillSucceeds(t *testing.T) {
	verified := []opbuild.Event{opbuild.EventVerified}
	for _, phase := range []opbuild.Phase{opbuild.Running, opbuild.Publishing, opbuild.CancelRequested, opbuild.Cancelling} {
		v, ok := build.PlanFor(phase, true, finishedVerdict(t), noDigest).(build.PlanVerify)
		if !ok {
			t.Fatalf("%s: not verified", phase)
		}
		if got := replay(t, replay(t, phase, v.Events), verified); got != opbuild.Succeeded {
			t.Errorf("%s: %s", phase, got)
		}
	}
	v, ok := build.PlanFor(opbuild.VerifyingOutput, true, outcome.Gone{}, opt.Some(digest(t, stepDigest))).(build.PlanVerify)
	if !ok {
		t.Fatal("verifying: not verified")
	}
	if got := replay(t, replay(t, opbuild.VerifyingOutput, v.Events), verified); got != opbuild.Succeeded {
		t.Errorf("verifying: %s", got)
	}
}

func TestEveryPlanIsLegalFromEveryObservedPhase(t *testing.T) {
	verdicts := []outcome.JobVerdict{
		outcome.Pending{},
		outcome.Fetching{},
		outcome.Building{},
		finishedVerdict(t),
		failedVerdict(outcome.OutOfMemory),
		outcome.Gone{},
	}
	for _, phase := range observed() {
		for _, cancel := range []bool{false, true} {
			for _, verdict := range verdicts {
				for _, reported := range []opt.Val[artifact.Digest]{noDigest, opt.Some(digest(t, stepDigest))} {
					replay(t, phase, build.EventsOf(build.PlanFor(phase, cancel, verdict, reported)))
				}
			}
		}
	}
}

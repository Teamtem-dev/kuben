package run_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"testing/quick"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/ops"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
)

func TestPhasesHoldEveryPhaseOnceAndParseInvertsString(t *testing.T) {
	names := []string{
		"planned", "awaitingApproval", "pendingDelivery", "acceptedByCluster", "preflight", "blocked", "applying",
		"verifying", "succeeded", "failed", "superseded", "cancelRequested", "cancelled", "recoveryRequested",
		"recovering", "recovered", "recoveryFailed", "manualActionRequired",
	}
	phases := run.Phases()
	if len(phases) != len(names) {
		t.Fatalf("got %d phases", len(phases))
	}
	for i, phase := range phases {
		if phase.String() != names[i] {
			t.Errorf("phase %d is %s, want %s", i, phase, names[i])
		}
		if got, err := run.ParsePhase(names[i]); err != nil || got != phase {
			t.Errorf("%s: got %s, %v", names[i], got, err)
		}
	}
	if _, err := run.ParsePhase("finished"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("finished: %v", err)
	}
}

func TestEventsHoldEveryEventOnce(t *testing.T) {
	names := []string{
		"requireApproval", "readyForDelivery", "approved", "rejected", "acceptedByCluster", "preflightStarted",
		"preflightPassed", "blocked", "unblocked", "applied", "verified", "failed", "superseded", "cancelRequested",
		"stopped", "recoveryRequested", "recoveryStarted", "recovered", "recoveryFailed", "manualActionRequired",
	}
	events := run.Events()
	if len(events) != len(names) {
		t.Fatalf("got %d events", len(events))
	}
	for i, event := range events {
		if got, err := run.ParseEvent(names[i]); err != nil || got != event || event.String() != names[i] {
			t.Errorf("%s: got %s, %v", names[i], got, err)
		}
	}
	if _, err := run.ParseEvent("done"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("done: %v", err)
	}
}

func TestPhasesAndEventsKeepTheirJSON(t *testing.T) {
	raw, err := json.Marshal(struct {
		Phase run.Phase `json:"phase"`
		Event run.Event `json:"event"`
	}{run.ManualActionRequired, run.EventReadyForDelivery})
	if err != nil || string(raw) != `{"phase":"manualActionRequired","event":"readyForDelivery"}` {
		t.Fatalf("got %s, %v", raw, err)
	}
	var phase run.Phase
	if err = json.Unmarshal([]byte(`"awaitingApproval"`), &phase); err != nil || phase != run.AwaitingApproval {
		t.Errorf("got %s, %v", phase, err)
	}
	if err = json.Unmarshal([]byte(`"AwaitingApproval"`), &phase); err == nil {
		t.Error("phases are camelCase on the wire")
	}
	var event run.Event
	if err = json.Unmarshal([]byte(`"stopped"`), &event); err != nil || event != run.EventStopped {
		t.Errorf("got %s, %v", event, err)
	}
	if err = json.Unmarshal([]byte(`"halted"`), &event); err == nil {
		t.Error("halted is not an event")
	}
}

// rule is one arm of the Rust match: any of these phases, on any of these
// events, moves to `to`.
type rule struct {
	from []run.Phase
	on   []run.Event
	to   run.Phase
}

// rules is the machine as the Rust match wrote it, arm by arm, so the table
// is checked against an independent statement of the same rules.
func rules() []rule {
	one := func(from run.Phase, on run.Event, to run.Phase) rule {
		return rule{[]run.Phase{from}, []run.Event{on}, to}
	}
	undelivered := []run.Phase{run.Planned, run.AwaitingApproval, run.PendingDelivery}
	delivered := []run.Phase{
		run.AcceptedByCluster, run.Preflight, run.Blocked, run.Applying, run.Verifying, run.CancelRequested,
	}
	var nonFinal []run.Phase
	for _, p := range run.Phases() {
		if !p.IsFinal() {
			nonFinal = append(nonFinal, p)
		}
	}
	return []rule{
		one(run.Planned, run.EventRequireApproval, run.AwaitingApproval),
		one(run.Planned, run.EventReadyForDelivery, run.PendingDelivery),
		one(run.AwaitingApproval, run.EventApproved, run.PendingDelivery),
		one(run.PendingDelivery, run.EventAcceptedByCluster, run.AcceptedByCluster),
		one(run.AcceptedByCluster, run.EventPreflightStarted, run.Preflight),
		one(run.Blocked, run.EventUnblocked, run.Preflight),
		{[]run.Phase{run.Preflight, run.Applying, run.Blocked}, []run.Event{run.EventBlocked}, run.Blocked},
		one(run.Preflight, run.EventPreflightPassed, run.Applying),
		one(run.Applying, run.EventApplied, run.Verifying),
		{[]run.Phase{run.Verifying, run.CancelRequested}, []run.Event{run.EventVerified}, run.Succeeded},
		{nonFinal, []run.Event{run.EventSuperseded}, run.Superseded},
		{undelivered, []run.Event{run.EventCancelRequested}, run.Cancelled},
		one(run.AwaitingApproval, run.EventRejected, run.Cancelled),
		one(run.CancelRequested, run.EventStopped, run.Cancelled),
		{delivered, []run.Event{run.EventCancelRequested}, run.CancelRequested},
		{
			[]run.Phase{run.CancelRequested},
			[]run.Event{
				run.EventAcceptedByCluster, run.EventPreflightStarted, run.EventPreflightPassed, run.EventApplied,
			},
			run.CancelRequested,
		},
		{slices.Concat(undelivered, delivered), []run.Event{run.EventFailed}, run.Failed},
		{[]run.Phase{run.Failed, run.CancelRequested}, []run.Event{run.EventRecoveryRequested}, run.RecoveryRequested},
		one(run.RecoveryRequested, run.EventRecoveryStarted, run.Recovering),
		one(run.Recovering, run.EventRecovered, run.Recovered),
		one(run.Recovering, run.EventRecoveryFailed, run.RecoveryFailed),
		{
			[]run.Phase{run.RecoveryRequested, run.Recovering},
			[]run.Event{run.EventManualActionRequired},
			run.ManualActionRequired,
		},
	}
}

// want is the first arm that matches, as in a match.
func want(p run.Phase, e run.Event) (run.Phase, bool) {
	for _, r := range rules() {
		if slices.Contains(r.from, p) && slices.Contains(r.on, e) {
			return r.to, true
		}
	}
	return p, false
}

func TestEveryPairIsAllowedOrRefusedAsSpecified(t *testing.T) {
	allowed := 0
	for _, p := range append(run.Phases(), "finished") {
		for _, e := range append(run.Events(), "done") {
			next, ok := want(p, e)
			got, err := p.Apply(e)
			if ok {
				allowed++
				if err != nil || got != next {
					t.Errorf("%s + %s: got %s, %v; want %s", p, e, got, err, next)
				}
				continue
			}
			var illegal *ops.IllegalTransitionError
			if !errors.As(err, &illegal) {
				t.Errorf("%s + %s: got %s, %v; want it refused", p, e, got, err)
				continue
			}
			wantErr := ops.IllegalTransitionError{
				Machine: "DeploymentRun", From: string(p), Event: string(e), Terminal: p.IsFinal(),
			}
			if *illegal != wantErr || got != p {
				t.Errorf("%s + %s: got %s, %+v", p, e, got, *illegal)
			}
		}
	}
	if allowed != 56 {
		t.Errorf("the machine allows %d transitions, want 56", allowed)
	}
}

func TestFinalPhasesAcceptNothingAndNeverWrite(t *testing.T) {
	final := []run.Phase{
		run.Succeeded, run.Superseded, run.Cancelled, run.Recovered, run.RecoveryFailed, run.ManualActionRequired,
	}
	for _, p := range run.Phases() {
		if got := slices.Contains(final, p); p.IsFinal() != got {
			t.Errorf("%s: IsFinal is %v", p, p.IsFinal())
		}
		if !p.IsFinal() {
			continue
		}
		if p.MayWrite() {
			t.Errorf("%s is final and may write", p)
		}
		for _, e := range run.Events() {
			if got, err := p.Apply(e); err == nil {
				t.Errorf("%s + %s moved to %s", p, e, got)
			}
		}
	}
	if run.Phase("finished").IsFinal() || run.Phase("finished").MayWrite() {
		t.Error("an unknown phase is neither final nor writing")
	}
}

func apply(events ...run.Event) (run.Phase, error) {
	p := run.Planned
	for _, e := range events {
		next, err := p.Apply(e)
		if err != nil {
			return p, err
		}
		p = next
	}
	return p, nil
}

func delivered(then ...run.Event) []run.Event {
	return append([]run.Event{run.EventReadyForDelivery, run.EventAcceptedByCluster, run.EventPreflightStarted}, then...)
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []run.Event
		want   run.Phase
	}{
		{
			"happy path with approval",
			[]run.Event{
				run.EventRequireApproval, run.EventApproved, run.EventAcceptedByCluster, run.EventPreflightStarted,
				run.EventPreflightPassed, run.EventApplied, run.EventVerified,
			},
			run.Succeeded,
		},
		{
			"capacity block is removable",
			delivered(run.EventBlocked, run.EventUnblocked, run.EventPreflightPassed, run.EventApplied, run.EventVerified),
			run.Succeeded,
		},
		{"cancel before delivery is immediate", []run.Event{run.EventCancelRequested}, run.Cancelled},
		{"cancel after delivery is a request", delivered(run.EventCancelRequested), run.CancelRequested},
		{
			"a failed run can request its recovery",
			delivered(
				run.EventPreflightPassed, run.EventFailed, run.EventRecoveryRequested, run.EventRecoveryStarted,
				run.EventRecovered,
			),
			run.Recovered,
		},
		{
			"a superseded run",
			delivered(run.EventPreflightPassed, run.EventFailed, run.EventSuperseded),
			run.Superseded,
		},
		{
			// The outcome of the deploy stays "failed"; the recovery has its own phase.
			"failure is not succeeded after recovery",
			delivered(run.EventFailed, run.EventRecoveryRequested, run.EventRecoveryStarted, run.EventRecovered),
			run.Recovered,
		},
	} {
		if got, err := apply(tc.events...); err != nil || got != tc.want {
			t.Errorf("%s: got %s, %v", tc.name, got, err)
		}
	}
}

func TestACancelRequestStillTouchesTheCluster(t *testing.T) {
	if !run.CancelRequested.MayWrite() {
		t.Fatal("a stop still touches the cluster")
	}
}

func TestRecoveryIsRequestedOnce(t *testing.T) {
	_, err := run.Recovered.Apply(run.EventRecoveryRequested)
	var illegal *ops.IllegalTransitionError
	if !errors.As(err, &illegal) || !illegal.Terminal {
		t.Fatalf("got %v", err)
	}
}

func TestASupersededRunCanNoLongerRecoverOrWrite(t *testing.T) {
	if run.Superseded.MayWrite() {
		t.Error("a superseded run lost its rights")
	}
	if _, err := run.Superseded.Apply(run.EventRecoveryRequested); err == nil {
		t.Error("a superseded run cannot recover")
	}
}

func eventsOf(seed []byte) []run.Event {
	all := run.Events()
	events := make([]run.Event, 0, len(seed))
	for _, b := range seed {
		events = append(events, all[int(b)%len(all)])
	}
	return events
}

func TestFinalPhasesAreAbsorbing(t *testing.T) {
	property := func(seed []byte) bool {
		phase, settled, isSettled := run.Planned, run.Planned, false
		for _, event := range eventsOf(seed) {
			if next, err := phase.Apply(event); err == nil {
				if isSettled && next != settled {
					return false
				}
				phase = next
			}
			if phase.IsFinal() && !isSettled {
				settled, isSettled = phase, true
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// A run only reaches Succeeded through EventVerified.
func TestSucceededRequiresVerification(t *testing.T) {
	property := func(seed []byte) bool {
		phase, verified := run.Planned, false
		for _, event := range eventsOf(seed) {
			if next, err := phase.Apply(event); err == nil {
				if next == run.Succeeded && phase != run.Succeeded {
					verified = event == run.EventVerified
				}
				phase = next
			}
		}
		return phase != run.Succeeded || verified
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

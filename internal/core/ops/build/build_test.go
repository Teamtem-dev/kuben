package build_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"testing/quick"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/ops"
	"github.com/Teamtem-dev/kuben/internal/core/ops/build"
)

func TestPhasesRoundTripThroughTheirNames(t *testing.T) {
	names := []string{
		"queued", "blocked", "preparing", "running", "publishing", "verifyingOutput", "cancelRequested",
		"cancelling", "succeeded", "failed", "cancelled",
	}
	phases := build.Phases()
	if len(phases) != len(names) {
		t.Fatalf("got %d phases", len(phases))
	}
	for i, phase := range phases {
		if got, err := build.ParsePhase(names[i]); err != nil || got != phase || phase.String() != names[i] {
			t.Errorf("%s: got %s, %v", names[i], got, err)
		}
	}
	if _, err := build.ParsePhase("done"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("done: %v", err)
	}
}

func TestEventsRoundTripThroughTheirNames(t *testing.T) {
	names := []string{
		"blocked", "unblocked", "started", "building", "publishing", "published", "verified", "failed",
		"cancelRequested", "stopping", "stopped",
	}
	events := build.Events()
	if len(events) != len(names) {
		t.Fatalf("got %d events", len(events))
	}
	for i, event := range events {
		if got, err := build.ParseEvent(names[i]); err != nil || got != event || event.String() != names[i] {
			t.Errorf("%s: got %s, %v", names[i], got, err)
		}
	}
	if _, err := build.ParseEvent("done"); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("done: %v", err)
	}
}

func TestPhasesAndEventsKeepTheirJSON(t *testing.T) {
	raw, err := json.Marshal(struct {
		Phase build.Phase `json:"phase"`
		Event build.Event `json:"event"`
	}{build.VerifyingOutput, build.EventCancelRequested})
	if err != nil || string(raw) != `{"phase":"verifyingOutput","event":"cancelRequested"}` {
		t.Fatalf("got %s, %v", raw, err)
	}
	var phase build.Phase
	if err = json.Unmarshal([]byte(`"verifyingOutput"`), &phase); err != nil || phase != build.VerifyingOutput {
		t.Errorf("got %s, %v", phase, err)
	}
	if err = json.Unmarshal([]byte(`"VerifyingOutput"`), &phase); err == nil {
		t.Error("phases are camelCase on the wire")
	}
	var event build.Event
	if err = json.Unmarshal([]byte(`"stopping"`), &event); err != nil || event != build.EventStopping {
		t.Errorf("got %s, %v", event, err)
	}
	if err = json.Unmarshal([]byte(`"halted"`), &event); err == nil {
		t.Error("halted is not an event")
	}
}

// rule is one arm of the Rust match: any of these phases, on any of these
// events, moves to `to`.
type rule struct {
	from []build.Phase
	on   []build.Event
	to   build.Phase
}

// rules is the machine as the Rust match wrote it, arm by arm, so the table
// is checked against an independent statement of the same rules.
func rules() []rule {
	one := func(from build.Phase, on build.Event, to build.Phase) rule {
		return rule{[]build.Phase{from}, []build.Event{on}, to}
	}
	late := []build.Event{build.EventStarted, build.EventBuilding, build.EventPublishing, build.EventPublished}
	waiting := []build.Phase{build.Queued, build.Blocked}
	working := []build.Phase{build.Preparing, build.Running, build.Publishing, build.VerifyingOutput}
	stopping := []build.Phase{build.CancelRequested, build.Cancelling}
	return []rule{
		{waiting, []build.Event{build.EventBlocked}, build.Blocked},
		one(build.Blocked, build.EventUnblocked, build.Queued),
		one(build.Queued, build.EventStarted, build.Preparing),
		one(build.Preparing, build.EventBuilding, build.Running),
		one(build.Running, build.EventPublishing, build.Publishing),
		one(build.Publishing, build.EventPublished, build.VerifyingOutput),
		{
			[]build.Phase{build.VerifyingOutput, build.CancelRequested, build.Cancelling},
			[]build.Event{build.EventVerified},
			build.Succeeded,
		},
		{waiting, []build.Event{build.EventCancelRequested}, build.Cancelled},
		{stopping, []build.Event{build.EventStopped}, build.Cancelled},
		one(build.Cancelling, build.EventFailed, build.Cancelled),
		{append(slices.Clone(working), build.CancelRequested), []build.Event{build.EventCancelRequested}, build.CancelRequested},
		{[]build.Phase{build.CancelRequested}, late, build.CancelRequested},
		one(build.CancelRequested, build.EventStopping, build.Cancelling),
		{
			[]build.Phase{build.Cancelling},
			append(slices.Clone(late), build.EventCancelRequested, build.EventStopping),
			build.Cancelling,
		},
		{
			slices.Concat(waiting, working, []build.Phase{build.CancelRequested}),
			[]build.Event{build.EventFailed},
			build.Failed,
		},
	}
}

// want is the first arm that matches, as in a match.
func want(p build.Phase, e build.Event) (build.Phase, bool) {
	for _, r := range rules() {
		if slices.Contains(r.from, p) && slices.Contains(r.on, e) {
			return r.to, true
		}
	}
	return p, false
}

func TestEveryPairIsAllowedOrRefusedAsSpecified(t *testing.T) {
	allowed := 0
	for _, p := range append(build.Phases(), "done") {
		for _, e := range append(build.Events(), "done") {
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
				Machine: "BuildAttempt", From: string(p), Event: string(e), Terminal: p.IsTerminal(),
			}
			if *illegal != wantErr || got != p {
				t.Errorf("%s + %s: got %s, %+v", p, e, got, *illegal)
			}
		}
	}
	if allowed != 38 {
		t.Errorf("the machine allows %d transitions, want 38", allowed)
	}
}

func apply(events ...build.Event) (build.Phase, error) {
	p := build.Queued
	for _, e := range events {
		next, err := p.Apply(e)
		if err != nil {
			return p, err
		}
		p = next
	}
	return p, nil
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []build.Event
		want   build.Phase
	}{
		{
			"happy path reaches succeeded",
			[]build.Event{
				build.EventStarted, build.EventBuilding, build.EventPublishing, build.EventPublished, build.EventVerified,
			},
			build.Succeeded,
		},
		{"cancel before start needs no worker", []build.Event{build.EventCancelRequested}, build.Cancelled},
		{"cancel while blocked", []build.Event{build.EventBlocked, build.EventCancelRequested}, build.Cancelled},
		{
			"cancel while running is a request",
			[]build.Event{build.EventStarted, build.EventBuilding, build.EventCancelRequested},
			build.CancelRequested,
		},
		{
			"cancelled only after the job stops",
			[]build.Event{
				build.EventStarted, build.EventBuilding, build.EventCancelRequested, build.EventStopping,
				build.EventStopped,
			},
			build.Cancelled,
		},
		{
			"output verified before the stop is recorded as succeeded",
			[]build.Event{
				build.EventStarted, build.EventBuilding, build.EventPublishing, build.EventPublished,
				build.EventCancelRequested, build.EventVerified,
			},
			build.Succeeded,
		},
		{
			"a job killed by cancel is cancelled, not failed",
			[]build.Event{build.EventStarted, build.EventCancelRequested, build.EventStopping, build.EventFailed},
			build.Cancelled,
		},
	} {
		if got, err := apply(tc.events...); err != nil || got != tc.want {
			t.Errorf("%s: got %s, %v", tc.name, got, err)
		}
	}
	if build.CancelRequested.IsTerminal() {
		t.Error("not cancelled until termination is observed")
	}
}

func TestTerminalAttemptsNeverRunAgain(t *testing.T) {
	_, err := apply(build.EventStarted, build.EventFailed, build.EventStarted)
	var illegal *ops.IllegalTransitionError
	if !errors.As(err, &illegal) || !illegal.Terminal || illegal.From != "failed" {
		t.Fatalf("got %v", err)
	}
	for _, p := range []build.Phase{build.Succeeded, build.Failed, build.Cancelled} {
		for _, e := range build.Events() {
			if got, aerr := p.Apply(e); aerr == nil {
				t.Errorf("%s + %s moved to %s", p, e, got)
			}
		}
	}
}

func TestBlockedAttemptsDoNotHoldASlot(t *testing.T) {
	holding := []build.Phase{
		build.Preparing, build.Running, build.Publishing, build.VerifyingOutput, build.CancelRequested,
		build.Cancelling, // the Job may still be running
	}
	for _, p := range append(build.Phases(), "done") {
		if got := slices.Contains(holding, p); p.HoldsSlot() != got {
			t.Errorf("%s: HoldsSlot is %v", p, p.HoldsSlot())
		}
	}
}

func eventsOf(seed []byte) []build.Event {
	all := build.Events()
	events := make([]build.Event, 0, len(seed))
	for _, b := range seed {
		events = append(events, all[int(b)%len(all)])
	}
	return events
}

// Whatever the executor reports, a terminal phase is absorbing and an illegal
// event leaves the stored phase unchanged.
func TestTerminalPhasesAreAbsorbing(t *testing.T) {
	property := func(seed []byte) bool {
		phase, reached, isReached := build.Queued, build.Queued, false
		for _, event := range eventsOf(seed) {
			next, err := phase.Apply(event)
			var illegal *ops.IllegalTransitionError
			switch {
			case err == nil:
				if isReached && next != reached {
					return false
				}
				phase = next
			case !errors.As(err, &illegal) || illegal.Terminal != phase.IsTerminal() || next != phase:
				return false
			}
			if phase.IsTerminal() && !isReached {
				reached, isReached = phase, true
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// Cancelled is only reachable through a cancel request.
func TestCancelledRequiresACancelRequest(t *testing.T) {
	property := func(seed []byte) bool {
		phase, asked := build.Queued, false
		for _, event := range eventsOf(seed) {
			asked = asked || event == build.EventCancelRequested
			if next, err := phase.Apply(event); err == nil {
				phase = next
			}
		}
		return phase != build.Cancelled || asked
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

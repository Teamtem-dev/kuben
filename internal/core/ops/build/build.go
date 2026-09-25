// Package build has the BuildAttempt phases (ADR-028). It replaces the Rust
// module kuben-core/src/ops/build.rs.
//
//	Queued ⇄ Blocked
//	Queued → Preparing → Running → Publishing → VerifyingOutput → Succeeded
//	  any active → Failed
//	  Queued/Blocked + cancel → Cancelled            (nothing is running yet)
//	  running phases + cancel → CancelRequested → Cancelling → Cancelled
//
// Cancellation is a request, not an undo: if the executor reports a verified
// output before the stop took effect, the attempt is Succeeded (the artifact
// exists; the target's source epoch decides whether it deploys). An
// infrastructure retry is a new attempt; a terminal attempt never runs again.
package build

import (
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/ops"
)

// Phase is where a build attempt stands. Only [Phase.Apply] moves it.
type Phase string

// The phases, in declaration order.
const (
	Queued Phase = "queued"
	// Blocked: admission found no capacity or quota; re-evaluated later.
	Blocked         Phase = "blocked"
	Preparing       Phase = "preparing"
	Running         Phase = "running"
	Publishing      Phase = "publishing"
	VerifyingOutput Phase = "verifyingOutput"
	CancelRequested Phase = "cancelRequested"
	Cancelling      Phase = "cancelling"
	Succeeded       Phase = "succeeded"
	Failed          Phase = "failed"
	Cancelled       Phase = "cancelled"
)

// Event is something the executor or a user reported about a build attempt.
type Event string

// The events, in declaration order.
const (
	// EventBlocked: admission cannot place the build now.
	EventBlocked Event = "blocked"
	// EventUnblocked: capacity or quota is available again.
	EventUnblocked Event = "unblocked"
	// EventStarted: the Job exists and the source is being fetched.
	EventStarted Event = "started"
	// EventBuilding: BuildKit is building.
	EventBuilding Event = "building"
	// EventPublishing: the push to the registry started.
	EventPublishing Event = "publishing"
	// EventPublished: the push finished; the digest is being checked against
	// the registry.
	EventPublished Event = "published"
	// EventVerified: the output manifest was verified in the registry.
	EventVerified Event = "verified"
	// EventFailed: user error, OOM, deadline, lost worker, rejected output.
	EventFailed Event = "failed"
	// EventCancelRequested: a user, or a newer commit, asked to stop.
	EventCancelRequested Event = "cancelRequested"
	// EventStopping: the executor saw the request and is deleting the Job.
	EventStopping Event = "stopping"
	// EventStopped: the Job is gone.
	EventStopped Event = "stopped"
)

// edge is one allowed move out of a phase.
type edge struct {
	on Event
	to Phase
}

// transitions is the whole machine: from → event → to. What is not listed is
// refused. It is never written after initialization.
//
// Cancellation: before a Job exists it is immediate; afterwards it is a
// request until the Job is observed gone. A Job we are killing ends as
// cancelled, not failed. Late progress reports while stopping change nothing.
var transitions = map[Phase][]edge{
	Queued: {
		{EventBlocked, Blocked},
		{EventStarted, Preparing},
		{EventCancelRequested, Cancelled},
		{EventFailed, Failed},
	},
	Blocked: {
		{EventBlocked, Blocked},
		{EventUnblocked, Queued},
		{EventCancelRequested, Cancelled},
		{EventFailed, Failed},
	},
	Preparing: {
		{EventBuilding, Running},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Running: {
		{EventPublishing, Publishing},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Publishing: {
		{EventPublished, VerifyingOutput},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	VerifyingOutput: {
		{EventVerified, Succeeded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	CancelRequested: {
		{EventVerified, Succeeded},
		{EventStopped, Cancelled},
		{EventStopping, Cancelling},
		{EventCancelRequested, CancelRequested},
		{EventStarted, CancelRequested},
		{EventBuilding, CancelRequested},
		{EventPublishing, CancelRequested},
		{EventPublished, CancelRequested},
		{EventFailed, Failed},
	},
	Cancelling: {
		{EventVerified, Succeeded},
		{EventStopped, Cancelled},
		{EventFailed, Cancelled},
		{EventCancelRequested, Cancelling},
		{EventStopping, Cancelling},
		{EventStarted, Cancelling},
		{EventBuilding, Cancelling},
		{EventPublishing, Cancelling},
		{EventPublished, Cancelling},
	},
	// Terminal phases accept nothing.
	Succeeded: {},
	Failed:    {},
	Cancelled: {},
}

// Phases is every phase, in declaration order.
func Phases() []Phase {
	return []Phase{
		Queued, Blocked, Preparing, Running, Publishing, VerifyingOutput, CancelRequested, Cancelling,
		Succeeded, Failed, Cancelled,
	}
}

// Events is every event, in declaration order.
func Events() []Event {
	return []Event{
		EventBlocked, EventUnblocked, EventStarted, EventBuilding, EventPublishing, EventPublished, EventVerified,
		EventFailed, EventCancelRequested, EventStopping, EventStopped,
	}
}

// ParsePhase reads a phase as stored.
func ParsePhase(s string) (Phase, error) {
	if _, ok := transitions[Phase(s)]; ok {
		return Phase(s), nil
	}
	return "", kerr.New(kerr.Validation, "unknown build phase `%s`", s)
}

// ParseEvent reads an event by its wire name.
func ParseEvent(s string) (Event, error) {
	for _, e := range Events() {
		if string(e) == s {
			return e, nil
		}
	}
	return "", kerr.New(kerr.Validation, "unknown build event `%s`", s)
}

func (p Phase) String() string { return string(p) }

func (e Event) String() string { return string(e) }

// UnmarshalText refuses a phase that does not exist.
func (p *Phase) UnmarshalText(text []byte) error {
	parsed, err := ParsePhase(string(text))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// UnmarshalText refuses an event that does not exist.
func (e *Event) UnmarshalText(text []byte) error {
	parsed, err := ParseEvent(string(text))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// IsTerminal reports whether no event changes the phase.
func (p Phase) IsTerminal() bool {
	switch p {
	case Succeeded, Failed, Cancelled:
		return true
	case Queued, Blocked, Preparing, Running, Publishing, VerifyingOutput, CancelRequested, Cancelling:
		return false
	}
	return false
}

// HoldsSlot reports whether the attempt may hold a build slot and a Job.
func (p Phase) HoldsSlot() bool {
	switch p {
	case Preparing, Running, Publishing, VerifyingOutput, CancelRequested, Cancelling:
		return true
	case Queued, Blocked, Succeeded, Failed, Cancelled:
		return false
	}
	return false
}

// Apply is the next phase after event. Anything the table does not allow is
// refused with an [*ops.IllegalTransitionError], and the phase stays as it is.
func (p Phase) Apply(event Event) (Phase, error) {
	for _, e := range transitions[p] {
		if e.on == event {
			return e.to, nil
		}
	}
	return p, &ops.IllegalTransitionError{
		Machine:  "BuildAttempt",
		From:     string(p),
		Event:    string(event),
		Terminal: p.IsTerminal(),
	}
}

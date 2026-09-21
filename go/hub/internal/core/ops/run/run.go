// Package run has the DeploymentRun phases (ADR-026). It replaces the Rust
// module kuben-core/src/ops/run.rs.
//
//	Planned → AwaitingApproval → PendingDelivery → AcceptedByCluster
//	        → Preflight ⇄ Blocked → Applying → Verifying → Succeeded
//	any non-final → Superseded            (a newer run owns the target)
//	before delivery + cancel → Cancelled
//	after delivery + cancel → CancelRequested → Cancelled | RecoveryRequested
//	Failed | CancelRequested → RecoveryRequested → Recovering
//	                         → Recovered | RecoveryFailed | ManualActionRequired
//
// Superseded means the run lost the right to decide the target, not that its
// pods are gone. Failed is settled but may still request its one recorded
// compensation; everything else final is absorbing.
package run

import (
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops"
)

// Phase is where a deployment run stands. Only [Phase.Apply] moves it.
type Phase string

// The phases, in declaration order.
const (
	Planned           Phase = "planned"
	AwaitingApproval  Phase = "awaitingApproval"
	PendingDelivery   Phase = "pendingDelivery"
	AcceptedByCluster Phase = "acceptedByCluster"
	Preflight         Phase = "preflight"
	// Blocked: a removable condition (capacity, quota) stops progress; it is
	// re-checked later.
	Blocked              Phase = "blocked"
	Applying             Phase = "applying"
	Verifying            Phase = "verifying"
	Succeeded            Phase = "succeeded"
	Failed               Phase = "failed"
	Superseded           Phase = "superseded"
	CancelRequested      Phase = "cancelRequested"
	Cancelled            Phase = "cancelled"
	RecoveryRequested    Phase = "recoveryRequested"
	Recovering           Phase = "recovering"
	Recovered            Phase = "recovered"
	RecoveryFailed       Phase = "recoveryFailed"
	ManualActionRequired Phase = "manualActionRequired"
)

// Event is something that happened to a deployment run.
type Event string

// The events, in declaration order.
const (
	EventRequireApproval Event = "requireApproval"
	// EventReadyForDelivery: the plan is frozen and needs no (more) approval.
	EventReadyForDelivery Event = "readyForDelivery"
	EventApproved         Event = "approved"
	EventRejected         Event = "rejected"
	// EventAcceptedByCluster: the agent persisted and validated the execution
	// envelope.
	EventAcceptedByCluster Event = "acceptedByCluster"
	EventPreflightStarted  Event = "preflightStarted"
	EventPreflightPassed   Event = "preflightPassed"
	EventBlocked           Event = "blocked"
	EventUnblocked         Event = "unblocked"
	EventApplied           Event = "applied"
	EventVerified          Event = "verified"
	EventFailed            Event = "failed"
	// EventSuperseded: a newer run took the target (generation CAS).
	EventSuperseded      Event = "superseded"
	EventCancelRequested Event = "cancelRequested"
	// EventStopped: execution stopped; nothing will be compensated.
	EventStopped              Event = "stopped"
	EventRecoveryRequested    Event = "recoveryRequested"
	EventRecoveryStarted      Event = "recoveryStarted"
	EventRecovered            Event = "recovered"
	EventRecoveryFailed       Event = "recoveryFailed"
	EventManualActionRequired Event = "manualActionRequired"
)

// edge is one allowed move out of a phase.
type edge struct {
	on Event
	to Phase
}

// transitions is the whole machine: from → event → to. What is not listed is
// refused. It is never written after initialization.
//
// A newer run owns the target, so every non-final phase accepts
// EventSuperseded. Cancelling before delivery is immediate; after delivery it
// is a request, and late progress reports while stopping change nothing.
var transitions = map[Phase][]edge{
	Planned: {
		{EventRequireApproval, AwaitingApproval},
		{EventReadyForDelivery, PendingDelivery},
		{EventSuperseded, Superseded},
		{EventCancelRequested, Cancelled},
		{EventFailed, Failed},
	},
	AwaitingApproval: {
		{EventApproved, PendingDelivery},
		{EventRejected, Cancelled},
		{EventSuperseded, Superseded},
		{EventCancelRequested, Cancelled},
		{EventFailed, Failed},
	},
	PendingDelivery: {
		{EventAcceptedByCluster, AcceptedByCluster},
		{EventSuperseded, Superseded},
		{EventCancelRequested, Cancelled},
		{EventFailed, Failed},
	},
	AcceptedByCluster: {
		{EventPreflightStarted, Preflight},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Preflight: {
		{EventBlocked, Blocked},
		{EventPreflightPassed, Applying},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Blocked: {
		{EventUnblocked, Preflight},
		{EventBlocked, Blocked},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Applying: {
		{EventBlocked, Blocked},
		{EventApplied, Verifying},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Verifying: {
		{EventVerified, Succeeded},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventFailed, Failed},
	},
	Failed: {
		{EventSuperseded, Superseded},
		{EventRecoveryRequested, RecoveryRequested},
	},
	CancelRequested: {
		{EventVerified, Succeeded},
		{EventStopped, Cancelled},
		{EventSuperseded, Superseded},
		{EventCancelRequested, CancelRequested},
		{EventAcceptedByCluster, CancelRequested},
		{EventPreflightStarted, CancelRequested},
		{EventPreflightPassed, CancelRequested},
		{EventApplied, CancelRequested},
		{EventFailed, Failed},
		{EventRecoveryRequested, RecoveryRequested},
	},
	RecoveryRequested: {
		{EventRecoveryStarted, Recovering},
		{EventManualActionRequired, ManualActionRequired},
		{EventSuperseded, Superseded},
	},
	Recovering: {
		{EventRecovered, Recovered},
		{EventRecoveryFailed, RecoveryFailed},
		{EventManualActionRequired, ManualActionRequired},
		{EventSuperseded, Superseded},
	},
	// Final phases accept nothing.
	Succeeded:            {},
	Superseded:           {},
	Cancelled:            {},
	Recovered:            {},
	RecoveryFailed:       {},
	ManualActionRequired: {},
}

// Phases is every phase, in declaration order.
func Phases() []Phase {
	return []Phase{
		Planned, AwaitingApproval, PendingDelivery, AcceptedByCluster, Preflight, Blocked, Applying, Verifying,
		Succeeded, Failed, Superseded, CancelRequested, Cancelled, RecoveryRequested, Recovering, Recovered,
		RecoveryFailed, ManualActionRequired,
	}
}

// Events is every event, in declaration order.
func Events() []Event {
	return []Event{
		EventRequireApproval, EventReadyForDelivery, EventApproved, EventRejected, EventAcceptedByCluster,
		EventPreflightStarted, EventPreflightPassed, EventBlocked, EventUnblocked, EventApplied, EventVerified,
		EventFailed, EventSuperseded, EventCancelRequested, EventStopped, EventRecoveryRequested,
		EventRecoveryStarted, EventRecovered, EventRecoveryFailed, EventManualActionRequired,
	}
}

// ParsePhase reads a phase as stored.
func ParsePhase(s string) (Phase, error) {
	if _, ok := transitions[Phase(s)]; ok {
		return Phase(s), nil
	}
	return "", kerr.New(kerr.Validation, "unknown deployment run phase `%s`", s)
}

// ParseEvent reads an event by its wire name.
func ParseEvent(s string) (Event, error) {
	for _, e := range Events() {
		if string(e) == s {
			return e, nil
		}
	}
	return "", kerr.New(kerr.Validation, "unknown deployment run event `%s`", s)
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

// IsFinal reports whether the phase is absorbing: no event changes it.
func (p Phase) IsFinal() bool {
	switch p {
	case Succeeded, Superseded, Cancelled, Recovered, RecoveryFailed, ManualActionRequired:
		return true
	case Planned, AwaitingApproval, PendingDelivery, AcceptedByCluster, Preflight, Blocked, Applying, Verifying,
		Failed, CancelRequested, RecoveryRequested, Recovering:
		return false
	}
	return false
}

// MayWrite reports whether the run may still change the target's resources.
func (p Phase) MayWrite() bool {
	switch p {
	case AcceptedByCluster, Preflight, Blocked, Applying, Verifying, CancelRequested, RecoveryRequested, Recovering:
		return true
	case Planned, AwaitingApproval, PendingDelivery, Succeeded, Failed, Superseded, Cancelled, Recovered,
		RecoveryFailed, ManualActionRequired:
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
		Machine:  "DeploymentRun",
		From:     string(p),
		Event:    string(event),
		Terminal: p.IsFinal(),
	}
}

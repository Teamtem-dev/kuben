package build

// What the build worker does next, decided without I/O (steps.rs).
//
// [NextFor] picks the kind of work for an attempt's phase; [PlanFor] turns
// what the Job shows into the events to record, in order, and what follows
// them. The events are the ones Phase.Apply accepts on the way, so a worker
// that crashes between two of them resumes from the recorded phase.

import (
	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Next is the kind of work an attempt needs.
type Next string

// The kinds of work.
const (
	// NextSettle: the attempt is final: settle its operation and clean up.
	NextSettle Next = "settle"
	// NextCancelQueued: nothing runs yet and a stop was asked: cancel
	// without a Job.
	NextCancelQueued Next = "cancelQueued"
	// NextAdmit: take a build slot and create the Job.
	NextAdmit Next = "admit"
	// NextObserve: read the Job.
	NextObserve Next = "observe"
)

// ParseNext reads a kind of work by its name.
func ParseNext(s string) (Next, error) {
	switch n := Next(s); n {
	case NextSettle, NextCancelQueued, NextAdmit, NextObserve:
		return n, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown kind of build work `%s`", s)
}

// NextFor is the work for an attempt in phase.
func NextFor(phase opbuild.Phase, cancelRequested bool) Next {
	switch phase {
	case opbuild.Succeeded, opbuild.Failed, opbuild.Cancelled:
		return NextSettle
	case opbuild.Queued, opbuild.Blocked:
		if cancelRequested {
			return NextCancelQueued
		}
		return NextAdmit
	case opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.VerifyingOutput,
		opbuild.CancelRequested, opbuild.Cancelling:
		return NextObserve
	}
	return NextObserve
}

// Plan is what follows the events it records.
//
//sumtype:decl
type Plan interface{ plan() }

type (
	// PlanWait records the events and looks again later.
	PlanWait struct{ Events []opbuild.Event }
	// PlanVerify records the events, then checks Digest in the registry.
	PlanVerify struct {
		Events []opbuild.Event
		Digest artifact.Digest
	}
	// PlanFail records the events, then fails with Failure.
	PlanFail struct {
		Events  []opbuild.Event
		Failure outcome.BuildFailure
		Detail  string
	}
	// PlanStop records the events, deletes the build's objects and looks
	// again later.
	PlanStop struct{ Events []opbuild.Event }
	// PlanCancelled records the events: the attempt is cancelled.
	PlanCancelled struct{ Events []opbuild.Event }
)

func (PlanWait) plan()      {}
func (PlanVerify) plan()    {}
func (PlanFail) plan()      {}
func (PlanStop) plan()      {}
func (PlanCancelled) plan() {}

// EventsOf are the events plan records first.
func EventsOf(plan Plan) []opbuild.Event {
	switch p := plan.(type) {
	case PlanWait:
		return p.Events
	case PlanVerify:
		return p.Events
	case PlanFail:
		return p.Events
	case PlanStop:
		return p.Events
	case PlanCancelled:
		return p.Events
	}
	return nil
}

// toCancelling are the events that move a running attempt to Cancelling.
func toCancelling(phase opbuild.Phase) []opbuild.Event {
	switch phase {
	case opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.VerifyingOutput:
		return []opbuild.Event{opbuild.EventCancelRequested, opbuild.EventStopping}
	case opbuild.CancelRequested:
		return []opbuild.Event{opbuild.EventStopping}
	case opbuild.Queued, opbuild.Blocked, opbuild.Cancelling, opbuild.Succeeded, opbuild.Failed, opbuild.Cancelled:
	}
	return []opbuild.Event{}
}

// toVerifying are the events that bring phase up to VerifyingOutput.
func toVerifying(phase opbuild.Phase) []opbuild.Event {
	switch phase {
	case opbuild.Preparing:
		return []opbuild.Event{opbuild.EventBuilding, opbuild.EventPublishing, opbuild.EventPublished}
	case opbuild.Running:
		return []opbuild.Event{opbuild.EventPublishing, opbuild.EventPublished}
	case opbuild.Publishing:
		return []opbuild.Event{opbuild.EventPublished}
	case opbuild.Queued, opbuild.Blocked, opbuild.VerifyingOutput, opbuild.CancelRequested, opbuild.Cancelling,
		opbuild.Succeeded, opbuild.Failed, opbuild.Cancelled:
	}
	return []opbuild.Event{}
}

// PlanFor is the plan for an attempt in the observing phase, given what
// its Job shows and the digest it reported earlier.
func PlanFor(
	phase opbuild.Phase, cancelRequested bool, verdict outcome.JobVerdict, reported opt.Val[artifact.Digest],
) Plan {
	none := []opbuild.Event{}
	if phase == opbuild.VerifyingOutput && !cancelRequested {
		if digest, ok := reported.Get(); ok {
			return PlanVerify{Events: none, Digest: digest}
		}
		return PlanFail{
			Events: none, Failure: outcome.OutputRejected, Detail: "no digest was reported before verification",
		}
	}
	if cancelRequested || phase == opbuild.CancelRequested || phase == opbuild.Cancelling {
		return planStop(phase, verdict, reported)
	}
	switch v := verdict.(type) {
	case outcome.Building:
		if phase == opbuild.Preparing {
			return PlanWait{Events: []opbuild.Event{opbuild.EventBuilding}}
		}
		return PlanWait{Events: none}
	case outcome.Pending, outcome.Fetching:
		return PlanWait{Events: none}
	case outcome.Finished:
		return PlanVerify{Events: toVerifying(phase), Digest: v.Report.Digest}
	case outcome.Failed:
		return PlanFail{Events: none, Failure: v.Failure, Detail: v.Detail}
	case outcome.Gone:
		return PlanFail{Events: none, Failure: outcome.LostWorker, Detail: "the build Job disappeared"}
	}
	return PlanWait{Events: none}
}

// planStop: a stop was asked while the attempt may hold a Job. An output
// that was already finished is still verified: the artifact exists
// (ADR-028).
func planStop(phase opbuild.Phase, verdict outcome.JobVerdict, reported opt.Val[artifact.Digest]) Plan {
	finished := opt.None[artifact.Digest]()
	if f, ok := verdict.(outcome.Finished); ok {
		finished = opt.Some(f.Report.Digest)
	} else if phase == opbuild.VerifyingOutput {
		finished = reported
	}
	if digest, ok := finished.Get(); ok {
		events := []opbuild.Event{opbuild.EventCancelRequested}
		if phase == opbuild.CancelRequested || phase == opbuild.Cancelling {
			events = []opbuild.Event{}
		}
		return PlanVerify{Events: events, Digest: digest}
	}
	events := toCancelling(phase)
	switch verdict.(type) {
	// The Job is gone, or ended on its own while being stopped.
	case outcome.Gone, outcome.Failed:
		return PlanCancelled{Events: append(events, opbuild.EventStopped)}
	case outcome.Pending, outcome.Fetching, outcome.Building, outcome.Finished:
	}
	return PlanStop{Events: events}
}

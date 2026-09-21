// Package ops has what the durable operation state machines share (ADR-026,
// ADR-027). It replaces the Rust module kuben-core/src/ops/mod.rs.
//
// Every long-running operation is a record with a typed phase. The rules for
// moving between phases live in the subpackages as pure functions over
// transition tables, so they are unit- and property-tested without a
// database or a cluster: ops/build has the BuildAttempt phases, ops/run the
// DeploymentRun phases, ops/outcome the meaning of a build Job and
// ops/target the target's control state. Controllers persist the phase that
// build.Phase.Apply, run.Phase.Apply or target.State's methods return;
// nothing else may write it (I-19).
package ops

import "fmt"

// IllegalTransitionError is an event that the current phase does not accept.
// The caller records it as evidence instead of guessing a phase.
type IllegalTransitionError struct {
	// Machine is the record the phase belongs to, e.g. `BuildAttempt`.
	Machine string
	// From is the wire name of the phase that refused the event.
	From string
	// Event is the wire name of the refused event.
	Event string
	// Terminal says the phase is final; no event can change it.
	Terminal bool
}

func (e *IllegalTransitionError) Error() string {
	terminal := ""
	if e.Terminal {
		terminal = " (terminal)"
	}
	return fmt.Sprintf("%s: event `%s` is not allowed in phase `%s`%s", e.Machine, e.Event, e.From, terminal)
}

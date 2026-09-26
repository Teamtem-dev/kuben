// Package clock is the agent's wall clock: the one place that reads it, so
// the code that decides by the time takes a func() time.Time and tests can
// set it. (The hub's core/clock is internal to the hub module.)
package clock

import "time"

// Now is the wall clock.
func Now() time.Time {
	return time.Now() //nolint:forbidigo // the one reading of the wall clock
}

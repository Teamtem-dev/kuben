package discovery

import (
	"context"
	"time"
)

// Internals under test.
var (
	LargestNode    = largestNode
	ReadinessOf    = readinessOf
	DefaultClass   = defaultClass
	PolicyEnforcer = policyEnforcer
)

// Loop is the state of Run between rounds, for tests that drive it one
// round at a time.
type Loop struct{ l loop }

// NewLoop is a loop over d that has written nothing yet.
func NewLoop(d Deps) *Loop { return &Loop{l: loop{deps: d}} }

// Round runs one round of Run.
func (l *Loop) Round(ctx context.Context) (time.Duration, bool) { return l.l.round(ctx) }

// Package status is the public state of a service (M5.3): what a status
// page says about an app, from what Kuben observed, and nothing more.
package status

import (
	"slices"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// Service is a component's or a page's state.
type Service string

// The states, from best to worst.
const (
	Operational Service = "operational"
	Degraded    Service = "degraded"
	MajorOutage Service = "majorOutage"
)

var severity = []Service{Operational, Degraded, MajorOutage}

// Observed is what Kuben observed of one app.
type Observed struct {
	// Ready is the app's own report; absent when there is none yet.
	Ready     opt.Val[bool]
	Pods      uint32
	ReadyPods uint32
	// CriticalIncident: an open critical incident concerns the app.
	CriticalIncident bool
	// WarningIncident: an open warning incident concerns the app.
	WarningIncident bool
}

// Component is the public state of an app. Nothing observed is never
// operational.
func Component(o Observed) Service {
	if o.Pods > 0 && o.ReadyPods == 0 {
		return MajorOutage
	}
	ready, reported := o.Ready.Get()
	switch {
	case reported && ready && o.ReadyPods >= o.Pods && !o.CriticalIncident && !o.WarningIncident:
		return Operational
	case reported && !ready && o.Pods == 0:
		return MajorOutage
	default:
		return Degraded
	}
}

// Page is the state of a page: its worst component; operational without any.
func Page(components []Service) Service {
	worst := 0
	for _, c := range components {
		worst = max(worst, slices.Index(severity, c))
	}
	return severity[worst]
}

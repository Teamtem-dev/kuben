package render

import (
	"fmt"
	"strings"
)

// BuildReason is why an App cannot be turned into workloads, as the
// CamelCase reason of a Kubernetes condition.
type BuildReason string

// The build reasons (controller::resources::BuildError).
const (
	ReasonNoProcesses              BuildReason = "NoProcesses"
	ReasonMultiplePorts            BuildReason = "MultiplePorts"
	ReasonUnknownSize              BuildReason = "UnknownSize"
	ReasonAwaitingBuild            BuildReason = "AwaitingBuild"
	ReasonInvalidSource            BuildReason = "InvalidSource"
	ReasonVolumeNeedsSingleReplica BuildReason = "VolumeNeedsSingleReplica"
	ReasonScheduledWithPort        BuildReason = "ScheduledWithPort"
	ReasonInvalidSchedule          BuildReason = "InvalidSchedule"
	ReasonInvalidDomainTLS         BuildReason = "InvalidDomainTls"
)

// BuildError is a spec the builder refuses (surfaced as a condition).
type BuildError struct {
	Reason  BuildReason
	message string
}

func (e *BuildError) Error() string { return e.message }

func errNoProcesses() *BuildError {
	return &BuildError{ReasonNoProcesses, "app has no processes"}
}

func errMultiplePorts(names []string) *BuildError {
	return &BuildError{ReasonMultiplePorts, "only one process may expose a port, found: " + strings.Join(names, ", ")}
}

func errUnknownSize(process, size string) *BuildError {
	return &BuildError{ReasonUnknownSize, fmt.Sprintf("process `%s` uses unknown size `%s`", process, size)}
}

func errAwaitingBuild() *BuildError {
	return &BuildError{ReasonAwaitingBuild, "no image yet: git sources are deployed once a build has produced an image"}
}

func errInvalidSource() *BuildError {
	return &BuildError{ReasonInvalidSource, "set exactly one of source.image or source.git"}
}

func errVolumeNeedsSingleReplica() *BuildError {
	return &BuildError{ReasonVolumeNeedsSingleReplica, "apps with volumes run a single process with at most one replica (ReadWriteOnce)"}
}

func errScheduledWithPort(process string) *BuildError {
	return &BuildError{ReasonScheduledWithPort, fmt.Sprintf("scheduled process `%s` cannot expose a port", process)}
}

func errInvalidSchedule(process, schedule string) *BuildError {
	return &BuildError{ReasonInvalidSchedule, fmt.Sprintf("process `%s` has an invalid schedule `%s`", process, schedule)}
}

func errInvalidDomainTLS(host, tls string) *BuildError {
	return &BuildError{ReasonInvalidDomainTLS, fmt.Sprintf("domain `%s` has tls `%s`: use `auto`, `none` or the name of a Secret", host, tls)}
}

// TooLargeError: the rendered resources exceed the envelope bound.
type TooLargeError struct {
	Bytes int
	Max   int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("the rendered resources take %d bytes, more than the %d-byte envelope", e.Bytes, e.Max)
}

// IncompleteError: a rendered object lacks a field the inventory needs.
type IncompleteError struct {
	What string
}

func (e *IncompleteError) Error() string { return "a rendered object has no " + e.What }

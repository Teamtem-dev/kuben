// Package outcome says what a build Job's observed state means for its
// attempt (ADR-028). It replaces the Rust module
// kuben-core/src/ops/outcome.rs.
//
// The executor reads the Job and its pod and hands the facts to [Classify],
// a pure function, so every failure class (out of memory, a full disk, the
// deadline, a lost node, a bad source, bad output) is decided and tested in
// one place. A pod's termination message is only a hint: a reported digest
// is checked against the registry before the attempt succeeds.
package outcome

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	// FetchContainer is the name of the init container that fetches the source.
	FetchContainer = "fetch"
	// BuildContainer is the name of the container that builds and pushes.
	BuildContainer = "build"
	// ScanContainer is the name of the container that scans the pushed image
	// (M4.6). It runs after the build and never decides the build's outcome: a
	// scan that fails is recorded as unavailable.
	ScanContainer = "scan"
	// UnschedulableLimitSecs is how long a build pod may stay unschedulable
	// before the attempt fails.
	UnschedulableLimitSecs uint64 = 600

	detailMax = 1024
)

// BuildFailure is why an attempt failed. The code is stored and shown; only a
// lost worker is retried automatically, as a new attempt.
type BuildFailure string

// The failures.
const (
	// BuildError: the build itself failed (a Dockerfile step, a compiler error).
	BuildError BuildFailure = "BuildError"
	// OutOfMemory: the build exceeded its memory limit.
	OutOfMemory BuildFailure = "OutOfMemory"
	// DiskFull: the build exceeded its ephemeral-storage limit or the disk
	// filled up.
	DiskFull BuildFailure = "DiskFull"
	// DeadlineExceeded: the build ran past its deadline.
	DeadlineExceeded BuildFailure = "DeadlineExceeded"
	// LostWorker: the node or pod disappeared under the build.
	LostWorker BuildFailure = "LostWorker"
	// SourceUnavailable: the repository or commit could not be fetched.
	SourceUnavailable BuildFailure = "SourceUnavailable"
	// OutputRejected: the build finished without a valid, verifiable output.
	OutputRejected BuildFailure = "OutputRejected"
	// Unschedulable: no node could take the build within the limit.
	Unschedulable BuildFailure = "Unschedulable"
	// CredentialsRefused: the provider refused the credentials or the
	// installation is suspended.
	CredentialsRefused BuildFailure = "CredentialsRefused"
	// InvalidBudget: the Job could not be created with its budget.
	InvalidBudget BuildFailure = "InvalidBudget"
)

// ParseBuildFailure reads a failure code as stored.
func ParseBuildFailure(s string) (BuildFailure, error) {
	switch f := BuildFailure(s); f {
	case BuildError, OutOfMemory, DiskFull, DeadlineExceeded, LostWorker, SourceUnavailable, OutputRejected,
		Unschedulable, CredentialsRefused, InvalidBudget:
		return f, nil
	}
	return "", kerr.New(kerr.Validation, "unknown build failure `%s`", s)
}

// String is the stored code.
func (f BuildFailure) String() string { return string(f) }

// UnmarshalText refuses a failure that does not exist.
func (f *BuildFailure) UnmarshalText(text []byte) error {
	parsed, err := ParseBuildFailure(string(text))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

// Retryable reports whether this is an infrastructure failure a new attempt
// may not repeat.
func (f BuildFailure) Retryable() bool { return f == LostWorker }

// Explain is a short explanation with the next step for the user; empty for
// an unknown failure.
func (f BuildFailure) Explain() string {
	switch f {
	case BuildError:
		return "the build failed; see the build log"
	case OutOfMemory:
		return "the build ran out of memory; raise the build memory limit or use a build node"
	case DiskFull:
		return "the build ran out of disk; raise the build storage limit or shrink the context"
	case DeadlineExceeded:
		return "the build ran past its deadline; raise the deadline or speed the build up"
	case LostWorker:
		return "the build pod or its node was lost; the build is retried"
	case SourceUnavailable:
		return "the commit could not be fetched; check the repository and the app's access"
	case OutputRejected:
		return "the build reported no verifiable image"
	case Unschedulable:
		return "no node had room for the build; add a build node or lower its requests"
	case CredentialsRefused:
		return "the Git provider refused access; check the installation"
	case InvalidBudget:
		return "the build budget is invalid"
	}
	return ""
}

// ContainerExit is a terminated container as the pod status reports it.
type ContainerExit struct {
	Name     string
	ExitCode int32
	Reason   opt.Val[string]
	Message  opt.Val[string]
}

// PodObservation has the facts about the build pod.
type PodObservation struct {
	// Phase is `Pending`, `Running`, `Succeeded`, `Failed` or `Unknown`.
	Phase   string
	Reason  opt.Val[string]
	Message opt.Val[string]
	// Exits are the containers (init or not) that have terminated.
	Exits []ContainerExit
	// Running are the containers currently running.
	Running []string
	// UnschedulableSecs is how long the pod has been unschedulable, if it is.
	UnschedulableSecs opt.Val[uint64]
}

// JobObservation has the facts about the build Job.
type JobObservation struct {
	Exists    bool
	Succeeded bool
	Failed    bool
	// FailedReason is the reason of the Job's `Failed` condition
	// (`DeadlineExceeded`, `BackoffLimitExceeded`).
	FailedReason opt.Val[string]
	Pod          opt.Val[PodObservation]
}

// BuildReport is what the build container writes to its termination log.
type BuildReport struct {
	Digest artifact.Digest `json:"digest"`
	// Strategy is the strategy the pod chose (`dockerfile` or `railpack`);
	// empty when the report does not say.
	Strategy string `json:"strategy"`
}

// UnmarshalJSON requires a valid digest, which a plain decode would leave
// zero when the field is missing or null.
func (r *BuildReport) UnmarshalJSON(data []byte) error {
	var raw struct {
		Digest   opt.Val[artifact.Digest] `json:"digest"`
		Strategy json.RawMessage          `json:"strategy"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	digest, ok := raw.Digest.Get()
	if !ok {
		return errors.New("build report: missing field `digest`")
	}
	strategy := ""
	if raw.Strategy != nil {
		if err := json.Unmarshal(raw.Strategy, &strategy); err != nil || string(raw.Strategy) == "null" {
			return errors.New("build report: `strategy` is not a string")
		}
	}
	*r = BuildReport{Digest: digest, Strategy: strategy}
	return nil
}

// JobVerdict is where a build stands, from its Job.
//
//sumtype:decl
type JobVerdict interface{ jobVerdict() }

type (
	// Pending: created, not started yet.
	Pending struct{}
	// Fetching the source.
	Fetching struct{}
	// Building (and pushing, which is the same command).
	Building struct{}
	// Finished with a report still to verify.
	Finished struct{ Report BuildReport }
	// Failed for a reason, with the detail to show.
	Failed struct {
		Failure BuildFailure
		Detail  string
	}
	// Gone: the Job does not exist.
	Gone struct{}
)

func (Pending) jobVerdict()  {}
func (Fetching) jobVerdict() {}
func (Building) jobVerdict() {}
func (Finished) jobVerdict() {}
func (Failed) jobVerdict()   {}
func (Gone) jobVerdict()     {}

// bounded trims text and cuts it to detailMax bytes without splitting a rune.
func bounded(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= detailMax {
		return text
	}
	end := detailMax
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func failed(failure BuildFailure, detail string) JobVerdict {
	if strings.TrimSpace(detail) == "" {
		return Failed{Failure: failure, Detail: failure.Explain()}
	}
	return Failed{Failure: failure, Detail: bounded(detail)}
}

func mentionsDisk(text opt.Val[string]) bool {
	t, ok := text.Get()
	if !ok {
		return false
	}
	t = asciiLower(t)
	return strings.Contains(t, "no space left on device") ||
		strings.Contains(t, "ephemeral-storage") ||
		strings.Contains(t, "ephemeral local storage")
}

// asciiLower lowers A-Z only, so no other letter can come to match.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

func is(o opt.Val[string], s string) bool {
	v, ok := o.Get()
	return ok && v == s
}

// Classify is the meaning of job for the attempt, by the precedence
// documented on each rule: resource kills first, then the deadline, the
// source, the build, the output and the worker.
func Classify(job JobObservation) JobVerdict {
	if !job.Exists {
		return Gone{}
	}
	pod, hasPod := job.Pod.Get()
	exits := make([]ContainerExit, 0, len(pod.Exits))
	scanEnded := false
	for _, e := range pod.Exits {
		if e.Name == ScanContainer {
			scanEnded = true
			continue
		}
		exits = append(exits, e)
	}
	if verdict, ok := classifyFailure(job, pod, exits); ok {
		return verdict
	}
	// 7. Success needs a parsable report from the build container; a scan
	// that ended, however it ended, does not change that.
	if job.Succeeded || (job.Failed && scanEnded) {
		if report, ok := reportOf(exits); ok {
			return Finished{Report: report}
		}
		return failed(OutputRejected, "the build container reported no image digest")
	}
	// 8. A failed Job without an explanation lost its pod.
	if job.Failed {
		return failed(LostWorker, job.FailedReason.Or("the build pod disappeared"))
	}
	if !hasPod {
		return Pending{}
	}
	return progress(pod, exits)
}

// classifyFailure applies rules 1 to 6 to a Job that exists. The zero pod
// stands for no pod: none of its facts is set. exits leaves the scan out.
func classifyFailure(job JobObservation, pod PodObservation, exits []ContainerExit) (JobVerdict, bool) {
	if verdict, ok := resourceKill(pod, exits); ok {
		return verdict, true
	}
	// 3. The Job's activeDeadlineSeconds.
	if is(job.FailedReason, "DeadlineExceeded") || is(pod.Reason, "DeadlineExceeded") {
		return failed(DeadlineExceeded, ""), true
	}
	// 4. Any other eviction or a vanished node is the worker, not the build.
	if is(pod.Reason, "Evicted") || pod.Phase == "Unknown" || is(pod.Reason, "NodeLost") {
		return failed(LostWorker, pod.Message.Or("")), true
	}
	for _, e := range exits {
		if e.ExitCode == 0 {
			continue
		}
		// 5. The fetch init container failed.
		if e.Name == FetchContainer {
			return failed(SourceUnavailable, e.Message.Or("")), true
		}
		// 6. The build container failed.
		return failed(BuildError, e.Message.Or(fmt.Sprintf("%s exited with code %d", e.Name, e.ExitCode))), true
	}
	return nil, false
}

// resourceKill applies rules 1 and 2: the build was killed for memory or disk.
func resourceKill(pod PodObservation, exits []ContainerExit) (JobVerdict, bool) {
	// 1. The kernel's OOM killer is unambiguous.
	for _, e := range exits {
		if is(e.Reason, "OOMKilled") {
			return failed(OutOfMemory, ""), true
		}
	}
	// 2. An eviction for ephemeral storage, or a build that saw ENOSPC.
	disk := is(pod.Reason, "Evicted") && mentionsDisk(pod.Message)
	for _, e := range exits {
		disk = disk || (e.ExitCode != 0 && mentionsDisk(e.Message))
	}
	if disk {
		return failed(DiskFull, pod.Message.Or("")), true
	}
	return nil, false
}

// reportOf is the report of the build container that exited cleanly.
func reportOf(exits []ContainerExit) (BuildReport, bool) {
	for _, e := range exits {
		if e.Name != BuildContainer || e.ExitCode != 0 {
			continue
		}
		message, ok := e.Message.Get()
		if !ok {
			return BuildReport{}, false
		}
		var report BuildReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(message)), &report); err != nil {
			return BuildReport{}, false
		}
		return report, true
	}
	return BuildReport{}, false
}

// progress is where a Job that neither ended nor failed stands.
func progress(pod PodObservation, exits []ContainerExit) JobVerdict {
	if secs, ok := pod.UnschedulableSecs.Get(); ok && secs >= UnschedulableLimitSecs {
		return failed(Unschedulable, pod.Message.Or(""))
	}
	fetching := false
	for _, c := range pod.Running {
		if c == BuildContainer || c == ScanContainer {
			return Building{}
		}
		fetching = fetching || c == FetchContainer
	}
	for _, e := range exits {
		fetching = fetching || e.Name == FetchContainer
	}
	if fetching {
		return Fetching{}
	}
	return Pending{}
}

package outcome_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func exit(name string, code int32, reason, message string) outcome.ContainerExit {
	e := outcome.ContainerExit{Name: name, ExitCode: code}
	if reason != "" {
		e.Reason = opt.Some(reason)
	}
	if message != "" {
		e.Message = opt.Some(message)
	}
	return e
}

func job(pod outcome.PodObservation) outcome.JobObservation {
	return outcome.JobObservation{Exists: true, Pod: opt.Some(pod)}
}

func failure(v outcome.JobVerdict) outcome.BuildFailure {
	if f, ok := v.(outcome.Failed); ok {
		return f.Failure
	}
	return ""
}

func TestAMissingJobIsGone(t *testing.T) {
	if got := outcome.Classify(outcome.JobObservation{}); got != (outcome.Gone{}) {
		t.Fatalf("got %#v", got)
	}
}

func TestProgressFollowsTheRunningContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  outcome.JobObservation
		want outcome.JobVerdict
	}{
		{"pending", job(outcome.PodObservation{Phase: "Pending"}), outcome.Pending{}},
		{
			"fetching",
			job(outcome.PodObservation{Phase: "Pending", Running: []string{outcome.FetchContainer}}),
			outcome.Fetching{},
		},
		{
			"fetched, nothing running yet",
			job(outcome.PodObservation{
				Phase: "Pending",
				Exits: []outcome.ContainerExit{exit(outcome.FetchContainer, 0, "Completed", "")},
			}),
			outcome.Fetching{},
		},
		{
			"building",
			job(outcome.PodObservation{
				Phase:   "Running",
				Exits:   []outcome.ContainerExit{exit(outcome.FetchContainer, 0, "Completed", "")},
				Running: []string{outcome.BuildContainer},
			}),
			outcome.Building{},
		},
		{"no pod yet", outcome.JobObservation{Exists: true}, outcome.Pending{}},
	} {
		if got := outcome.Classify(tc.job); got != tc.want {
			t.Errorf("%s: got %#v", tc.name, got)
		}
	}
}

func doneWith(message string) outcome.JobObservation {
	done := job(outcome.PodObservation{
		Phase: "Succeeded",
		Exits: []outcome.ContainerExit{
			exit(outcome.FetchContainer, 0, "", ""),
			exit(outcome.BuildContainer, 0, "Completed", message),
		},
	})
	done.Succeeded = true
	return done
}

func TestSuccessNeedsAValidReport(t *testing.T) {
	got := outcome.Classify(doneWith(`{"digest":"` + digest + `","strategy":"railpack"}`))
	finished, ok := got.(outcome.Finished)
	if !ok || finished.Report.Digest.String() != digest || finished.Report.Strategy != "railpack" {
		t.Fatalf("got %#v", got)
	}
	for name, message := range map[string]string{
		"forged":        `{"digest":"sha256:nope"}`,
		"silent":        "",
		"no digest":     `{"strategy":"railpack"}`,
		"null digest":   `{"digest":null}`,
		"null strategy": `{"digest":"` + digest + `","strategy":null}`,
		"not json":      digest,
	} {
		got = outcome.Classify(doneWith(message))
		want := outcome.Failed{Failure: outcome.OutputRejected, Detail: "the build container reported no image digest"}
		if got != want {
			t.Errorf("%s: got %#v", name, got)
		}
	}
	padded := outcome.Classify(doneWith(" {\"digest\":\"" + digest + "\",\"extra\":1}\n"))
	if f, isFinished := padded.(outcome.Finished); !isFinished || f.Report.Strategy != "" {
		t.Errorf("the strategy is optional and the message is trimmed: got %#v", padded)
	}
}

func TestOutOfMemoryWinsOverAFailedJob(t *testing.T) {
	oom := job(outcome.PodObservation{
		Phase: "Failed",
		Exits: []outcome.ContainerExit{exit(outcome.BuildContainer, 137, "OOMKilled", "")},
	})
	oom.Failed = true
	oom.FailedReason = opt.Some("BackoffLimitExceeded")
	got, ok := outcome.Classify(oom).(outcome.Failed)
	if !ok || got.Failure != outcome.OutOfMemory || !strings.Contains(got.Detail, "memory") {
		t.Fatalf("got %#v", got)
	}
}

func TestAFullDiskIsToldApartFromOtherEvictions(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  outcome.PodObservation
		want outcome.BuildFailure
	}{
		{
			"evicted for storage",
			outcome.PodObservation{
				Phase: "Failed", Reason: opt.Some("Evicted"),
				Message: opt.Some("Pod ephemeral local storage usage exceeds the total limit of containers 2Gi."),
			},
			outcome.DiskFull,
		},
		{
			"ENOSPC in the build",
			outcome.PodObservation{
				Phase: "Failed",
				Exits: []outcome.ContainerExit{
					exit(outcome.BuildContainer, 1, "Error", "write /tmp/x: No Space Left On Device"),
				},
			},
			outcome.DiskFull,
		},
		{
			"evicted for memory pressure",
			outcome.PodObservation{
				Phase: "Failed", Reason: opt.Some("Evicted"),
				Message: opt.Some("The node was low on resource: memory."),
			},
			outcome.LostWorker,
		},
	} {
		if got := failure(outcome.Classify(job(tc.pod))); got != tc.want {
			t.Errorf("%s: got %s", tc.name, got)
		}
	}
	if !outcome.LostWorker.Retryable() || outcome.DiskFull.Retryable() {
		t.Error("only a lost worker is retried")
	}
}

func TestTheDeadlineIsReportedAsSuch(t *testing.T) {
	late := outcome.JobObservation{Exists: true, Failed: true, FailedReason: opt.Some("DeadlineExceeded")}
	if got := failure(outcome.Classify(late)); got != outcome.DeadlineExceeded {
		t.Fatalf("got %s", got)
	}
	pod := job(outcome.PodObservation{Phase: "Failed", Reason: opt.Some("DeadlineExceeded")})
	if got := failure(outcome.Classify(pod)); got != outcome.DeadlineExceeded {
		t.Fatalf("got %s", got)
	}
}

func TestFetchAndBuildErrorsKeepTheirMessage(t *testing.T) {
	fetch := job(outcome.PodObservation{
		Phase: "Failed",
		Exits: []outcome.ContainerExit{exit(outcome.FetchContainer, 128, "Error", "fatal: reference is not a tree")},
	})
	want := outcome.Failed{Failure: outcome.SourceUnavailable, Detail: "fatal: reference is not a tree"}
	if got := outcome.Classify(fetch); got != want {
		t.Errorf("got %#v", got)
	}
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{"long", strings.Repeat("e", 5000), strings.Repeat("e", 1024)},
		{"cut on a rune boundary", strings.Repeat("é", 600), strings.Repeat("é", 512)},
		{"a rune across the limit", "x" + strings.Repeat("é", 600), "x" + strings.Repeat("é", 511)},
		{"trimmed", "  step 3 failed\n", "step 3 failed"},
		{"silent", "", "build exited with code 1"},
		{"blank", "   ", "the build failed; see the build log"},
	} {
		build := job(outcome.PodObservation{
			Phase: "Failed",
			Exits: []outcome.ContainerExit{
				exit(outcome.FetchContainer, 0, "", ""),
				{Name: outcome.BuildContainer, ExitCode: 1, Reason: opt.Some("Error")},
			},
		})
		if tc.message != "" {
			pod, _ := build.Pod.Get()
			pod.Exits[1].Message = opt.Some(tc.message)
		}
		got, ok := outcome.Classify(build).(outcome.Failed)
		if !ok || got.Failure != outcome.BuildError || got.Detail != tc.want || !utf8.ValidString(got.Detail) {
			t.Errorf("%s: got %s, %d bytes", tc.name, got.Failure, len(got.Detail))
		}
	}
}

func TestTheScanNeverDecidesTheBuild(t *testing.T) {
	report := `{"digest":"` + digest + `","strategy":"dockerfile"}`
	scanning := job(outcome.PodObservation{
		Phase: "Running",
		Exits: []outcome.ContainerExit{
			exit(outcome.FetchContainer, 0, "", ""),
			exit(outcome.BuildContainer, 0, "Completed", report),
		},
		Running: []string{outcome.ScanContainer},
	})
	if got := outcome.Classify(scanning); got != (outcome.Building{}) {
		t.Errorf("got %#v", got)
	}
	for _, scan := range []outcome.ContainerExit{
		exit(outcome.ScanContainer, 0, "Completed", `{"status":"ok"}`),
		exit(outcome.ScanContainer, 137, "OOMKilled", ""),
		exit(outcome.ScanContainer, 1, "Error", "no space left on device"),
	} {
		failed := scan.ExitCode != 0
		done := outcome.JobObservation{Exists: true, Succeeded: !failed, Failed: failed}
		phase := "Succeeded"
		if failed {
			done.FailedReason = opt.Some("BackoffLimitExceeded")
			phase = "Failed"
		}
		done.Pod = opt.Some(outcome.PodObservation{
			Phase: phase,
			Exits: []outcome.ContainerExit{
				exit(outcome.FetchContainer, 0, "", ""),
				exit(outcome.BuildContainer, 0, "Completed", report),
				scan,
			},
		})
		got, ok := outcome.Classify(done).(outcome.Finished)
		if !ok || got.Report.Digest.String() != digest {
			t.Errorf("%+v: got %#v", scan, got)
		}
	}
}

func TestLostPodsAndUnschedulableBuilds(t *testing.T) {
	vanished := outcome.JobObservation{Exists: true, Failed: true}
	want := outcome.Failed{Failure: outcome.LostWorker, Detail: "the build pod disappeared"}
	if got := outcome.Classify(vanished); got != want {
		t.Errorf("got %#v", got)
	}
	backoff := outcome.JobObservation{Exists: true, Failed: true, FailedReason: opt.Some("BackoffLimitExceeded")}
	want = outcome.Failed{Failure: outcome.LostWorker, Detail: "BackoffLimitExceeded"}
	if got := outcome.Classify(backoff); got != want {
		t.Errorf("got %#v", got)
	}
	for _, reason := range []opt.Val[string]{opt.None[string](), opt.Some("NodeLost")} {
		phase := "Unknown"
		if reason.IsSome() {
			phase = "Running"
		}
		lost := job(outcome.PodObservation{Phase: phase, Reason: reason})
		if got := failure(outcome.Classify(lost)); got != outcome.LostWorker {
			t.Errorf("%s: got %s", phase, got)
		}
	}
	waiting := job(outcome.PodObservation{
		Phase: "Pending", UnschedulableSecs: opt.Some(outcome.UnschedulableLimitSecs - 1),
	})
	if got := outcome.Classify(waiting); got != (outcome.Pending{}) {
		t.Errorf("got %#v", got)
	}
	stuck := job(outcome.PodObservation{
		Phase: "Pending", UnschedulableSecs: opt.Some(outcome.UnschedulableLimitSecs),
		Message: opt.Some("0/1 nodes are available: 1 Insufficient memory."),
	})
	want = outcome.Failed{Failure: outcome.Unschedulable, Detail: "0/1 nodes are available: 1 Insufficient memory."}
	if got := outcome.Classify(stuck); got != want {
		t.Errorf("got %#v", got)
	}
}

func TestFailuresKeepTheirCodesAndExplainThemselves(t *testing.T) {
	codes := []string{
		"BuildError", "OutOfMemory", "DiskFull", "DeadlineExceeded", "LostWorker", "SourceUnavailable",
		"OutputRejected", "Unschedulable", "CredentialsRefused", "InvalidBudget",
	}
	for _, code := range codes {
		f, err := outcome.ParseBuildFailure(code)
		if err != nil || f.String() != code || f.Explain() == "" {
			t.Errorf("%s: got %s, %v", code, f, err)
		}
		raw, err := json.Marshal(f)
		if err != nil || string(raw) != `"`+code+`"` {
			t.Errorf("%s: json %s, %v", code, raw, err)
		}
		var back outcome.BuildFailure
		if err = json.Unmarshal(raw, &back); err != nil || back != f {
			t.Errorf("%s: back %s, %v", code, back, err)
		}
		if f.Retryable() != (f == outcome.LostWorker) {
			t.Errorf("%s: retryable", code)
		}
	}
	if _, err := outcome.ParseBuildFailure("outOfMemory"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("got %v", err)
	}
	var f outcome.BuildFailure
	if err := json.Unmarshal([]byte(`"Oops"`), &f); err == nil {
		t.Error("Oops is not a failure")
	}
	if outcome.BuildFailure("Oops").Explain() != "" || outcome.BuildFailure("Oops").Retryable() {
		t.Error("an unknown failure explains nothing and is not retried")
	}
}

func TestBuildReportsKeepTheirJSON(t *testing.T) {
	var report outcome.BuildReport
	in := `{"digest":"` + digest + `","strategy":"dockerfile"}`
	if err := json.Unmarshal([]byte(in), &report); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil || string(raw) != in {
		t.Fatalf("got %s, %v", raw, err)
	}
	if err = json.Unmarshal([]byte(`{"digest":"`+digest+`"}`), &report); err != nil || report.Strategy != "" {
		t.Fatalf("the strategy defaults to empty: %+v, %v", report, err)
	}
	raw, err = json.Marshal(report)
	if err != nil || string(raw) != `{"digest":"`+digest+`","strategy":""}` {
		t.Fatalf("got %s, %v", raw, err)
	}
}

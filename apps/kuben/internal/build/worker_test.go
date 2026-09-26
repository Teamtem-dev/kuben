package build_test

import (
	"encoding/json"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Ported from worker.rs.

func TestTheBuildRunShowsCoarsePhases(t *testing.T) {
	cases := map[opbuild.Phase]string{
		opbuild.Queued:          "Queued",
		opbuild.Blocked:         "Queued",
		opbuild.Preparing:       "Running",
		opbuild.Running:         "Running",
		opbuild.Publishing:      "Running",
		opbuild.VerifyingOutput: "Running",
		opbuild.CancelRequested: "Running",
		opbuild.Cancelling:      "Running",
		opbuild.Succeeded:       "Succeeded",
		opbuild.Failed:          "Failed",
		opbuild.Cancelled:       "Cancelled",
	}
	for phase, want := range cases {
		if got := build.CrdPhase(phase); got != want {
			t.Errorf("%s: %s, want %s", phase, got, want)
		}
	}
}

// at is the member path of a JSON document.
func at(doc map[string]any, path ...any) any {
	var v any = doc
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any) //nolint:errcheck // a missing member reads as nil
			v = m[k]
		case int:
			l, _ := v.([]any) //nolint:errcheck // as above
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func TestFinalStatusCarriesTheDigestAndTheReason(t *testing.T) {
	now := ms(t, "2026-09-17T12:00:00Z")
	a := attempt(t, source.BuildRecipe{})
	done := build.StatusPatch(a, opbuild.Succeeded, opt.Some(stepDigest), now)
	for path, want := range map[string]any{
		"phase": "Succeeded", "imageDigest": stepDigest, "finishedAt": "2026-09-17T12:00:00Z",
	} {
		if got := at(done, "status", path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	if got := at(done, "status", "conditions", 0, "status"); got != "True" {
		t.Errorf("condition %v", got)
	}
	data := check(json.Marshal(done["status"])).must(t)
	status := parse[v1alpha1.BuildRunStatus](t, string(data))
	if status.ImageDigest == nil || *status.ImageDigest != stepDigest {
		t.Errorf("not a valid BuildRunStatus: %+v", status)
	}

	oom := attempt(t, source.BuildRecipe{})
	oom.Failure = opt.Some("OutOfMemory")
	failed := build.StatusPatch(oom, opbuild.Failed, opt.None[string](), now)
	if at(failed, "status", "conditions", 0, "reason") != "OutOfMemory" || at(failed, "status", "conditions", 0, "status") != "False" {
		t.Errorf("failed %v", failed)
	}
	if _, ok := at(failed, "status").(map[string]any)["imageDigest"]; ok {
		t.Error("a failed build has a digest")
	}

	started := build.StatusPatch(a, opbuild.Preparing, opt.None[string](), now)
	if at(started, "status", "phase") != "Running" || at(started, "status", "startedAt") != "2026-09-17T12:00:00Z" {
		t.Errorf("started %v", started)
	}
	if _, ok := at(started, "status").(map[string]any)["finishedAt"]; ok {
		t.Error("a running build finished")
	}
}

func TestFinishedBuildRunsExpireAfterADay(t *testing.T) {
	run := build.RunOf(attempt(t, source.BuildRecipe{}), settings())
	now := ms(t, "2026-09-18T12:00:01Z")
	if build.Expired(run, now) {
		t.Error("a running build never expires")
	}
	run.Status = &v1alpha1.BuildRunStatus{FinishedAt: new("2026-09-17T12:00:00Z")}
	if !build.Expired(run, now) {
		t.Error("a day and a second")
	}
	if build.Expired(run, ms(t, "2026-09-18T11:59:59Z")) {
		t.Error("less than a day")
	}
	if build.OperationPhase(opbuild.Cancelled) != "cancelled" || build.OperationPhase(opbuild.Failed) != "failed" {
		t.Error("operation phases")
	}
}

func TestAPIErrorsAreToldApart(t *testing.T) {
	api := func(code int32) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{Code: code}}
	}
	if !build.IsNotFound(api(http.StatusNotFound)) || !build.IsConflict(api(http.StatusConflict)) {
		t.Error("404 or 409")
	}
	if !build.IsInvalid(api(http.StatusUnprocessableEntity)) || !build.IsInvalid(api(http.StatusBadRequest)) {
		t.Error("422 or 400")
	}
	if build.IsInvalid(api(http.StatusInternalServerError)) {
		t.Error("500 is not invalid")
	}
}

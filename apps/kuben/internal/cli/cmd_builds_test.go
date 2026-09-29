package cli

import (
	"bytes"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/apiclient"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

func testBuilds() []apiclient.BuildDto {
	return []apiclient.BuildDto{
		{
			ID: "0192f3a1-0000-7000-8000-000000000002", Attempt: 1, Repository: "acme/web", Branch: "main",
			Commit: "0123456789abcdef0123456789abcdef01234567", Strategy: "auto", Phase: "running",
			CreatedAt: 1_790_000_000_000, StartedAt: opt.Some[int64](1_790_000_001_000),
			Stages: []apiclient.BuildStageDto{
				{Name: "clone", Status: "succeeded"}, {Name: "plan", Status: "succeeded"}, {Name: "build", Status: "running"},
			},
		},
		{
			ID: "0192f3a1-0000-7000-8000-000000000001", Attempt: 1, Repository: "acme/web", Branch: "main",
			Commit: "fedcba9876543210fedcba9876543210fedcba98", Strategy: "dockerfile", Phase: "failed",
			Failure: opt.Some("BuildError"), CreatedAt: 1_789_990_000_000,
			Stages: []apiclient.BuildStageDto{{Name: "clone", Status: "succeeded"}, {Name: "build", Status: "failed"}},
		},
		{
			ID: "0192f3a1-0000-7000-8000-000000000000", Attempt: 2, Repository: "acme/web", Branch: "main",
			Commit: "abc", Strategy: "auto", Phase: "queued", CreatedAt: 1_789_980_000_000,
		},
	}
}

func TestBuildsArePrintedAsATable(t *testing.T) {
	var out bytes.Buffer
	if err := printBuilds(&out, testBuilds(), false); err != nil {
		t.Fatal(err)
	}
	want := "BUILD                                 PHASE                STAGE           COMMIT        BRANCH  CREATED\n" +
		"0192f3a1-0000-7000-8000-000000000002  running              build           0123456789ab  main    2026-09-21T14:13:20Z\n" +
		"0192f3a1-0000-7000-8000-000000000001  failed (BuildError)  build (failed)  fedcba987654  main    2026-09-21T11:26:40Z\n" +
		"0192f3a1-0000-7000-8000-000000000000  queued               -               abc           main    2026-09-21T08:40:00Z\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestBuildsJSONKeepsTheAPIsFields(t *testing.T) {
	var out bytes.Buffer
	if err := printBuilds(&out, nil, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Errorf("empty: %q", out.String())
	}
	out.Reset()
	if err := printBuilds(&out, testBuilds()[2:], true); err != nil {
		t.Fatal(err)
	}
	want := `[
  {
    "id": "0192f3a1-0000-7000-8000-000000000000",
    "attempt": 2,
    "repository": "acme/web",
    "branch": "main",
    "commit": "abc",
    "strategy": "auto",
    "phase": "queued",
    "blockedReason": null,
    "failure": null,
    "failureDetail": null,
    "image": null,
    "release": null,
    "deployment": null,
    "deployDecision": null,
    "cancelRequested": false,
    "createdAt": 1789980000000,
    "startedAt": null,
    "finishedAt": null
  }
]
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func buildEvents(events ...apiclient.FollowEvent) iter.Seq2[apiclient.FollowEvent, error] {
	return func(yield func(apiclient.FollowEvent, error) bool) {
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func TestFollowedBuildLogsNameTheirStage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	events := buildEvents(
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "kbuild-1", Process: opt.Some("clone"), Line: "cloned"}},
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "kbuild-1", Line: "no stage"}},
		apiclient.EndEvent{LogEnd: apiclient.LogEnd{Pod: opt.Some("kbuild-1"), Error: opt.Some("the pod is gone")}},
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "kbuild-1", Line: "never printed"}},
	)
	if err := printFollowedBuild(&stdout, &stderr, events); err != nil {
		t.Fatal(err)
	}
	if want := "[clone] cloned\nno stage\n"; stdout.String() != want {
		t.Errorf("stdout %q", stdout.String())
	}
	if want := "the pod is gone\n"; stderr.String() != want {
		t.Errorf("stderr %q", stderr.String())
	}
	broken := func(yield func(apiclient.FollowEvent, error) bool) { yield(nil, errors.New("unexpected event: EOF")) }
	if err := printFollowedBuild(&stdout, &stderr, broken); err == nil {
		t.Error("a broken stream is an error")
	}
}

func TestTriggersAndCancelsSayWhatHappened(t *testing.T) {
	app := apiclient.AppPath{Project: "shop", Environment: "prod", App: "web"}
	var out bytes.Buffer
	triggered := apiclient.TriggeredBuildDto{SyncOperation: "0192f3a1-0000-7000-8000-00000000000a", Build: opt.Some(testBuilds()[0])}
	if err := printTriggered(&out, app, triggered); err != nil {
		t.Fatal(err)
	}
	want := "shop/prod/web: building the source head (sync 0192f3a1-0000-7000-8000-00000000000a)\n" +
		"newest build  0192f3a1-0000-7000-8000-000000000002  running  0123456789ab\n" +
		"follow it with: kuben builds shop/prod/web\n"
	if out.String() != want {
		t.Errorf("got:\n%s", out.String())
	}
	out.Reset()
	if err := printTriggered(&out, app, apiclient.TriggeredBuildDto{SyncOperation: "op"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "newest build") {
		t.Errorf("no build yet:\n%s", out.String())
	}
	out.Reset()
	stopping := testBuilds()[0]
	stopping.Phase = "cancelRequested"
	cancelled := testBuilds()[2]
	cancelled.Phase = "cancelled"
	for _, b := range []apiclient.BuildDto{stopping, cancelled} {
		if err := printCancelled(&out, app, b); err != nil {
			t.Fatal(err)
		}
	}
	want = "shop/prod/web: build 0192f3a1-0000-7000-8000-000000000002 is stopping (cancelRequested)\n" +
		"shop/prod/web: build 0192f3a1-0000-7000-8000-000000000000 cancelled\n"
	if out.String() != want {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestBuildIdsAreCheckedBeforeTheyGoIntoAPath(t *testing.T) {
	if got, err := buildID("0192F3A1-0000-7000-8000-000000000002"); err != nil || got != "0192f3a1-0000-7000-8000-000000000002" {
		t.Errorf("%q %v", got, err)
	}
	for _, bad := range []string{"", "../x", "b1"} {
		if _, err := buildID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBuildsSubcommandsAreFound(t *testing.T) {
	root := Root()
	for _, args := range [][]string{
		{"builds", "web"}, {"builds", "logs", "web", "b"}, {"builds", "trigger", "web"}, {"builds", "cancel", "web", "b"},
	} {
		c, rest, err := root.Find(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if err := c.ValidateArgs(rest); err != nil {
			t.Errorf("%v: %s %v", args, c.Name(), err)
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/internal/apiclient"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// noClientEnv is an environment without variables.
func noClientEnv(string) (string, bool) { return "", false }

// clientOptCmp compares values holding opt.Val fields.
func clientOptCmp() cmp.Option {
	return cmp.AllowUnexported(opt.Val[string]{}, opt.Val[int64]{}, opt.Val[bool]{})
}

func spaces(n int) string { return strings.Repeat(" ", n) }

func TestContextsAreWrittenForTheirOwnerOnlyAndReadBack(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kuben", "contexts.json")
	doc := contextsDoc{
		Current: opt.Some("prod"),
		Contexts: map[string]serverContext{
			"prod": {URL: "https://kuben.example.com", Token: "kbn_x", Project: opt.Some("shop")},
		},
	}
	if err := doc.save(file); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode %o", mode)
	}
	parent, err := os.Stat(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if mode := parent.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode %o", mode)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "current": "prod",
  "contexts": {
    "prod": {
      "url": "https://kuben.example.com",
      "token": "kbn_x",
      "project": "shop"
    }
  }
}`
	if string(text) != want {
		t.Errorf("file:\n%s", text)
	}
	read, err := loadContexts(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(doc, read, clientOptCmp()); diff != "" {
		t.Errorf("(-saved +read):\n%s", diff)
	}
	current, err := read.resolve(opt.None[string](), noClientEnv)
	if err != nil || current.Project.Or("") != "shop" {
		t.Errorf("current: %v %v", current, err)
	}
	if _, err := read.resolve(opt.Some("staging"), noClientEnv); err == nil ||
		err.Error() != "no context `staging`; kuben login creates it" {
		t.Errorf("staging: %v", err)
	}
	empty, err := loadContexts(filepath.Join(dir, "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(contextsDoc{Contexts: map[string]serverContext{}}, empty, clientOptCmp()); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if _, err := empty.resolve(opt.None[string](), noClientEnv); err == nil ||
		err.Error() != "not logged in: kuben login https://<your server> (or set KUBEN_URL and KUBEN_TOKEN)" {
		t.Errorf("not logged in: %v", err)
	}
}

func TestTheEnvironmentOverridesTheContexts(t *testing.T) {
	env := func(name string) (string, bool) {
		switch name {
		case "KUBEN_URL":
			return "https://ci.example.com", true
		case "KUBEN_TOKEN":
			return "kbn_ci", true
		}
		return "", false
	}
	got, err := contextsDoc{}.resolve(opt.Some("prod"), env)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(serverContext{URL: "https://ci.example.com", Token: "kbn_ci"}, got, clientOptCmp()); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	files := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"KUBEN_CONTEXT_FILE": "/x/c.json", "HOME": "/home/u"}, "/x/c.json"},
		{map[string]string{"XDG_CONFIG_HOME": "/cfg", "HOME": "/home/u"}, "/cfg/kuben/contexts.json"},
		{map[string]string{"HOME": "/home/u"}, "/home/u/.config/kuben/contexts.json"},
	}
	for _, f := range files {
		got, err := contextsFile(func(name string) (string, bool) { v, ok := f.env[name]; return v, ok })
		if err != nil || got != f.want {
			t.Errorf("contextsFile(%v) = %s, %v", f.env, got, err)
		}
	}
	if _, err := contextsFile(noClientEnv); err == nil {
		t.Error("no HOME accepted")
	}
	if got := pathWithExtension("/a/contexts.json", "json.partial"); got != "/a/contexts.json.partial" {
		t.Errorf("partial %s", got)
	}
}

func TestTargetsFallBackToTheEnvironment(t *testing.T) {
	t.Setenv("KUBEN_PROJECT", "shop")
	cmd := &cobra.Command{}
	read := targetFlags(cmd)
	if err := cmd.ParseFlags([]string{"--environment=prod"}); err != nil {
		t.Fatal(err)
	}
	if err := applyEnv(cmd); err != nil {
		t.Fatal(err)
	}
	want := clientTarget{project: opt.Some("shop"), environment: opt.Some("prod")}
	if diff := cmp.Diff(want, read(), clientOptCmp(), cmp.AllowUnexported(clientTarget{})); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func testRelease(revision int64, current bool) apiclient.ReleaseDto {
	return apiclient.ReleaseDto{Revision: revision, Reason: "deploy", Current: current}
}

func TestRevisionsAreFoundInTheHistory(t *testing.T) {
	history := []apiclient.ReleaseDto{testRelease(5, false), testRelease(4, true), testRelease(3, false), testRelease(1, false)}
	if got := currentRevision(history); got != 4 {
		t.Errorf("current %d", got)
	}
	if got, ok := previousRevision(history).Get(); !ok || got != 3 {
		t.Errorf("previous %d %v", got, ok)
	}
	if got := currentRevision(nil); got != 0 {
		t.Errorf("empty current %d", got)
	}
	if previousRevision([]apiclient.ReleaseDto{testRelease(1, true)}).IsSome() {
		t.Error("a first revision has a previous one")
	}
}

func TestHostsNameContexts(t *testing.T) {
	for url, want := range map[string]string{
		"https://kuben.example.com/": "kuben.example.com",
		"http://127.0.0.1:3000":      "127.0.0.1",
		"kuben.example.com":          "kuben.example.com",
	} {
		if got := hostOf(url); got != want {
			t.Errorf("hostOf(%s) = %s", url, got)
		}
	}
	if got := emailOf(map[string]any{"email": "ana@example.com"}); got != "ana@example.com" {
		t.Errorf("email %s", got)
	}
	if got := emailOf([]any{}); got != "?" {
		t.Errorf("no email %s", got)
	}
}

func TestAppsArePrintedAsATable(t *testing.T) {
	rows := []apiclient.AppDto{
		{
			Name: "web", Project: "shop", Environment: "prod", Ready: true,
			Image: opt.Some("ghcr.io/acme/web@sha256:1"), URL: opt.Some("https://web.example.com"),
			Processes: []apiclient.ProcessDto{{Name: "web"}, {Name: "worker"}},
		},
		{Name: "api", Project: "shop", Environment: "dev"},
		{Name: "db", Project: "shop", Environment: "dev", Reason: opt.Some("Progressing")},
	}
	var out bytes.Buffer
	if err := printApps(&out, rows, false); err != nil {
		t.Fatal(err)
	}
	want := "APP" + spaces(12) + "STATE" + spaces(8) + "IMAGE" + spaces(22) + "URL" + spaces(22) + "PROCESSES\n" +
		"shop/prod/web  ready" + spaces(8) + "ghcr.io/acme/web@sha256:1  https://web.example.com  2\n" +
		"shop/dev/api   not ready" + spaces(2+2+25+2+23+2) + "0\n" +
		"shop/dev/db    Progressing" + spaces(2+25+2+23+2) + "0\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}

	out.Reset()
	if err := printApps(&out, []apiclient.AppDto{}, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Errorf("empty JSON %q", out.String())
	}
	out.Reset()
	if err := printApps(&out, rows[1:2], true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "[\n  {\n    \"name\": \"api\",\n    \"project\": \"shop\",") ||
		strings.Contains(out.String(), "paused") {
		t.Errorf("JSON in declaration order, paused left out:\n%s", out.String())
	}
}

func TestTableColumnsCountCharacters(t *testing.T) {
	var out bytes.Buffer
	if err := printClientTable(&out, []string{"A", "B"}, [][]string{{"é", "x"}, {"ab", ""}}); err != nil {
		t.Fatal(err)
	}
	if want := "A   B\né   x\nab\n"; out.String() != want {
		t.Errorf("got %q", out.String())
	}
}

func statusFixture() (apiclient.AppPath, apiclient.AppDetail, []apiclient.ReleaseDto, apiclient.DoctorReport) {
	path := apiclient.AppPath{Project: "shop", Environment: "prod", App: "web"}
	detail := apiclient.AppDetail{
		App: apiclient.AppDto{
			Name: "web", Project: "shop", Environment: "prod", Namespace: "shop-prod",
			Reason: opt.Some("RolloutFailed"), Message: opt.Some("Back-off pulling image"),
			Image: opt.Some("ghcr.io/acme/web@sha256:abc"),
		},
		Pods: []apiclient.PodDto{
			{Name: "web-1", Phase: "running", Ready: true},
			{Name: "web-2", Phase: "running", Reason: opt.Some("CrashLoopBackOff"), Restarts: 3},
		},
	}
	releases := []apiclient.ReleaseDto{
		{Revision: 5, Reason: "deploy"},
		{Revision: 4, Reason: "deploy", Actor: opt.Some("ana@example.com"), Current: true},
	}
	doctor := apiclient.DoctorReport{
		Status: "fail",
		Checks: []apiclient.DoctorCheck{
			{ID: "gateway", Status: "ok", Detail: "accepted"},
			{ID: "dns", Subject: "shop.example.com", Status: "fail", Detail: "no A record", Hint: opt.Some("point shop.example.com at 203.0.113.7")},
			{ID: "agent", Status: "unknown", Detail: "not connected"},
			{ID: "route", Status: "warn", Detail: "not accepted yet"},
		},
	}
	return path, detail, releases, doctor
}

func TestAppStatusReadsLikeTheRustCLI(t *testing.T) {
	path, detail, releases, doctor := statusFixture()
	var out bytes.Buffer
	if err := printStatus(&out, path, detail, releases, doctor, nil); err != nil {
		t.Fatal(err)
	}
	want := `shop/prod/web  not ready (RolloutFailed)
  Back-off pulling image
image     ghcr.io/acme/web@sha256:abc
url       -
revision  4 (deploy by ana@example.com)
pods      1/2 ready
  web-1  running  restarts 0
  web-2  running (CrashLoopBackOff)  restarts 3
doctor    fail
  [OK  ] gateway: accepted
  [FAIL] dns shop.example.com: no A record
         point shop.example.com at 203.0.113.7
  [????] agent: not connected
  [WARN] route: not accepted yet
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	detail.App.Ready, detail.App.Reason, detail.App.Message = true, opt.None[string](), opt.Some("")
	detail.Pods = nil
	forbidden := apiclient.APIError{Status: 403, Code: "forbidden"}
	if err := printStatus(&out, path, detail, nil, doctor, forbidden); err != nil {
		t.Fatal(err)
	}
	want = `shop/prod/web  ready
image     ghcr.io/acme/web@sha256:abc
url       -
pods      0/0 ready
doctor    unavailable: 403 (forbidden)
`
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestAppStatusJSONHasSortedKeys(t *testing.T) {
	detail := apiclient.AppDetail{App: apiclient.AppDto{
		Name: "web", Project: "shop", Environment: "prod", Namespace: "shop-prod", Ready: true,
		Processes: []apiclient.ProcessDto{}, Env: []apiclient.EnvVarDto{}, Domains: []string{}, Volumes: []apiclient.VolumeDto{},
	}}
	releases := []apiclient.ReleaseDto{{Revision: 1, Reason: "create", CreatedAt: 1700000000000, Current: true}}
	doctor := apiclient.DoctorReport{
		Status: "ok", Checks: []apiclient.DoctorCheck{},
		Graph: json.RawMessage(`{"b":1.0,"a":"<x>"}`), Findings: json.RawMessage(`[]`),
	}
	var out bytes.Buffer
	if err := printStatusJSON(&out, detail, releases, opt.Some(doctor)); err != nil {
		t.Fatal(err)
	}
	want := `{
  "app": {
    "created_at": null,
    "domains": [],
    "env": [],
    "environment": "prod",
    "exposure": null,
    "git_repo": null,
    "image": null,
    "message": null,
    "name": "web",
    "namespace": "shop-prod",
    "processes": [],
    "project": "shop",
    "ready": true,
    "reason": null,
    "url": null,
    "volumes": []
  },
  "doctor": {
    "checks": [],
    "findings": [],
    "graph": {
      "a": "<x>",
      "b": 1.0
    },
    "status": "ok"
  },
  "pods": [],
  "releases": [
    {
      "actor": null,
      "created_at": 1700000000000,
      "current": true,
      "image": null,
      "note": null,
      "reason": "create",
      "revision": 1
    }
  ]
}
`
	if out.String() != want {
		t.Errorf("got:\n%s", out.String())
	}
	out.Reset()
	if err := printStatusJSON(&out, detail, releases, opt.None[apiclient.DoctorReport]()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\n  \"doctor\": null,\n") {
		t.Errorf("no doctor:\n%s", out.String())
	}
}

func TestLogsLoseTheirTimestampAndGainThePod(t *testing.T) {
	pods := []apiclient.PodLogs{
		{Pod: "web-1", Lines: []string{"2026-09-25T10:00:00.123456789Z hello", "short line"}},
		{Pod: "web-2", Lines: []string{}, Error: opt.Some("container is starting")},
	}
	var stdout, stderr bytes.Buffer
	if err := printLogs(&stdout, &stderr, pods); err != nil {
		t.Fatal(err)
	}
	if want := "[web-1] hello\n[web-1] short line\n"; stdout.String() != want {
		t.Errorf("stdout %q", stdout.String())
	}
	if want := "[web-2] container is starting\n"; stderr.String() != want {
		t.Errorf("stderr %q", stderr.String())
	}
	stdout.Reset()
	if err := printLogs(&stdout, &stderr, pods[:1]); err != nil {
		t.Fatal(err)
	}
	if want := "hello\nshort line\n"; stdout.String() != want {
		t.Errorf("one pod %q", stdout.String())
	}
}

func TestFollowedLogsStopAtTheEndOfTheStream(t *testing.T) {
	events := []apiclient.FollowEvent{
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "web-1", Line: "2026-09-25T10:00:00Z kept as sent"}},
		apiclient.EndEvent{LogEnd: apiclient.LogEnd{Pod: opt.Some("web-1"), Error: opt.Some("container exited")}},
		apiclient.EndEvent{LogEnd: apiclient.LogEnd{Pod: opt.Some("web-2")}},
		apiclient.EndEvent{},
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "web-3", Line: "never printed"}},
	}
	seq := func(yield func(apiclient.FollowEvent, error) bool) {
		for _, e := range events {
			if !yield(e, nil) {
				return
			}
		}
	}
	var stdout, stderr bytes.Buffer
	if err := printFollowed(&stdout, &stderr, seq); err != nil {
		t.Fatal(err)
	}
	if want := "[web-1] 2026-09-25T10:00:00Z kept as sent\n"; stdout.String() != want {
		t.Errorf("stdout %q", stdout.String())
	}
	if want := "[web-1] container exited\n[web-2] log ended\nthe stream ended\n"; stderr.String() != want {
		t.Errorf("stderr %q", stderr.String())
	}
	broken := func(yield func(apiclient.FollowEvent, error) bool) {
		yield(nil, apiclient.TransportError{Reason: "unexpected event: EOF"})
	}
	if err := printFollowed(&stdout, &stderr, broken); err == nil || err.Error() != "unexpected event: EOF" {
		t.Errorf("err = %v", err)
	}
}

// fakePacer moves its clock forward on every sleep.
type fakePacer struct{ at time.Time }

func (p *fakePacer) now() time.Time { return p.at }

func (p *fakePacer) sleep(_ context.Context, d time.Duration) error {
	p.at = p.at.Add(d)
	return nil
}

func TestDeploymentsAreWaitedFor(t *testing.T) {
	app := apiclient.AppPath{Project: "shop", Environment: "prod", App: "web"}
	tests := []struct {
		name    string
		start   string
		phases  []string
		timeout time.Duration
		out     string
		err     string
	}{
		{
			name: "succeeds", start: "pendingDelivery", phases: []string{"applying", "applying", "succeeded"},
			timeout: time.Minute, out: "shop/prod/web: applying\nshop/prod/web: succeeded\nshop/prod/web: deployed\n",
		},
		{
			name: "already done", start: "succeeded", timeout: time.Minute, out: "shop/prod/web: deployed\n",
		},
		{
			name: "ends otherwise", start: "applying", phases: []string{"failed", "superseded"}, timeout: time.Minute,
			out: "shop/prod/web: failed\nshop/prod/web: superseded\n",
			err: "shop/prod/web: the deployment ended superseded; kuben status shop/prod/web says why",
		},
		{
			name: "times out", start: "applying", phases: []string{"applying", "applying", "applying"}, timeout: 3 * time.Second,
			err: "shop/prod/web: still applying after 3s; it goes on on the server",
		},
		{
			name: "unknown phases are waited out", start: "somethingNew", phases: []string{"succeeded"}, timeout: time.Minute,
			out: "shop/prod/web: succeeded\nshop/prod/web: deployed\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phases := tt.phases
			poll := func(context.Context) (string, error) {
				if len(phases) == 0 {
					return "", errors.New("polled too often")
				}
				next := phases[0]
				phases = phases[1:]
				return next, nil
			}
			var out bytes.Buffer
			err := waitFor(t.Context(), &out, app, tt.start, tt.timeout, poll, &fakePacer{at: time.UnixMilli(0)})
			if got := errString(err); got != tt.err {
				t.Errorf("err %q", got)
			}
			if out.String() != tt.out {
				t.Errorf("out %q", out.String())
			}
		})
	}
	if got := durationSeconds(1 << 63); got <= 0 {
		t.Errorf("seconds overflowed: %s", got)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

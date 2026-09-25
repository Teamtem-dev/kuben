package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
)

func TestAnAppsObjectsAreToldApartFromItsNeighbours(t *testing.T) {
	workloads := []string{"web-web", "web-nightly"}
	pods := map[string]struct{}{"web-web-7d9c-x2x9q": {}}
	cases := []struct {
		kind, name string
		want       bool
	}{
		{"App", "web", true},
		{"HTTPRoute", "web", true},
		{"HTTPRoute", "web-api", false}, // another app's route
		{"Deployment", "web-web", true},
		{"Deployment", "web-web-2", false},
		{"ReplicaSet", "web-web-7d9c5f", true},
		{"Pod", "web-web-7d9c-x2x9q", true},
		{"Pod", "web-web-7d9c5f-abcde", true}, // a pod already gone
		{"CronJob", "web-nightly", true},
		{"Job", "web-nightly-29310240", true},
		{"Job", "web-nightly-run-1757548800", true},
		{"Pod", "web-nightly-run-1757548800-k2j4d", true},
		{"Pod", "api-web-7d9c5f-abcde", false},
		{"Pod", "web-web-a-b-c-d", false},
		{"Service", "api", false},
	}
	for _, c := range cases {
		if got := api.Belongs(c.kind, c.name, "web", workloads, pods); got != c.want {
			t.Errorf("belongs(%s %s) = %v, want %v", c.kind, c.name, got, c.want)
		}
	}
}

func TestAFollowedLogLineKeepsItsTimeAndIsCutOnACharacter(t *testing.T) {
	timeStr, line := api.SplitLine("2026-09-16T10:00:00.123456789Z hello world")
	if timeStr != opt.Some("2026-09-16T10:00:00.123456789Z") || line != "hello world" {
		t.Errorf("split = %v %q", timeStr, line)
	}
	timeStr, line = api.SplitLine("no time here")
	if timeStr.IsSome() || line != "no time here" {
		t.Errorf("split = %v %q", timeStr, line)
	}
	_, cut := api.SplitLine("2026-09-16T10:00:00Z " + strings.Repeat("é", api.MaxLine))
	if len(cut) > api.MaxLine || strings.Trim(cut, "é") != "" {
		t.Errorf("cut to %d bytes, not on a character", len(cut))
	}
}

func TestFollowedLogsAreCappedPerUserAndFreedWhenClosed(t *testing.T) {
	streams := api.NewLogStreams()
	alice, bob := ids.New[ids.User](), ids.New[ids.User]()
	held := make([]*api.LogStreamPermit, 0, api.MaxFollowsPerUser)
	for range api.MaxFollowsPerUser {
		permit, err := streams.Acquire(alice)
		if err != nil {
			t.Fatalf("a place: %v", err)
		}
		held = append(held, permit)
	}
	_, err := streams.Acquire(alice)
	var kerrErr *kerr.Error
	if !errors.As(err, &kerrErr) || kerrErr.Code != kerr.RateLimited {
		t.Fatalf("a fifth follow: %v", err)
	}
	other, err := streams.Acquire(bob)
	if err != nil {
		t.Fatalf("others are not affected: %v", err)
	}
	for _, p := range held {
		p.Release()
	}
	other.Release()
	for _, u := range []ids.UserID{alice, bob} {
		if n, ok := streams.OpenFor(u); ok {
			t.Errorf("a user with no follow left keeps an entry (%d)", n)
		}
	}
	permit, err := streams.Acquire(alice)
	if err != nil {
		t.Fatalf("free again: %v", err)
	}
	permit.Release()
}

func TestLogTextIsSplitAsRustLines(t *testing.T) {
	long := strings.Repeat("x", 70*1024) // past bufio.Scanner's 64 KiB
	cases := []struct {
		text string
		want []string
	}{
		{"", []string{}},
		{"a", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\r\nb\r\n", []string{"a", "b"}},
		{"a\n\nb\n", []string{"a", "", "b"}},
		{"\n", []string{""}},
		{"a\r\r\nb\r", []string{"a\r", "b\r"}},
		{long + "\nlast", []string{long, "last"}},
	}
	for _, c := range cases {
		if diff := cmp.Diff(c.want, api.TextLines(c.text)); diff != "" {
			t.Errorf("lines(%q) (-want +got):\n%s", c.text, diff)
		}
	}
}

func TestInvalidUTF8IsDescribedAsRustDoes(t *testing.T) {
	cases := []struct {
		text []byte
		want string
	}{
		{[]byte{0, 159, 146, 150}, "invalid utf-8 sequence of 1 bytes from index 1"},
		{[]byte("ok\xff"), "invalid utf-8 sequence of 1 bytes from index 2"},
		{[]byte("é")[:1], "incomplete utf-8 byte sequence from index 0"},
		{[]byte{'a', 0xF0, 0x9F}, "incomplete utf-8 byte sequence from index 1"},
		{[]byte{0xE2, 0x82, 'x'}, "invalid utf-8 sequence of 2 bytes from index 0"},
		{[]byte{0xF0, 0x9F, 0x98, 'x'}, "invalid utf-8 sequence of 3 bytes from index 0"},
		{[]byte{0xE0, 0x80, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"}, // overlong
		{[]byte{0xED, 0xA0, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"}, // surrogate
		{[]byte{0xC0, 0x80}, "invalid utf-8 sequence of 1 bytes from index 0"},
	}
	for _, c := range cases {
		if got := api.UTF8ErrorText(c.text); got != c.want {
			t.Errorf("%x: %q, want %q", c.text, got, c.want)
		}
	}
}

func TestLogLinesAndEndsWriteEveryMember(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{api.LogLine{Pod: "web-1", Line: "hi"}, `{"pod":"web-1","process":null,"time":null,"line":"hi"}`},
		{
			api.LogLine{Pod: "web-1", Process: opt.Some("web"), Time: opt.Some("2026-09-16T10:00:00Z"), Line: "hi"},
			`{"pod":"web-1","process":"web","time":"2026-09-16T10:00:00Z","line":"hi"}`,
		},
		{api.LogEnd{}, `{"pod":null,"error":null}`},
		{api.LogEnd{Pod: opt.Some("web-1"), Error: opt.Some("gone")}, `{"pod":"web-1","error":"gone"}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.value)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestEventsAreFramedAsAxumWritesThem(t *testing.T) {
	line := api.SSEEvent("line", api.LogLine{Pod: "web-1", Line: "a<b>&\u2028\"\\u2028\n"})
	want := "event: line\ndata: {\"pod\":\"web-1\",\"process\":null,\"time\":null,\"line\":\"a<b>&\u2028\\\"\\\\u2028\\n\"}\n\n"
	if string(line) != want {
		t.Errorf("line event:\n%q\nwant\n%q", line, want)
	}
	end := api.SSEEvent("end", api.LogEnd{Pod: opt.Some("web-1")})
	if string(end) != "event: end\ndata: {\"pod\":\"web-1\",\"error\":null}\n\n" {
		t.Errorf("end event: %q", end)
	}
	if api.SSEPing != ": ping\n\n" {
		t.Errorf("keep-alive: %q", api.SSEPing)
	}
}

// eventRow is the parts of an AppEvent the tests compare ("null" for
// absent).
type eventRow struct {
	Kind, Name, Type, Reason, Message, FirstSeen, LastSeen, Source string
	Count                                                          int32
}

func nullable(v gen.OptNilString) string {
	if s, ok := v.Get(); ok {
		return s
	}
	if v.IsNull() {
		return "null"
	}
	return "unset"
}

func rowOf(e gen.AppEvent) eventRow {
	return eventRow{
		Kind: e.Kind, Name: e.Name, Type: e.Type, Reason: nullable(e.Reason), Message: nullable(e.Message),
		FirstSeen: nullable(e.FirstSeen), LastSeen: nullable(e.LastSeen), Source: nullable(e.Source), Count: e.Count,
	}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAKubernetesEventBecomesAnAppEvent(t *testing.T) {
	obj := corev1.ObjectReference{Kind: "Pod", Name: "web-1"}
	cases := []struct {
		name  string
		event corev1.Event
		want  eventRow
	}{{
		name: "the series is newest and keeps its fraction",
		event: corev1.Event{
			InvolvedObject: obj, Type: "Warning", Reason: "BackOff", Message: "restarting",
			Series:         &corev1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(at("2026-09-16T10:00:05.250Z"))},
			LastTimestamp:  metav1.NewTime(at("2026-09-16T10:00:04Z")),
			FirstTimestamp: metav1.NewTime(at("2026-09-16T09:00:00Z")),
			Count:          7, ReportingController: "kubelet", Source: corev1.EventSource{Component: "old-kubelet"},
		},
		want: eventRow{
			Kind: "Pod", Name: "web-1", Type: "Warning", Reason: "BackOff", Message: "restarting",
			FirstSeen: "2026-09-16T09:00:00Z", LastSeen: "2026-09-16T10:00:05.25Z", Source: "kubelet", Count: 3,
		},
	}, {
		name: "a series without a count or time falls back to the event's",
		event: corev1.Event{
			InvolvedObject: obj, Series: &corev1.EventSeries{},
			LastTimestamp: metav1.NewTime(at("2026-09-16T10:00:04Z")), Count: 7,
			Source: corev1.EventSource{Component: "scheduler"},
		},
		want: eventRow{
			Kind: "Pod", Name: "web-1", Type: "Normal", Reason: "null", Message: "null",
			FirstSeen: "2026-09-16T10:00:04Z", LastSeen: "2026-09-16T10:00:04Z", Source: "scheduler", Count: 7,
		},
	}, {
		name: "the event time, then first_seen from last_seen; a negative count is 1",
		event: corev1.Event{
			InvolvedObject: obj, EventTime: metav1.NewMicroTime(at("2026-09-16T10:00:00.000001Z")),
			Series: &corev1.EventSeries{Count: -2},
		},
		want: eventRow{
			Kind: "Pod", Name: "web-1", Type: "Normal", Reason: "null", Message: "null",
			FirstSeen: "2026-09-16T10:00:00.000001Z", LastSeen: "2026-09-16T10:00:00.000001Z", Source: "null", Count: 1,
		},
	}, {
		name: "the creation time last",
		event: corev1.Event{
			InvolvedObject: obj,
			ObjectMeta:     metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(at("2026-09-16T08:00:00Z"))},
		},
		want: eventRow{
			Kind: "Pod", Name: "web-1", Type: "Normal", Reason: "null", Message: "null",
			FirstSeen: "2026-09-16T08:00:00Z", LastSeen: "2026-09-16T08:00:00Z", Source: "null", Count: 1,
		},
	}, {
		name:  "nothing known",
		event: corev1.Event{},
		want: eventRow{
			Type: "Normal", Reason: "null", Message: "null", FirstSeen: "null", LastSeen: "null", Source: "null", Count: 1,
		},
	}}
	for _, c := range cases {
		if diff := cmp.Diff(c.want, rowOf(api.AppEventFrom(&c.event))); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.name, diff)
		}
	}
}

func TestEventsAreNewestFirstNeverSeenLastAndTiesKeepTheirOrder(t *testing.T) {
	event := func(name string, lastSeen opt.Val[string]) gen.AppEvent {
		e := gen.AppEvent{Name: name}
		if s, ok := lastSeen.Get(); ok {
			e.LastSeen = gen.NewOptNilString(s)
		} else {
			e.LastSeen.SetToNull()
		}
		return e
	}
	events := []gen.AppEvent{
		event("a", opt.Some("2026-09-16T10:00:01Z")),
		event("b", opt.None[string]()),
		event("c", opt.Some("2026-09-16T10:00:02Z")),
		event("d", opt.Some("2026-09-16T10:00:01Z")),
		event("e", opt.None[string]()),
		event("f", opt.Some("2026-09-16T10:00:02.5Z")), // compared as text, as Rust did
		event("g", opt.Some("2026-09-16T10:00:01Z")),
	}
	api.SortEvents(events)
	got := make([]string, 0, len(events))
	for _, e := range events {
		got = append(got, e.Name)
	}
	if diff := cmp.Diff([]string{"c", "f", "a", "d", "g", "b", "e"}, got); diff != "" {
		t.Errorf("order (-want +got):\n%s", diff)
	}
}

func TestAnEmptyProcessIsAFilterToo(t *testing.T) {
	p := projection.New()
	pod := func(name string, process opt.Val[string]) {
		p.UpsertPod(projection.PodView{
			Key: "ns/" + name, Namespace: "ns", Name: name, App: opt.Some("web"), Process: process,
		})
	}
	pod("web-1", opt.Some("web"))
	pod("web-2", opt.Some("worker"))
	pod("web-3", opt.None[string]())
	pod("web-4", opt.Some(""))
	names := func(process opt.Val[string]) []string {
		out := []string{}
		for _, v := range api.PodsOf(p, "ns", "web", process) {
			out = append(out, v.Name)
		}
		return out
	}
	cases := []struct {
		process opt.Val[string]
		want    []string
	}{
		{opt.None[string](), []string{"web-1", "web-2", "web-3", "web-4"}},
		{opt.Some("web"), []string{"web-1"}},
		{opt.Some(""), []string{"web-4"}},
		{opt.Some("nothing"), []string{}},
	}
	for _, c := range cases {
		if diff := cmp.Diff(c.want, names(c.process)); diff != "" {
			t.Errorf("process %v (-want +got):\n%s", c.process, diff)
		}
	}
	for i := range 12 {
		pod("many-"+string(rune('a'+i)), opt.Some("many"))
	}
	if n := len(api.PodsOf(p, "ns", "web", opt.Some("many"))); n != 10 {
		t.Errorf("%d pods read at once, want 10", n)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestAPodsLinesAreReadAsFuturesLinesReadsThem(t *testing.T) {
	line := func(t opt.Val[string], text string) api.LogPiece {
		return api.LinePiece(api.LogLine{Pod: "web-1", Process: opt.Some("web"), Time: t, Line: text})
	}
	none := opt.None[string]()
	cases := []struct {
		name string
		body io.Reader
		want []api.LogPiece
		end  opt.Val[string]
	}{{
		name: "lines, CRLF, an empty line and a last one without newline",
		body: strings.NewReader("2026-09-16T10:00:00Z one\r\ntwo\r\r\n\nthree\r"),
		want: []api.LogPiece{
			line(opt.Some("2026-09-16T10:00:00Z"), "one"), line(none, "two\r"), line(none, ""), line(none, "three\r"),
		},
		end: none,
	}, {
		name: "a line that is not UTF-8 ends the read",
		body: strings.NewReader("ok\nbad \xff\nlater\n"),
		want: []api.LogPiece{line(none, "ok")},
		end:  opt.Some("stream did not contain valid UTF-8"),
	}, {
		name: "a read error drops the partial line and ends the read",
		body: io.MultiReader(strings.NewReader("a\npartial"), failingReader{}),
		want: []api.LogPiece{line(none, "a")},
		end:  opt.Some("connection reset"),
	}}
	for _, c := range cases {
		out := make(chan api.LogPiece, 16)
		end, ok := api.ReadLines(t.Context(), c.body, "web-1", opt.Some("web"), out)
		close(out)
		got := []api.LogPiece{}
		for p := range out {
			got = append(got, p)
		}
		if !ok || end != c.end {
			t.Errorf("%s: end %v (delivered %v), want %v", c.name, end, ok, c.end)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %d lines, want %d: %v", c.name, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: line %d = %v, want %v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// follow runs a followed log's event loop until stop is called and returns
// what it wrote.
func follow(
	t *testing.T, c clock.Clock, limit, rescan time.Duration, pods func() []*projection.PodView,
	read func(*projection.PodView, *corev1.PodLogOptions), pieces <-chan api.LogPiece,
) (stop func() string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- api.RunFollow(ctx, &buf, c, limit, rescan, time.Hour, 200, pods, read, pieces)
	}()
	return func() string {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("follow: %v", err)
		}
		return buf.String()
	}
}

func noPods() []*projection.PodView { return nil }

func noRead(*projection.PodView, *corev1.PodLogOptions) {}

func TestAFollowedLogEndsAtItsLimitWithoutAPod(t *testing.T) {
	var buf bytes.Buffer
	err := api.RunFollow(t.Context(), &buf, clock.Fixed(0), time.Millisecond, time.Hour, time.Hour, 200,
		noPods, noRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "event: end\ndata: {\"pod\":null,\"error\":\"" + api.LimitMessage + "\"}\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestTheSameEndOfAPodIsToldOnce(t *testing.T) {
	pieces := make(chan api.LogPiece)
	stop := follow(t, clock.Fixed(0), time.Hour, time.Hour, noPods, noRead, pieces)
	pieces <- api.LinePiece(api.LogLine{Pod: "web-1", Line: "hi"})
	pieces <- api.EndPiece("web-1", opt.Some("boom"))
	pieces <- api.EndPiece("web-1", opt.Some("boom"))
	pieces <- api.EndPiece("web-1", opt.None[string]())
	pieces <- api.EndPiece("web-1", opt.None[string]())
	pieces <- api.EndPiece("web-2", opt.Some("boom"))
	want := "event: line\ndata: {\"pod\":\"web-1\",\"process\":null,\"time\":null,\"line\":\"hi\"}\n\n" +
		"event: end\ndata: {\"pod\":\"web-1\",\"error\":\"boom\"}\n\n" +
		"event: end\ndata: {\"pod\":\"web-1\",\"error\":null}\n\n" +
		"event: end\ndata: {\"pod\":\"web-2\",\"error\":\"boom\"}\n\n"
	if got := stop(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// steps is a clock that reads each time in turn, then stays on the last.
type steps struct {
	mu  sync.Mutex
	now []int64
}

func (s *steps) NowMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.now[0]
	if len(s.now) > 1 {
		s.now = s.now[1:]
	}
	return v
}

type readCall struct {
	Pod, Container string
	Tail, Since    opt.Val[int64]
}

func TestAPodIsReadAgainFromWhereItsReadEnded(t *testing.T) {
	var mu sync.Mutex
	current := []*projection.PodView{{Name: "web-1", Process: opt.Some("web")}}
	pods := func() []*projection.PodView {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(current)
	}
	reads := make(chan readCall, 8)
	read := func(pod *projection.PodView, opts *corev1.PodLogOptions) {
		if !opts.Follow || !opts.Timestamps || opts.Previous || opts.LimitBytes != nil {
			t.Errorf("options %+v", opts)
		}
		reads <- readCall{pod.Name, opts.Container, opt.FromPtr(opts.TailLines), opt.FromPtr(opts.SinceSeconds)}
	}
	pieces := make(chan api.LogPiece)
	stop := follow(t, &steps{now: []int64{1_000, 43_500}}, time.Hour, time.Millisecond, pods, read, pieces)

	want := []readCall{{Pod: "web-1", Container: "web", Tail: opt.Some(int64(200))}}
	got := []readCall{<-reads}
	pieces <- api.EndPiece("web-1", opt.None[string]())
	want = append(want, readCall{Pod: "web-1", Container: "web", Since: opt.Some(int64(42))})
	got = append(got, <-reads)
	mu.Lock()
	current = append(current, &projection.PodView{Name: "web-2"})
	mu.Unlock()
	want = append(want, readCall{Pod: "web-2"})
	got = append(got, <-reads)
	stop()
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b opt.Val[int64]) bool { return a == b })); diff != "" {
		t.Errorf("reads (-want +got):\n%s", diff)
	}
}

func TestAQuietFollowedLogIsKeptAlive(t *testing.T) {
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- api.RunFollow(ctx, w, clock.Fixed(0), time.Hour, time.Hour, time.Millisecond, 200, noPods, noRead, nil)
	}()
	got := make([]byte, len(api.SSEPing))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != ": ping\n\n" {
		t.Errorf("keep-alive %q", got)
	}
	cancel()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	<-done // ends quietly on cancel, or with the closed pipe
}

func TestAFollowedLogBypassesTheRequestTimeout(t *testing.T) {
	server, err := api.New(api.Deps{Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	const logs = "/api/v1/projects/p/environments/e/apps/a/logs"
	cases := []struct {
		method, target string
		want           bool
	}{
		{"GET", logs + "?follow=true", true},
		{"GET", logs + "?follow=1", true},
		{"GET", logs + "?follow=t", true},
		{"GET", logs + "?follow=True", true},
		{"GET", logs + "?tail=5&follow=TRUE", true},
		{"GET", logs, false},
		{"GET", logs + "?follow=false", false},
		{"GET", logs + "?follow=0", false},
		{"GET", logs + "?follow=", false},
		{"GET", logs + "?follow=yes", false},
		{"GET", logs + "?follow=1&follow=1", false}, // the decoder refuses two values
		{"POST", logs + "?follow=1", false},
		{"GET", "/api/v1/projects/p/environments/e/apps/a/events?follow=1", false},
		{"GET", "/api/v1/something/logs?follow=true", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.target, nil)
		if got := api.FollowsLogs(server, r); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.target, got, c.want)
		}
	}
}

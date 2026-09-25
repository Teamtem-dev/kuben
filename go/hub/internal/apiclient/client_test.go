package apiclient_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/apiclient"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

func some(s string) opt.Val[string] { return opt.Some(s) }

func none() opt.Val[string] { return opt.None[string]() }

func TestAppPathsTakeDefaultsAndRefuseOddNames(t *testing.T) {
	full, err := apiclient.ParseAppPath("shop/prod/web", none(), none())
	if err != nil {
		t.Fatal(err)
	}
	if got := full.API(); got != "/projects/shop/environments/prod/apps/web" {
		t.Errorf("api %s", got)
	}
	if got := full.String(); got != "shop/prod/web" {
		t.Errorf("display %s", got)
	}
	defaults, err := apiclient.ParseAppPath("web", some("shop"), some("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(apiclient.AppPath{Project: "shop", Environment: "dev", App: "web"}, defaults); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	given, err := apiclient.ParseAppPath("live/web", some("shop"), some("dev"))
	if err != nil || given.Environment != "live" {
		t.Errorf("environment given: %v %v", given, err)
	}
	for _, refused := range []struct {
		text                 string
		project, environment opt.Val[string]
	}{
		{"web", none(), some("dev")}, // no project
		{"shop/prod/../x", none(), none()},
		{"shop/prod/Web", none(), none()},
		{"a/b/c/d", none(), none()},
	} {
		if _, err := apiclient.ParseAppPath(refused.text, refused.project, refused.environment); err == nil {
			t.Errorf("%s accepted", refused.text)
		}
	}
}

func TestTheTokenNeverShowsInDebugOutput(t *testing.T) {
	c := apiclient.New("https://kuben.example.com/", "kbn_secret")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		shown := fmt.Sprintf(verb, c)
		if !strings.Contains(shown, "https://kuben.example.com/api/v1") || strings.Contains(shown, "kbn_secret") {
			t.Errorf("%s: %s", verb, shown)
		}
	}
}

func TestLogQueriesCarryOnlyWhatIsAsked(t *testing.T) {
	if got := (apiclient.LogOptions{}).Query(false); got != "" {
		t.Errorf("default: %q", got)
	}
	options := apiclient.LogOptions{Tail: opt.Some[int64](20), Process: some("worker"), Previous: true}
	if got := options.Query(true); got != "?tail=20&process=worker&previous=true&follow=true" {
		t.Errorf("all: %q", got)
	}
	odd := apiclient.LogOptions{Process: some("a&b")}
	if got := odd.Query(false); got != "" {
		t.Errorf("a name that is no slug is left out: %q", got)
	}
}

func TestServerSentEventsAreSplitAcrossChunks(t *testing.T) {
	var parser apiclient.SSEParser
	if got := parser.Push("event: line\ndata: {\"a\""); len(got) != 0 {
		t.Errorf("incomplete: %v", got)
	}
	got := parser.Push(":1}\n\n: ping\n\nevent: end\r\ndata: {}\r\n\r\ndata: x\n")
	want := []apiclient.SSEEvent{{Event: "line", Data: `{"a":1}`}, {Event: "end", Data: "{}"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]apiclient.SSEEvent{{Event: "message", Data: "x"}}, parser.Push("\n")); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// serve runs handler on httptest's in-memory network and returns a client
// of it.
func serve(t *testing.T, handler http.HandlerFunc) apiclient.Client {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	return apiclient.NewWith(srv.URL+"/", "kbn_x", srv.Client().Transport)
}

func TestProblemsBecomeAPIErrors(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       apiclient.APIError
		shown      string
	}{
		{
			name:   "problem with detail",
			body:   `{"code":"not_found","title":"Not Found","status":404,"detail":"no app web"}`,
			status: http.StatusNotFound,
			want:   apiclient.APIError{Status: 404, Code: "not_found", Detail: some("no app web")},
			shown:  "no app web (not_found)",
		},
		{
			name:   "problem without detail",
			body:   `{"code":"forbidden","title":"Forbidden","status":403}`,
			status: http.StatusForbidden,
			want:   apiclient.APIError{Status: 403, Code: "forbidden"},
			shown:  "403 (forbidden)",
		},
		{
			name:   "no problem document",
			body:   `<html>bad gateway</html>`,
			status: http.StatusBadGateway,
			want:   apiclient.APIError{Status: 502, Code: "502"},
			shown:  "502 (502)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/projects" || r.Header.Get("Authorization") != "Bearer kbn_x" {
					t.Errorf("request %s %s", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body) //nolint:errcheck // test server
			})
			_, err := c.Projects(t.Context())
			var got apiclient.APIError
			if !errors.As(err, &got) {
				t.Fatalf("err = %v", err)
			}
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(opt.Val[string]{})); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
			if got.Error() != tt.shown {
				t.Errorf("shown %q", got.Error())
			}
		})
	}
}

func TestFollowedLogsArriveAsEvents(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "tail=5&follow=true" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: line\ndata: {\"pod\":\"web-1\",\"process\":\"web\",\"time\":null,\"line\":\"h\xc3") //nolint:errcheck // test server
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "\xa9llo\"}\n\n: ping\n\nevent: other\ndata: 1\n\nevent: end\ndata: {\"pod\":null,\"error\":null}\n\n") //nolint:errcheck // test server
	})
	app := apiclient.AppPath{Project: "shop", Environment: "prod", App: "web"}
	events, err := c.FollowLogs(t.Context(), app, apiclient.LogOptions{Tail: opt.Some[int64](5)})
	if err != nil {
		t.Fatal(err)
	}
	var got []apiclient.FollowEvent
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}
	want := []apiclient.FollowEvent{
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "web-1", Process: some("web"), Line: "héllo"}},
		apiclient.EndEvent{},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(opt.Val[string]{})); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

package apiclient_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/apiclient"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

const buildsPath = "/api/v1/projects/shop/environments/prod/apps/web/builds"

var web = apiclient.AppPath{Project: "shop", Environment: "prod", App: "web"}

func TestBuildsAreListedTriggeredAndCancelled(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET " + buildsPath + "?limit=5":
			_, _ = io.WriteString(w, `[{"id":"b1","attempt":1,"repository":"acme/web","branch":"main",`+ //nolint:errcheck // test server
				`"commit":"0123456789abcdef","strategy":"auto","phase":"running","blockedReason":null,"failure":null,`+
				`"failureDetail":null,"image":null,"release":null,"deployment":null,"deployDecision":null,`+
				`"cancelRequested":false,"createdAt":1,"startedAt":2,"finishedAt":null,`+
				`"stages":[{"name":"clone","status":"succeeded","startedAt":2,"finishedAt":3,"detail":null}]}]`)
		case "POST " + buildsPath:
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"syncOperation":"op1","build":null}`) //nolint:errcheck // test server
		case "POST " + buildsPath + "/b1/cancel":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"id":"b1","phase":"cancelRequested","cancelRequested":true}`) //nolint:errcheck // test server
		case "GET " + buildsPath + "/b1/logs":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "==> clone <==\ncloned\n") //nolint:errcheck // test server
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	builds, err := c.Builds(t.Context(), web, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || builds[0].StartedAt != opt.Some[int64](2) || len(builds[0].Stages) != 1 ||
		builds[0].Stages[0].Status != "succeeded" {
		t.Fatalf("%+v", builds)
	}
	triggered, err := c.TriggerBuild(t.Context(), web)
	if err != nil || triggered.SyncOperation != "op1" || triggered.Build.IsSome() {
		t.Fatalf("%+v %v", triggered, err)
	}
	cancelled, err := c.CancelBuild(t.Context(), web, "b1")
	if err != nil || cancelled.Phase != "cancelRequested" {
		t.Fatalf("%+v %v", cancelled, err)
	}
	text, err := c.BuildLog(t.Context(), web, "b1")
	if err != nil || text != "==> clone <==\ncloned\n" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestFollowedBuildLogsArriveAsEvents(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != buildsPath+"/b1/logs?follow=true" {
			t.Errorf("request %s", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: line\ndata: {\"pod\":\"kbuild-1\",\"process\":\"build\",\"time\":null,\"line\":\"#1 done\"}\n\n"+ //nolint:errcheck // test server
			"event: end\ndata: {\"pod\":\"kbuild-1\",\"error\":null}\n\n")
	})
	events, err := c.FollowBuildLog(t.Context(), web, "b1")
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
		apiclient.LineEvent{LogLine: apiclient.LogLine{Pod: "kbuild-1", Process: some("build"), Line: "#1 done"}},
		apiclient.EndEvent{LogEnd: apiclient.LogEnd{Pod: some("kbuild-1")}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(opt.Val[string]{})); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

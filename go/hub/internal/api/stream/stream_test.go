package stream_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/stream"
)

type fake struct {
	snap   stream.Snapshot
	deltas chan stream.Delta
}

func (f fake) Snapshot([]string) stream.Snapshot                       { return f.snap }
func (f fake) Subscribe(context.Context, []string) <-chan stream.Delta { return f.deltas }

func TestSnapshotThenDeltas(t *testing.T) {
	f := fake{snap: stream.Empty{}.Snapshot(nil), deltas: make(chan stream.Delta, 2)}
	f.snap.Seq = 7
	f.deltas <- stream.Delta{Name: "delta", Seq: 8, Data: []byte(`{"kind":"pod_delete","seq":8,"key":"x"}`)}
	close(f.deltas)
	rec := httptest.NewRecorder()
	if err := stream.Serve(rec, httptest.NewRequest("GET", "/api/v1/stream", nil), f, []string{"org"}); err != nil {
		t.Fatal(err)
	}
	want := "event: snapshot\nid: 7\ndata: {\"seq\":7,\"pods\":[],\"projects\":[],\"environments\":[],\"apps\":[]}\n\n" +
		"event: delta\nid: 8\ndata: {\"kind\":\"pod_delete\",\"seq\":8,\"key\":\"x\"}\n\n"
	if rec.Body.String() != want || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %q", rec.Body.String())
	}
}

func TestACurrentClientGetsNoSnapshot(t *testing.T) {
	f := fake{snap: stream.Empty{}.Snapshot(nil), deltas: make(chan stream.Delta)}
	close(f.deltas)
	r := httptest.NewRequest("GET", "/api/v1/stream", nil)
	r.Header.Set("Last-Event-ID", "0")
	rec := httptest.NewRecorder()
	if err := stream.Serve(rec, r, f, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "snapshot") {
		t.Fatalf("got %q", rec.Body.String())
	}
}

func TestEmptySourceEndsWithTheRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", "/api/v1/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	if err := stream.Serve(rec, r, stream.Empty{}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Body.String(), "event: snapshot\nid: 0\n") {
		t.Fatalf("got %q", rec.Body.String())
	}
}

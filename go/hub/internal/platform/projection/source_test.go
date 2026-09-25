package projection_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/stream"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The tests of crates/kuben-api/src/stream.rs (Visibility), and the
// stream source built on it.

func project(name, org string) projection.ProjectView {
	return projection.ProjectView{Name: name, DisplayName: name, Org: opt.Some(org), Ready: true}
}

func orgPod(name, org string) *projection.PodView {
	return &projection.PodView{
		Key: "ns/" + name, Namespace: "ns", Name: name, Org: opt.Some(org),
		Phase: projection.PodRunning, Ready: true,
	}
}

func TestSnapshotAndDeltasAreTenantFiltered(t *testing.T) {
	p := projection.New()
	p.UpsertProject(project("mine", "a"))
	p.UpsertProject(project("theirs", "b"))
	v := projection.NewVisibility([]string{"a"})
	snap := v.Snapshot(p.Snapshot())
	if len(snap.Projects) != 1 || snap.Projects[0].Name != "mine" {
		t.Fatalf("projects: %+v", snap.Projects)
	}
	steps := []struct {
		delta projection.Delta
		want  bool
		why   string
	}{
		{projection.PodUpsert{Seq: 9, Pod: orgPod("x", "b")}, false, "another org"},
		{projection.PodDelete{Seq: 10, Key: "ns/x"}, false, "unseen deletes are dropped"},
		{projection.PodUpsert{Seq: 11, Pod: orgPod("y", "a")}, true, "own org"},
		{projection.PodDelete{Seq: 12, Key: "ns/y"}, true, "a seen delete"},
		{projection.ProjectDelete{Seq: 13, Key: "mine"}, true, "seen in the snapshot"},
		{projection.ProjectDelete{Seq: 14, Key: "theirs"}, false, "filtered from the snapshot"},
		{projection.Resync{Seq: 15}, true, "everyone resyncs"},
	}
	for _, s := range steps {
		if got := v.Admit(s.delta); got != s.want {
			t.Errorf("%s: admit = %v", s.why, got)
		}
	}
}

func TestAnExposureChangeReachesOnlyThoseWhoSeeTheApp(t *testing.T) {
	a := &v1alpha1.App{}
	a.Name, a.Namespace = "api", "kb-shop-prod"
	a.Labels = map[string]string{v1alpha1.LabelOrg: "a"}
	if err := json.Unmarshal([]byte(`{
		"source": { "image": "nginx" },
		"runtime": { "processes": { "web": { "port": 80 } } }
	}`), &a.Spec); err != nil {
		t.Fatal(err)
	}
	view := projection.AppViewOf(a)
	exposure := func(key string) projection.Delta { return projection.ExposureChanged{Seq: 2, Key: key} }
	mine := projection.NewVisibility([]string{"a"})
	theirs := projection.NewVisibility([]string{"b"})
	upsert := projection.AppUpsert{Seq: 1, App: &view}
	if !mine.Admit(upsert) || theirs.Admit(upsert) {
		t.Fatal("upsert")
	}
	if !mine.Admit(exposure("kb-shop-prod/api")) || theirs.Admit(exposure("kb-shop-prod/api")) {
		t.Fatal("exposure")
	}
	if mine.Admit(exposure("kb-other/api")) {
		t.Fatal("an app it never saw")
	}
}

func recv(t *testing.T, ch <-chan stream.Delta) stream.Delta {
	t.Helper()
	select {
	case d, ok := <-ch:
		if !ok {
			t.Fatal("closed")
		}
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no delta")
	}
	return stream.Delta{}
}

func TestTheSourceStreamsWhatTheCallerMaySee(t *testing.T) {
	p := projection.New()
	p.UpsertProject(project("mine", "a"))
	p.UpsertProject(project("theirs", "b"))
	var src stream.Source = projection.NewSource(p, nil)

	ctx, cancel := context.WithCancel(context.Background())
	deltas := src.Subscribe(ctx, []string{"a"})
	snap := src.Snapshot([]string{"a"})
	if snap.Seq != 2 || len(snap.Projects) != 1 || len(snap.Pods) != 0 || snap.Apps == nil {
		t.Fatalf("snapshot: %+v", snap)
	}
	var mine projection.ProjectView
	if err := json.Unmarshal(snap.Projects[0], &struct {
		Name *string `json:"name"`
	}{&mine.Name}); err != nil || mine.Name != "mine" {
		t.Fatalf("project: %s", snap.Projects[0])
	}

	p.UpsertPod(*orgPod("x", "b"))
	p.RemoveProject("theirs")
	p.RemoveProject("mine")
	p.ReplaceApps(nil)
	d := recv(t, deltas)
	if d.Name != "delta" || d.Seq != 5 || string(d.Data) != `{"kind":"project_delete","seq":5,"key":"mine"}` {
		t.Fatalf("got %+v %s", d, d.Data)
	}
	d = recv(t, deltas)
	if d.Name != "resync" || d.Seq != 6 || string(d.Data) != `{"kind":"resync","seq":6}` {
		t.Fatalf("got %+v %s", d, d.Data)
	}
	cancel()
	for range deltas { //nolint:revive // drain until the source closes the channel
	}
}

func TestASlowConnectionGetsAResyncWithoutAnID(t *testing.T) {
	p := projection.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deltas := projection.NewSource(p, nil).Subscribe(ctx, []string{"a"})
	// Nobody reads: DeltaCapacity deltas fill the backlog, the rest are
	// missed.
	for range projection.DeltaCapacity + 5 {
		p.UpsertProject(project("mine", "a"))
		p.RemoveProject("mine")
	}
	for {
		d := recv(t, deltas)
		if d.Name == "resync" {
			if d.Seq != 0 || len(d.Data) == 0 {
				t.Fatalf("a lag notice carries the count, not a sequence: %+v", d)
			}
			return
		}
	}
}

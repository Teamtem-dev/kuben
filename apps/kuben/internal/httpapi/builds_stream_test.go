package httpapi_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// feedClock is a clock at 100 s.
type feedClock struct{ clock.System }

func (feedClock) NowMs() int64 { return 100_000 }

func TestChangedBuildsArePublishedOnceToTheirOrganization(t *testing.T) {
	p := projection.New()
	s, err := httpapi.New(httpapi.Deps{Projections: p, Clock: feedClock{}})
	if err != nil {
		t.Fatal(err)
	}
	acme, other := ids.New[ids.Org](), ids.New[ids.Org]()
	attempt := store.BuildAttempt{ID: ids.New[ids.BuildAttempt](), Phase: build.Running}
	change := store.BuildChange{
		Attempt: attempt, UpdatedAt: 99_000, Namespace: "kb-shop-prod",
		ProjectSlug: "shop", EnvironmentSlug: "prod", AppSlug: "web",
	}
	builds := httpapi.FixedBuilds{acme: {change}, other: {}}
	feed := httpapi.NewBuildFeed(builds, 100_000)

	if err := feed.Look(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if p.Seq() != 0 {
		t.Fatal("published without a stream")
	}
	sub := p.Subscribe()
	defer sub.Close()
	for range 2 {
		// A change read twice is published once.
		if err := feed.Look(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := sub.TryRecv()
	if !ok {
		t.Fatal("no delta")
	}
	if _, more := sub.TryRecv(); more {
		t.Fatal("the same change twice")
	}
	if !projection.NewVisibility([]string{acme.String()}).Admit(got.Delta) ||
		projection.NewVisibility([]string{other.String()}).Admit(got.Delta) {
		t.Fatal("only the build's organization hears it")
	}
	data, err := got.Delta.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var delta map[string]any
	if err := json.Unmarshal(data, &delta); err != nil {
		t.Fatal(err)
	}
	b, _ := delta["build"].(map[string]any)
	want := map[string]any{"kind": "build", "app": "kb-shop-prod/web", "project": "shop", "environment": "prod", "name": "web"}
	for k, v := range want {
		if diff := cmp.Diff(v, delta[k]); diff != "" {
			t.Errorf("%s: %s", k, diff)
		}
	}
	if b["id"] != attempt.ID.String() || b["phase"] != "running" {
		t.Errorf("the build: %v", b)
	}

	attempt.Phase = build.Succeeded
	change.Attempt, change.UpdatedAt = attempt, 101_000
	builds[acme] = []store.BuildChange{change}
	if err := feed.Look(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, ok := sub.TryRecv(); !ok {
		t.Fatal("the new phase is published")
	}
	if feed.Since() != 99_000 {
		t.Errorf("the next look starts at %d", feed.Since())
	}
}

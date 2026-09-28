package store_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// The builds that changed are read with the app they build, oldest change
// first, each change of their stages included.
func TestChangedBuildsAreReadWithTheirApp(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "a", 7)
	other := newBuildFixture(t, s, "b", 8)
	syncHead(t, s, f, headA)
	syncHead(t, s, other, headA)
	tn := tenant(t, s, f.org)
	changed := must[[]store.BuildChange](t, "changed")(tn.BuildsChangedSince(ctx, 0, 10))
	if len(changed) != 1 {
		t.Fatalf("the organization's build only: %+v", changed)
	}
	c := changed[0]
	if c.Namespace != "a-shop" || c.ProjectSlug != "shop" || c.EnvironmentSlug != "production" || c.AppSlug != "web" ||
		c.Attempt.Target != f.target || c.Attempt.Commit.String() != headA {
		t.Fatalf("%+v", c)
	}
	if later := must[[]store.BuildChange](t, "later")(tn.BuildsChangedSince(ctx, c.UpdatedAt+1, 10)); len(later) != 0 {
		t.Fatalf("nothing changed since: %+v", later)
	}
	stages := []store.BuildStage{{Name: store.StageClone, Status: store.StageRunning}}
	if ok := must[bool](t, "stages")(tn.SetBuildStages(ctx, c.Attempt.ID, stages)); !ok {
		t.Fatal("no such build")
	}
	again := must[[]store.BuildChange](t, "again")(tn.BuildsChangedSince(ctx, c.UpdatedAt, 10))
	if len(again) != 1 || len(again[0].Attempt.Stages) != 1 || again[0].UpdatedAt < c.UpdatedAt {
		t.Fatalf("a stage is a change: %+v", again)
	}
	rollback(t, tn)
}

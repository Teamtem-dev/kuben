package previews_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/internal/previews"
)

// An unclear answer from GitHub never deletes a preview (previews.rs
// Janitor::verify).
func TestPullStatesNeverDeleteOnAnUnclearAnswer(t *testing.T) {
	cases := []struct {
		name string
		open bool
		err  error
		want string
	}{
		{"open", true, nil, "open"},
		{"closed", false, nil, "closed"},
		{"gone", false, build.NotFound{What: "acme/shop#12"}, "closed"},
		{"gone, wrapped", false, fmt.Errorf("asking: %w", build.NotFound{What: "acme/shop#12"}), "closed"},
		{"refused", false, build.Refused{Reason: "no pull request permission"}, "unknown"},
		{"unavailable", false, build.Unavailable{Reason: "HTTP 502"}, "unknown"},
		{"other", true, errors.New("timeout"), "unknown"},
	}
	for _, c := range cases {
		if got := previews.PullStateOf(c.open, c.err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A preview's app gets no secret references, custom domains or pull
// secrets; a configuration that is not an object is copied as it is.
func TestPreviewConfigurations(t *testing.T) {
	config := map[string]any{
		"env":     []any{map[string]any{"name": "DB", "fromSecret": map[string]any{"name": "db", "key": "url"}}},
		"domains": []any{map[string]any{"host": "shop.example.com"}},
	}
	got, removed := previews.PreviewConfig(config)
	if diff := cmp.Diff(map[string]any{"env": []any{}, "domains": []any{}}, got); diff != "" {
		t.Errorf("config (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"env DB", "domain shop.example.com"}, removed); diff != "" {
		t.Errorf("removed (-want +got):\n%s", diff)
	}
	if got, removed := previews.PreviewConfig(nil); got != nil || len(removed) != 0 {
		t.Errorf("null: %v %v", got, removed)
	}
}

// What a pull request event writes in the audit log (previews.rs audit).
func TestPullRequestAudits(t *testing.T) {
	repo, err := source.ParseRepoName("acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	head, err := source.ParseCommitSha("89abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	a := previews.PullAudit(source.PullEvent{
		Action: source.PullOpened, InstallationID: 77, Repository: repo, Number: 12,
		HeadRepository: "mallory/shop", Head: head, Open: true,
	}, "openPreview", "shop-pr12-1")
	if a.ActorKind != "webhook" || a.ActorID != opt.Some("github:77") || a.Action != "openPreview" ||
		a.TargetKind != opt.Some("environment") || a.TargetRef != opt.Some("shop-pr12-1") || a.Outcome != "accepted" {
		t.Errorf("audit: %+v", a)
	}
	data, _ := a.Data.Get()
	want := map[string]any{
		"repository": "acme/shop", "pullRequest": uint64(12),
		"head": "89abcdef0123456789abcdef0123456789abcdef", "headRepository": "mallory/shop",
	}
	if diff := cmp.Diff(any(want), data); diff != "" {
		t.Errorf("data (-want +got):\n%s", diff)
	}
}

package store_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// repo/previews.rs tests.

const (
	previewHead = "0123456789abcdef0123456789abcdef01234567"
	hourMs      = int64(3_600_000)
)

func repoName(t *testing.T, text string) source.RepoName {
	t.Helper()
	r, err := source.ParseRepoName(text)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return r
}

// previewEnvironment is the environment pr12-1 of a new project `shop`.
func previewEnvironment(t *testing.T, tn *store.Tenant) (ids.ProjectID, ids.EnvironmentID) {
	t.Helper()
	ctx := t.Context()
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(
		tn.CreateEnvironmentTyped(ctx, project, "pr12-1", "PR #12", store.Preview, opt.None[any]()))
	return project, env
}

func TestPreviewsHaveEpochsAndExpire(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	repo := repoName(t, "acme/shop")
	head := commitSha(t, previewHead)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	src := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "staging", "Staging", false))
	if err := tn.SetPreviewPolicy(ctx, store.PreviewPolicy{
		Project: project, Enabled: true, SourceEnvironment: src, TTLHours: 2, MaxActive: 3,
		AllowForks: false, UpdatedBy: "u", UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if p, ok, err := tn.PreviewPolicy(ctx, project); err != nil || !ok || p.TTLHours != 2 {
		t.Fatalf("read: %+v %v %v", p, ok, err)
	}
	if epoch, closed, err := tn.PreviewEpoch(ctx, project, repo, 12); err != nil || epoch != 1 || closed.IsSome() {
		t.Fatalf("epoch: %d %v %v", epoch, closed, err)
	}
	env := must[ids.EnvironmentID](t, "environment")(
		tn.CreateEnvironmentTyped(ctx, project, "pr12-1", "PR #12", store.Preview, opt.None[any]()))
	preview := store.NewPreview{
		Environment: env, Project: project, InstallationID: 7, Repository: repo, Number: 12, Epoch: 1,
		HeadRepository: "acme/shop", Branch: "feature", Commit: head, Trusted: false,
		ExpiresAt: 2 * hourMs, EventAt: 100, CreatedBy: "github:7",
	}
	if err := tn.InsertPreview(ctx, preview); err != nil {
		t.Fatalf("insert: %v", err)
	}
	twice := preview
	twice.Epoch = 2
	if err := tn.InsertPreview(ctx, twice); !store.IsUniqueViolation(err) {
		t.Fatalf("one active preview per pull request: %v", err)
	}
}

func TestPreviewLifecyclesIgnoreStaleEvents(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	repo := repoName(t, "acme/shop")
	head := commitSha(t, previewHead)
	tn := tenant(t, s, o)
	project, env := previewEnvironment(t, tn)
	if err := tn.InsertPreview(ctx, store.NewPreview{
		Environment: env, Project: project, InstallationID: 7, Repository: repo, Number: 12, Epoch: 1,
		HeadRepository: "mallory/shop", Branch: "feature", Commit: head, Trusted: false,
		ExpiresAt: hourMs, EventAt: 100, CreatedBy: "github:7",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if untrusted, err := tn.UntrustedEnvironment(ctx, env); err != nil || !untrusted {
		t.Fatalf("trust: %v %v", untrusted, err)
	}
	if touched, err := tn.TouchPreview(ctx, env, "mallory/shop", "feature", head, 50, 9*hourMs); err != nil || touched {
		t.Fatalf("older: %v %v", touched, err)
	}
	if touched, err := tn.TouchPreview(ctx, env, "mallory/shop", "feature", head, 200, 9*hourMs); err != nil || !touched {
		t.Fatalf("touch: %v %v", touched, err)
	}
	active, ok, err := tn.ActivePreview(ctx, project, repo, 12)
	if err != nil || !ok {
		t.Fatalf("active: %v %v", ok, err)
	}
	if active.ExpiresAt != 9*hourMs || active.LastEventAt != 200 {
		t.Fatalf("touched: %d %d", active.ExpiresAt, active.LastEventAt)
	}
	due := func(now int64) int {
		t.Helper()
		return len(must[[]store.PreviewRecord](t, "due")(tn.DuePreviews(ctx, now, 10)))
	}
	if n := due(8 * hourMs); n != 0 {
		t.Fatalf("not due yet: %d", n)
	}
	if n := due(10 * hourMs); n != 1 {
		t.Fatalf("due: %d", n)
	}
	if extended, err := tn.ExtendPreview(ctx, env, 20*hourMs, false); err != nil || !extended {
		t.Fatalf("extend: %v %v", extended, err)
	}
	if n := due(30 * hourMs); n != 0 {
		t.Fatalf("kept: %d", n)
	}
	if closed, err := tn.ClosePreview(ctx, env, store.CloseClosed, 300); err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	if closed, err := tn.ClosePreview(ctx, env, store.CloseClosed, 300); err != nil || closed {
		t.Fatalf("once: %v %v", closed, err)
	}
	if epoch, closedAt, err := tn.PreviewEpoch(ctx, project, repo, 12); err != nil || epoch != 2 || closedAt != opt.Some[int64](300) {
		t.Fatalf("epoch: %d %v %v", epoch, closedAt, err)
	}
	if n, err := tn.ActivePreviewCount(ctx, project); err != nil || n != 0 {
		t.Fatalf("count: %d %v", n, err)
	}
	all := must[[]store.PreviewRecord](t, "list")(tn.Previews(ctx, project, true))
	if len(all) == 0 || all[0].CloseReason != opt.Some("closed") {
		t.Fatalf("closed: %+v", all)
	}
	if open := must[[]store.PreviewRecord](t, "list")(tn.Previews(ctx, project, false)); len(open) != 0 {
		t.Fatalf("no active preview: %+v", open)
	}
	commit(t, tn)
	if orgs := must[[]ids.OrgID](t, "orgs")(s.PreviewOrgs(ctx)); len(orgs) != 0 {
		t.Fatalf("no organization has an active preview: %v", orgs)
	}
}

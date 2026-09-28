package store_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Git connections, organization registries and build logs (2.1).

const integrationsBy = "user:alice"

func newGitConnection(name string, tag byte) store.NewGitConnection {
	return store.NewGitConnection{
		ID:            ids.New[ids.GitConnection](),
		Provider:      store.GitProviderGitLab,
		Name:          name,
		BaseURL:       "https://gitlab.example.com",
		Token:         sealedTag(tag),
		TokenHint:     "abcd",
		WebhookSecret: sealedTag(tag + 1),
		CreatedBy:     integrationsBy,
	}
}

func storedConnection(t *testing.T, saved store.GitConnectionSaved) store.GitConnection {
	t.Helper()
	s, ok := saved.(store.GitConnectionStored)
	if !ok {
		t.Fatalf("not stored: %#v", saved)
	}
	return s.Connection
}

func TestGitConnectionsAreKeptSealedPerOrganization(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	acme, other := org(t, s, "acme", "Acme"), org(t, s, "other", "Other")

	tn := tenant(t, s, acme)
	input := newGitConnection("gitlab-acme", 1)
	created := storedConnection(t, must[store.GitConnectionSaved](t, "create")(tn.CreateGitConnection(ctx, input)))
	if created.ID != input.ID || created.Org != acme || created.Provider != store.GitProviderGitLab ||
		created.AuthKind != store.GitAuthToken || created.TokenHint != "abcd" || created.LastCheckedAt.IsSome() {
		t.Fatalf("created: %+v", created)
	}
	if diff := cmp.Diff(sealedTag(1), created.Token); diff != "" {
		t.Fatalf("token (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(sealedTag(2), created.WebhookSecret); diff != "" {
		t.Fatalf("webhook secret (-want +got):\n%s", diff)
	}
	// The name is the organization's.
	again := must[store.GitConnectionSaved](t, "create again")(tn.CreateGitConnection(ctx, newGitConnection("gitlab-acme", 3)))
	if _, ok := again.(store.GitConnectionNameTaken); !ok {
		t.Fatalf("a taken name: %#v", again)
	}
	commit(t, tn)

	// A webhook finds the organization from the connection alone.
	if o, ok, err := s.GitConnectionOrg(ctx, input.ID); err != nil || !ok || o != acme {
		t.Fatalf("org of the connection: %v %v %v", o, ok, err)
	}
	// Another organization neither sees nor changes it, and may use the name.
	tn = tenant(t, s, other)
	if _, found, err := tn.GitConnection(ctx, input.ID); err != nil || found {
		t.Fatalf("another organization sees it: %v %v", found, err)
	}
	gone := must[store.GitConnectionSaved](t, "update")(tn.UpdateGitConnection(ctx, input.ID,
		store.GitConnectionChange{Name: opt.Some("stolen")}))
	if _, ok := gone.(store.GitConnectionNotFound); !ok {
		t.Fatalf("changed across organizations: %#v", gone)
	}
	storedConnection(t, must[store.GitConnectionSaved](t, "same name")(tn.CreateGitConnection(ctx, newGitConnection("gitlab-acme", 4))))
	commit(t, tn)

	tn = tenant(t, s, acme)
	token := store.SealedToken{Sealed: sealedTag(5), Hint: "wxyz"}
	changed := storedConnection(t, must[store.GitConnectionSaved](t, "update")(tn.UpdateGitConnection(ctx, input.ID,
		store.GitConnectionChange{Token: opt.Some(token), DefaultBranch: opt.Some(opt.Some("main"))})))
	if changed.Name != "gitlab-acme" || changed.TokenHint != "wxyz" || changed.DefaultBranch != opt.Some("main") {
		t.Fatalf("changed: %+v", changed)
	}
	if diff := cmp.Diff(sealedTag(5), changed.Token); diff != "" {
		t.Fatalf("new token (-want +got):\n%s", diff)
	}
	cleared := storedConnection(t, must[store.GitConnectionSaved](t, "clear")(tn.UpdateGitConnection(ctx, input.ID,
		store.GitConnectionChange{DefaultBranch: opt.Some(opt.None[string]())})))
	if cleared.DefaultBranch.IsSome() || cleared.TokenHint != "wxyz" {
		t.Fatalf("cleared: %+v", cleared)
	}
	if diff := cmp.Diff(sealedTag(2), cleared.WebhookSecret); diff != "" {
		t.Fatalf("the webhook secret changed with the token (-want +got):\n%s", diff)
	}
	rotated := storedConnection(t, must[store.GitConnectionSaved](t, "rotate")(tn.UpdateGitConnection(ctx, input.ID,
		store.GitConnectionChange{WebhookSecret: opt.Some(sealedTag(6))})))
	if diff := cmp.Diff(sealedTag(6), rotated.WebhookSecret); diff != "" {
		t.Fatalf("rotated webhook secret (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(sealedTag(5), rotated.Token); diff != "" {
		t.Fatalf("the token changed with the webhook secret (-want +got):\n%s", diff)
	}
	if !must[bool](t, "check")(tn.RecordGitConnectionCheck(ctx, input.ID, store.GitConnectionCheck{
		Username: opt.Some("alice"), Error: opt.Some(strings.Repeat("x", 2000)),
	})) {
		t.Fatal("no check recorded")
	}
	checked, _, err := tn.GitConnectionByName(ctx, "gitlab-acme")
	if err != nil || checked.Username != opt.Some("alice") || checked.LastCheckedAt.IsNone() ||
		len(checked.LastError.Or("")) != 1024 {
		t.Fatalf("checked: %+v %v", checked, err)
	}
	list := must[[]store.GitConnection](t, "list")(tn.GitConnections(ctx))
	if len(list) != 1 || list[0].ID != input.ID {
		t.Fatalf("list: %+v", list)
	}
	if deleted := must[store.GitConnectionDeleted](t, "delete")(tn.DeleteGitConnection(ctx, input.ID)); deleted != (store.GitConnectionRemoved{}) {
		t.Fatalf("delete: %#v", deleted)
	}
	if deleted := must[store.GitConnectionDeleted](t, "delete again")(tn.DeleteGitConnection(ctx, input.ID)); deleted != (store.GitConnectionNotFound{}) {
		t.Fatalf("delete again: %#v", deleted)
	}
	commit(t, tn)
}

func TestSourcesBindThroughConnections(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "acme", 7)

	tn := tenant(t, s, f.org)
	input := newGitConnection("gitlab-acme", 1)
	storedConnection(t, must[store.GitConnectionSaved](t, "create")(tn.CreateGitConnection(ctx, input)))
	binding := bindingInput(t)
	binding.Connection = opt.Some(ids.New[ids.GitConnection]())
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, binding)); !isBound[store.BoundConnectionMissing](bound) {
		t.Fatalf("an unknown connection: %#v", bound)
	}
	binding.Connection = opt.Some(input.ID)
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, binding)); !isBound[store.BoundChanged](bound) {
		t.Fatalf("moved to the connection: %#v", bound)
	}
	moved, ok, err := tn.BindingOfTarget(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("binding: %v %v", ok, err)
	}
	if moved.Provider != store.GitProviderGitLab || moved.Connection != opt.Some(input.ID) || moved.InstallationID != 0 {
		t.Fatalf("moved: %+v", moved)
	}
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, binding)); !isBound[store.BoundUnchanged](bound) {
		t.Fatalf("the same binding: %#v", bound)
	}
	pushed := must[[]store.SourceBinding](t, "push")(tn.BindingsForConnectionPush(ctx, input.ID, binding.Repository, binding.Branch))
	if len(pushed) != 1 || pushed[0].ID != moved.ID {
		t.Fatalf("push: %+v", pushed)
	}
	if found := must[[]store.SourceBinding](t, "app push")(tn.BindingsForPush(ctx, 7, binding.Repository, binding.Branch)); len(found) != 0 {
		t.Fatalf("an installation push still finds it: %+v", found)
	}
	// A connection a source reads through stays.
	deleted := must[store.GitConnectionDeleted](t, "delete")(tn.DeleteGitConnection(ctx, input.ID))
	if deleted != (store.GitConnectionInUse{Bindings: 1}) {
		t.Fatalf("delete in use: %#v", deleted)
	}
	commit(t, tn)
}

func TestOnlyGitlabSourcesNestInSubgroups(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "acme", 7)

	tn := tenant(t, s, f.org)
	gitlab := newGitConnection("gitlab-acme", 1)
	storedConnection(t, must[store.GitConnectionSaved](t, "create")(tn.CreateGitConnection(ctx, gitlab)))
	github := newGitConnection("github-acme", 3)
	github.Provider, github.BaseURL = store.GitProviderGitHub, "https://api.github.com"
	storedConnection(t, must[store.GitConnectionSaved](t, "create")(tn.CreateGitConnection(ctx, github)))
	binding := bindingInput(t)
	binding.Repository = must[source.RepoName](t, "path")(source.ParseNestedRepoName("acme/platform/shop"))
	binding.Connection = opt.Some(github.ID)
	if _, err := tn.BindSource(ctx, f.project, f.target, binding); kerrors.CodeOf(err) != kerrors.Validation {
		t.Fatalf("a nested path through GitHub: %v", err)
	}
	binding.Connection = opt.Some(gitlab.ID)
	if bound := must[store.Bound](t, "bind")(tn.BindSource(ctx, f.project, f.target, binding)); !isBound[store.BoundChanged](bound) {
		t.Fatalf("a nested path through GitLab: %#v", bound)
	}
	bound, ok, err := tn.BindingOfTarget(ctx, f.target)
	if err != nil || !ok || bound.Repository != binding.Repository {
		t.Fatalf("binding: %+v %v %v", bound, ok, err)
	}
	pushed := must[[]store.SourceBinding](t, "push")(tn.BindingsForConnectionPush(ctx, gitlab.ID, binding.Repository, binding.Branch))
	if len(pushed) != 1 || pushed[0].ID != bound.ID {
		t.Fatalf("push: %+v", pushed)
	}
	commit(t, tn)
}

func TestProvidersReadTheirRepositories(t *testing.T) {
	if r, err := store.GitProviderGitLab.ParseRepository("Acme/Platform/Shop"); err != nil || r.String() != "acme/platform/shop" {
		t.Errorf("gitlab: %v, %v", r, err)
	}
	for _, p := range []store.GitProvider{store.GitProviderGitHub, store.GitProviderGitea} {
		if _, err := p.ParseRepository("acme/platform/shop"); err == nil {
			t.Errorf("%s accepted a nested path", p)
		}
		if r, err := p.ParseRepository("acme/shop"); err != nil || r.String() != "acme/shop" {
			t.Errorf("%s: %v, %v", p, r, err)
		}
	}
}

func TestBuildsKeepTheirLogAndStages(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newBuildFixture(t, s, "acme", 7)
	queued, ok := syncHead(t, s, f, headA).(store.HeadQueued)
	if !ok {
		t.Fatal("no build queued")
	}
	tn := tenant(t, s, f.org)
	if tail, err := tn.BuildLogTail(ctx, queued.Attempt); err != nil || tail.IsSome() {
		t.Fatalf("a fresh build's log: %v %v", tail, err)
	}
	log := strings.Repeat("é", store.MaxBuildLogTail)
	if !must[bool](t, "tail")(tn.SetBuildLogTail(ctx, queued.Attempt, log)) {
		t.Fatal("no tail kept")
	}
	tail, err := tn.BuildLogTail(ctx, queued.Attempt)
	if kept := tail.Or(""); err != nil || len(kept) > store.MaxBuildLogTail || !strings.HasSuffix(log, kept) || kept == "" {
		t.Fatalf("tail: %d bytes, %v", len(kept), err)
	}
	stages := []store.BuildStage{
		{Name: store.StageClone, Status: store.StageSucceeded, StartedAt: opt.Some[int64](1), FinishedAt: opt.Some[int64](2)},
		{Name: store.StageBuild, Status: store.StageRunning, StartedAt: opt.Some[int64](2), Detail: opt.Some("step 3/9")},
	}
	if !must[bool](t, "stages")(tn.SetBuildStages(ctx, queued.Attempt, stages)) {
		t.Fatal("no stages kept")
	}
	if _, err := tn.SetBuildStages(ctx, queued.Attempt, []store.BuildStage{{Name: "compile", Status: store.StageRunning}}); err == nil {
		t.Fatal("an unknown stage was kept")
	}
	commit(t, tn)
	tn = tenant(t, s, f.org)
	attempt, ok, err := tn.BuildOfTarget(ctx, f.target, queued.Attempt)
	if err != nil || !ok {
		t.Fatalf("attempt: %v %v", ok, err)
	}
	if diff := cmp.Diff(stages, attempt.Stages, cmp.Comparer(func(a, b store.BuildStage) bool { return a == b })); diff != "" {
		t.Fatalf("stages (-want +got):\n%s", diff)
	}
	if attempt.Provider != store.GitProviderGitHub || attempt.InstallationID != 7 || attempt.Connection.IsSome() {
		t.Fatalf("reader: %+v", attempt)
	}
	commit(t, tn)
}

func TestOrgRegistriesBackEnvironmentsWithoutALogin(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "acme")

	tn := tenant(t, s, f.org)
	input := store.NewOrgRegistry{
		ID: ids.New[ids.OrgRegistry](), Name: "ghcr", Preset: store.PresetGHCR, Server: "ghcr.io",
		Username: "bot", Password: sealedTag(1), CreatedBy: integrationsBy,
	}
	saved := must[store.OrgRegistrySaved](t, "create")(tn.CreateOrgRegistry(ctx, input))
	created, ok := saved.(store.OrgRegistryStored)
	if !ok || created.Registry.ID != input.ID || created.Registry.Server != "ghcr.io" {
		t.Fatalf("created: %#v", saved)
	}
	sameServer := input
	sameServer.ID, sameServer.Name = ids.New[ids.OrgRegistry](), "ghcr-2"
	if taken := must[store.OrgRegistrySaved](t, "same server")(tn.CreateOrgRegistry(ctx, sameServer)); taken != (store.OrgRegistryTaken{}) {
		t.Fatalf("one login per server: %#v", taken)
	}
	found, ok, err := tn.OrgRegistryForServer(ctx, "ghcr.io")
	if err != nil || !ok || found.ID != input.ID {
		t.Fatalf("for ghcr.io: %+v %v %v", found, ok, err)
	}
	if _, ok, err := tn.OrgRegistryForServer(ctx, "quay.io"); err != nil || ok {
		t.Fatalf("for quay.io: %v %v", ok, err)
	}
	rotated := must[store.OrgRegistrySaved](t, "rotate")(tn.UpdateOrgRegistry(ctx, input.ID,
		store.OrgRegistryChange{Password: opt.Some(sealedTag(9))}))
	if r, ok := rotated.(store.OrgRegistryStored); !ok || !cmp.Equal(r.Registry.Password, sealedTag(9)) || r.Registry.Username != "bot" {
		t.Fatalf("rotated: %#v", rotated)
	}
	if !must[bool](t, "check")(tn.RecordOrgRegistryCheck(ctx, input.ID, opt.Some("unauthorized"))) {
		t.Fatal("no check recorded")
	}
	if r, _, err := tn.OrgRegistryByName(ctx, "ghcr"); err != nil || r.LastError != opt.Some("unauthorized") {
		t.Fatalf("checked: %+v %v", r, err)
	}
	commit(t, tn)

	// Another organization does not see it.
	other := org(t, s, "other", "Other")
	tn = tenant(t, s, other)
	if _, ok, err := tn.OrgRegistryForServer(ctx, "ghcr.io"); err != nil || ok {
		t.Fatalf("another organization's: %v %v", ok, err)
	}
	commit(t, tn)

	tn = tenant(t, s, f.org)
	if !must[bool](t, "delete")(tn.DeleteOrgRegistry(ctx, input.ID)) {
		t.Fatal("not deleted")
	}
	if list := must[[]store.OrgRegistry](t, "list")(tn.OrgRegistries(ctx)); len(list) != 0 {
		t.Fatalf("left: %+v", list)
	}
	commit(t, tn)
}

func TestIntegrationSealsAreResealed(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	acme := org(t, s, "acme", "Acme")
	tn := tenant(t, s, acme)
	input := newGitConnection("gitlab-acme", 1)
	storedConnection(t, must[store.GitConnectionSaved](t, "create")(tn.CreateGitConnection(ctx, input)))
	stale := must[[]store.IntegrationSeal](t, "stale")(tn.StaleIntegrationSeals(ctx, 2, 10))
	if len(stale) != 2 {
		t.Fatalf("stale: %+v", stale)
	}
	for _, seal := range stale {
		next := store.SealedBytes{Ciphertext: seal.Sealed.Ciphertext, WrappedKey: []byte("new"), KeyVersion: 2}
		if !must[bool](t, "reseal")(tn.ResealIntegration(ctx, seal, next)) {
			t.Fatalf("not resealed: %+v", seal)
		}
	}
	if left := must[[]store.IntegrationSeal](t, "stale")(tn.StaleIntegrationSeals(ctx, 2, 10)); len(left) != 0 {
		t.Fatalf("left: %+v", left)
	}
	commit(t, tn)
}

func TestLogTailsStartOnACharacter(t *testing.T) {
	if got := store.LogTail("short", 10); got != "short" {
		t.Errorf("short: %q", got)
	}
	if got := store.LogTail("abcdef", 3); got != "def" {
		t.Errorf("ascii: %q", got)
	}
	// "é" is two bytes: a cut inside it starts at the next character.
	if got := store.LogTail("éé", 3); got != "é" {
		t.Errorf("utf-8: %q", got)
	}
}

package gitlab_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitlab"
)

const sha = "89abcdef0123456789abcdef0123456789abcdef"

type result[T any] struct {
	v   T
	err error
}

func check[T any](v T, err error) result[T] { return result[T]{v, err} }

func (r result[T]) must(t *testing.T) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

func serve(t *testing.T, handler http.Handler) gitprovider.Options {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	return gitprovider.Options{Transport: srv.Client().Transport}
}

// fake is a GitLab under /gitlab (a relative URL root) with the project
// acme/shop, for the token "glpat-x" whose scopes are scopes (none: an
// older GitLab without /personal_access_tokens/self).
func fake(t *testing.T, scopes ...string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("PRIVATE-TOKEN") != "glpat-x" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
				return
			}
			next(w, r)
		}
	}
	const api = "/gitlab/api/v4/"
	mux.HandleFunc("GET "+api+"user", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":5,"username":"alice"}`))
	}))
	mux.HandleFunc("GET "+api+"personal_access_tokens/self", auth(func(w http.ResponseWriter, _ *http.Request) {
		if len(scopes) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"scopes":["` + strings.Join(scopes, `","`) + `"]}`))
	}))
	mux.HandleFunc("GET "+api+"projects", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("membership") != "true" || q.Get("per_page") != "30" {
			t.Errorf("projects query %v", q)
		}
		if q.Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
		} else {
			w.Header().Set("X-Next-Page", "")
		}
		if q.Get("search") == "shop" {
			_, _ = w.Write([]byte(`[{"id":42,"path_with_namespace":"acme/shop","visibility":"private",
				"default_branch":"main","description":"The shop","web_url":"https://gitlab.example.com/acme/shop",
				"last_activity_at":"2026-09-01T10:00:00.000Z"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":42,"path_with_namespace":"acme/shop","visibility":"private"},
			{"id":43,"path_with_namespace":"acme/docs","visibility":"public","default_branch":null}]`))
	}))
	mux.HandleFunc("GET "+api+"projects/{project}", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("project") {
		case "acme/shop":
			_, _ = w.Write([]byte(`{"id":42,"path_with_namespace":"acme/shop","default_branch":"main"}`))
		case "acme/platform/shop":
			_, _ = w.Write([]byte(`{"id":44,"path_with_namespace":"acme/platform/shop","default_branch":"main"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
		}
	}))
	mux.HandleFunc("GET "+api+"projects/{project}/repository/branches", auth(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawPath, "/"+strings.ReplaceAll(r.PathValue("project"), "/", "%2F")+"/") {
			t.Errorf("the project path is not encoded: %s", r.URL.RawPath)
		}
		_, _ = w.Write([]byte(`[{"name":"main","protected":true,"default":true,"commit":{"id":"` + sha + `"}},
			{"name":"feature/x","protected":false,"default":false,"commit":{"id":"` + sha + `"}}]`))
	}))
	mux.HandleFunc("GET "+api+"projects/{project}/repository/branches/{branch}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("branch") != "feature/x" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"feature/x","commit":{"id":"` + sha + `"}}`))
	}))
	return mux
}

const base = "https://gitlab.example.com/gitlab"

func TestATokenIsCheckedWithItsScopes(t *testing.T) {
	ctx := t.Context()
	full := gitlab.New(serve(t, fake(t, "read_api", "read_repository")), base, "glpat-x")
	want := gitprovider.Account{Username: "alice", Scopes: []string{"read_api", "read_repository"}, Missing: []string{}}
	if diff := cmp.Diff(want, check(full.Whoami(ctx)).must(t)); diff != "" {
		t.Errorf("full: %s", diff)
	}
	api := gitlab.New(serve(t, fake(t, "api")), base, "glpat-x")
	if got := check(api.Whoami(ctx)).must(t); len(got.Missing) != 0 {
		t.Errorf("api grants everything: %+v", got)
	}
	short := gitlab.New(serve(t, fake(t, "read_user")), base, "glpat-x")
	if diff := cmp.Diff([]string{"read_api", "read_repository"}, check(short.Whoami(ctx)).must(t).Missing); diff != "" {
		t.Errorf("read_user only: %s", diff)
	}
	old := gitlab.New(serve(t, fake(t)), base, "glpat-x")
	if got := check(old.Whoami(ctx)).must(t); got.Username != "alice" || len(got.Scopes) != 0 || len(got.Missing) != 0 {
		t.Errorf("unknown scopes: %+v", got)
	}
	_, err := gitlab.New(serve(t, fake(t, "api")), base, "wrong").Whoami(ctx)
	var refused build.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "401") {
		t.Errorf("a wrong token: %v", err)
	}
}

func TestProjectsBranchesAndHeads(t *testing.T) {
	ctx := t.Context()
	c := gitlab.New(serve(t, fake(t, "api")), base, "glpat-x")
	page := check(c.Repositories(ctx, "", 1)).must(t)
	if !page.HasMore || len(page.Repositories) != 2 || page.Repositories[1].Private || !page.Repositories[0].Private {
		t.Errorf("page 1: %+v", page)
	}
	if last := check(c.Repositories(ctx, "", 2)).must(t); last.HasMore {
		t.Errorf("page 2 is the last: %+v", last)
	}
	found := check(c.Repositories(ctx, "shop", 2)).must(t)
	wantShop := gitprovider.Repository{
		ID: "42", FullName: "acme/shop", Private: true, DefaultBranch: opt.Some("main"),
		Description: opt.Some("The shop"), WebURL: opt.Some("https://gitlab.example.com/acme/shop"),
		UpdatedAt: opt.Some(int64(1788256800000)),
	}
	if len(found.Repositories) != 1 || !cmp.Equal(wantShop, found.Repositories[0], cmp.AllowUnexported(opt.Val[string]{}, opt.Val[int64]{})) {
		t.Errorf("search: %+v", found)
	}
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	branches := check(c.Branches(ctx, repo)).must(t)
	if len(branches) != 2 || !branches[0].Default || !branches[0].Protected || branches[1].Commit.Or("") != sha {
		t.Errorf("branches %+v", branches)
	}
	head := check(c.Head(ctx, repo, check(source.ParseBranchName("feature/x")).must(t))).must(t)
	if head.Commit.String() != sha || head.RepositoryID != 42 {
		t.Errorf("head %+v", head)
	}
	_, err := c.Head(ctx, repo, check(source.ParseBranchName("gone")).must(t))
	var notFound build.NotFound
	if !errors.As(err, &notFound) || notFound.What != "acme/shop@gone" {
		t.Errorf("a missing branch: %v", err)
	}
	_, err = c.Head(ctx, check(source.ParseRepoName("acme/nope")).must(t), check(source.ParseBranchName("main")).must(t))
	if !errors.As(err, &notFound) || notFound.What != "acme/nope" {
		t.Errorf("a missing project: %v", err)
	}
	if got := c.CloneURL(repo); got != base+"/acme/shop.git" {
		t.Errorf("clone %s", got)
	}
}

func TestProjectsInSubgroupsAreOneEncodedPath(t *testing.T) {
	ctx := t.Context()
	c := gitlab.New(serve(t, fake(t, "api")), base, "glpat-x")
	repo := check(source.ParseNestedRepoName("Acme/Platform/Shop")).must(t)
	if branches := check(c.Branches(ctx, repo)).must(t); len(branches) != 2 {
		t.Errorf("branches %+v", branches)
	}
	head := check(c.Head(ctx, repo, check(source.ParseBranchName("feature/x")).must(t))).must(t)
	if head.Commit.String() != sha || head.RepositoryID != 44 {
		t.Errorf("head %+v", head)
	}
	if got := c.CloneURL(repo); got != base+"/acme/platform/shop.git" {
		t.Errorf("clone %s", got)
	}
}

func TestAnUnreachableGitLabIsUnavailable(t *testing.T) {
	down := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	_, err := gitlab.New(serve(t, down), base, "glpat-x").Whoami(t.Context())
	var unavailable build.Unavailable
	if !errors.As(err, &unavailable) {
		t.Errorf("503: %v", err)
	}
}

func TestPushWebhooks(t *testing.T) {
	secret := []byte("hook-secret")
	if !gitlab.Verify(secret, "hook-secret") || gitlab.Verify(secret, "hook-secreT") || gitlab.Verify(secret, "") ||
		gitlab.Verify(nil, "") {
		t.Error("the token is compared exactly")
	}
	push := `{"object_kind":"push","ref":"refs/heads/main","after":"` + sha + `",
		"project":{"id":42,"path_with_namespace":"Acme/Shop"}}`
	got := check(gitlab.ParsePush(gitlab.PushEvent, []byte(push))).must(t)
	p, ok := got.Get()
	if !ok || p.Repository.String() != "acme/shop" || p.Branch.String() != "main" || p.After != sha || p.Deleted {
		t.Errorf("push %+v", got)
	}
	deleted := strings.Replace(push, sha, "0000000000000000000000000000000000000000", 1)
	if p, _ := check(gitlab.ParsePush(gitlab.PushEvent, []byte(deleted))).must(t).Get(); !p.Deleted {
		t.Error("a deleted branch")
	}
	nested := strings.Replace(push, "Acme/Shop", "Acme/Platform/Shop", 1)
	if p, _ := check(gitlab.ParsePush(gitlab.PushEvent, []byte(nested))).must(t).Get(); p.Repository.String() != "acme/platform/shop" {
		t.Errorf("a project in a subgroup: %+v", p)
	}
	ignored := map[string]string{
		"a tag push": `{"object_kind":"tag_push","ref":"refs/tags/v1","project":{"path_with_namespace":"acme/shop"}}`,
		"too deep": `{"object_kind":"push","ref":"refs/heads/main","project":{"path_with_namespace":"` +
			strings.Repeat("g/", 21) + `shop"}}`,
		"not a branch":  `{"object_kind":"push","ref":"refs/tags/v1","project":{"path_with_namespace":"acme/shop"}}`,
		"another event": push,
	}
	for name, body := range ignored {
		event := gitlab.PushEvent
		if name == "another event" {
			event = "Merge Request Hook"
		}
		if got := check(gitlab.ParsePush(event, []byte(body))).must(t); got.IsSome() {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for name, body := range map[string]string{
		"not JSON":     `{`,
		"no project":   `{"object_kind":"push","ref":"refs/heads/main"}`,
		"a bad branch": `{"object_kind":"push","ref":"refs/heads/a..b","project":{"path_with_namespace":"acme/shop"}}`,
	} {
		if _, err := gitlab.ParsePush(gitlab.PushEvent, []byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCommitStatusesUseGitlabsWords(t *testing.T) {
	type call struct{ path, token, body string }
	var got []call
	o := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method + " " + r.URL.EscapedPath(), r.Header.Get("PRIVATE-TOKEN"), string(body)})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	c := gitlab.New(o, base, "glpat-x")
	repo := check(source.ParseNestedRepoName("acme/platform/shop")).must(t)
	for _, state := range []string{"pending", "success", "failure", "error"} {
		status := gitprovider.CommitStatus{State: state, Context: "kuben/build", Description: "build " + state}
		if state == "success" {
			status.TargetURL = opt.Some("https://kuben.example.com/projects/shop/prod/web")
		}
		if err := c.CommitStatus(t.Context(), repo, sha, status); err != nil {
			t.Fatal(err)
		}
	}
	path := "POST /gitlab/api/v4/projects/acme%2Fplatform%2Fshop/statuses/" + sha
	want := []call{
		{path, "glpat-x", `{"state":"pending","name":"kuben/build","description":"build pending"}`},
		{path, "glpat-x", `{"state":"success","name":"kuben/build","description":"build success",` +
			`"target_url":"https://kuben.example.com/projects/shop/prod/web"}`},
		{path, "glpat-x", `{"state":"failed","name":"kuben/build","description":"build failure"}`},
		{path, "glpat-x", `{"state":"failed","name":"kuben/build","description":"build error"}`},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(call{})); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	refused := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	var no build.Refused
	if err := gitlab.New(refused, base, "glpat-x").CommitStatus(t.Context(), repo, sha, gitprovider.CommitStatus{State: "success"}); !errors.As(err, &no) {
		t.Errorf("a token without the api scope: %v", err)
	}
}

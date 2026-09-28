package gitprovider_test

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
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// serve is an in-memory server for handler and the options that reach it,
// whatever the URL's host.
func serve(t *testing.T, handler http.Handler) gitprovider.Options {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	return gitprovider.Options{Transport: srv.Client().Transport}
}

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

func TestBaseURLsAreStoredOneWay(t *testing.T) {
	cases := []struct {
		provider store.GitProvider
		given    string
		want     string
	}{
		{store.GitProviderGitHub, "", "https://api.github.com"},
		{store.GitProviderGitHub, "https://github.com/", "https://api.github.com"},
		{store.GitProviderGitHub, "https://GHE.example.com/api/v3/", "https://ghe.example.com/api/v3"},
		{store.GitProviderGitLab, "", "https://gitlab.com"},
		{store.GitProviderGitLab, "https://gitlab.example.com/api/v4", "https://gitlab.example.com"},
		{store.GitProviderGitLab, "https://example.com/gitlab/", "https://example.com/gitlab"},
		{store.GitProviderGitea, "http://gitea.internal:3000/api/v1/", "http://gitea.internal:3000"},
		{store.GitProviderGitea, " https://codeberg.org ", "https://codeberg.org"},
	}
	for _, c := range cases {
		got, err := gitprovider.BaseURL(c.provider, c.given)
		if err != nil || got != c.want {
			t.Errorf("%s %q: %q, %v; want %q", c.provider, c.given, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"ftp://gitea.example.com", "https://user:pw@gitea.example.com", "https://gitea.example.com/?a=b",
		"https://gitea.example.com/#x", "gitea.example.com", "https://",
	} {
		if got, err := gitprovider.BaseURL(store.GitProviderGitea, bad); err == nil {
			t.Errorf("%q was accepted as %q", bad, got)
		}
	}
	if _, err := gitprovider.BaseURL(store.GitProviderGitea, ""); err == nil {
		t.Error("Gitea has no public service")
	}
}

func TestMissingScopesHonourTheirGrants(t *testing.T) {
	need := map[string][]string{"read_api": {"read_api", "api"}, "read_repository": {"read_repository", "api"}}
	order := []string{"read_api", "read_repository"}
	if got := gitprovider.Missing([]string{"api"}, need, order); len(got) != 0 {
		t.Errorf("api grants both: %v", got)
	}
	if diff := cmp.Diff([]string{"read_repository"}, gitprovider.Missing([]string{"read_api"}, need, order)); diff != "" {
		t.Error(diff)
	}
	if diff := cmp.Diff(order, gitprovider.Missing(nil, need, order)); diff != "" {
		t.Error(diff)
	}
}

func TestPagesAreReadFromTheHeaders(t *testing.T) {
	h := func(pairs ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out.Set(pairs[i], pairs[i+1])
		}
		return out
	}
	cases := []struct {
		name   string
		header http.Header
		got    int
		want   bool
	}{
		{"GitLab has a next page", h("X-Next-Page", "3"), 30, true},
		{"GitLab's last page", h("X-Next-Page", ""), 30, false},
		{"a Link to the next page", h("Link", `<https://x/?page=2>; rel="next", <https://x/?page=9>; rel="last"`), 1, true},
		{"a Link without a next page", h("Link", `<https://x/?page=1>; rel="first"`), 30, false},
		{"no header, a full page", h(), 30, true},
		{"no header, a short page", h(), 12, false},
	}
	for _, c := range cases {
		if got := gitprovider.NextPage(c.header, c.got, 30); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// githubFake is a GitHub with one repository, acme/shop, for the token
// "t0k3n".
func githubFake(t *testing.T, scopes string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer t0k3n" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /user", auth(func(w http.ResponseWriter, _ *http.Request) {
		if scopes != "-" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		_, _ = w.Write([]byte(`{"login":"octo"}`))
	}))
	mux.HandleFunc("GET /user/repos", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/user/repos?page=2>; rel="next"`)
		}
		_, _ = w.Write([]byte(`[{"id":1,"full_name":"acme/shop","private":true,"default_branch":"main",
			"description":null,"html_url":"https://github.com/acme/shop","updated_at":"2026-09-01T10:00:00Z"},
			{"id":2,"full_name":"acme/blog","private":false,"default_branch":"trunk"}]`))
	}))
	mux.HandleFunc("GET /repos/acme/shop", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42,"full_name":"acme/shop","default_branch":"main"}`))
	}))
	mux.HandleFunc("GET /repos/acme/shop/branches", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"main","protected":true,"commit":{"sha":"` + sha + `"}},
			{"name":"feature/x","protected":false,"commit":{"sha":"` + sha + `"}}]`))
	}))
	mux.HandleFunc("GET /repos/acme/shop/branches/{branch}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("branch") != "feature/x" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"feature/x","commit":{"sha":"` + sha + `"}}`))
	}))
	mux.HandleFunc("GET /repos/acme/down", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	return mux
}

func TestAGitHubTokenIsCheckedAndBrowsed(t *testing.T) {
	ctx := t.Context()
	o := serve(t, githubFake(t, "repo, read:org"))
	g := gitprovider.NewGitHub(o, gitprovider.GitHubAPI, "t0k3n")
	account := check(g.Whoami(ctx)).must(t)
	want := gitprovider.Account{Username: "octo", Scopes: []string{"repo", "read:org"}, Missing: []string{}}
	if diff := cmp.Diff(want, account); diff != "" {
		t.Errorf("whoami: %s", diff)
	}
	page := check(g.Repositories(ctx, "", 1)).must(t)
	if !page.HasMore || len(page.Repositories) != 2 {
		t.Fatalf("page %+v", page)
	}
	shop := page.Repositories[0]
	if shop.ID != "1" || shop.FullName != "acme/shop" || !shop.Private || shop.DefaultBranch.Or("") != "main" ||
		shop.Description.IsSome() || shop.UpdatedAt.Or(0) != 1788256800000 {
		t.Errorf("shop %+v", shop)
	}
	if found := check(g.Repositories(ctx, "BLOG", 2)).must(t); found.HasMore || len(found.Repositories) != 1 ||
		found.Repositories[0].FullName != "acme/blog" {
		t.Errorf("search %+v", found)
	}
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	branches := check(g.Branches(ctx, repo)).must(t)
	wantBranches := []gitprovider.Branch{
		{Name: "main", Protected: true, Default: true, Commit: opt.Some(sha)},
		{Name: "feature/x", Commit: opt.Some(sha)},
	}
	if diff := cmp.Diff(wantBranches, branches, cmp.AllowUnexported(opt.Val[string]{})); diff != "" {
		t.Errorf("branches: %s", diff)
	}
	head := check(g.Head(ctx, repo, check(source.ParseBranchName("feature/x")).must(t))).must(t)
	if head.Commit.String() != sha || head.RepositoryID != 42 {
		t.Errorf("head %+v", head)
	}
	if got := g.CloneURL(repo); got != "https://github.com/acme/shop.git" {
		t.Errorf("clone %s", got)
	}
	ghe := gitprovider.NewGitHub(o, "https://ghe.example.com/api/v3", "t0k3n")
	if got := ghe.CloneURL(repo); got != "https://ghe.example.com/acme/shop.git" {
		t.Errorf("enterprise clone %s", got)
	}
}

func TestGitHubAnswersBecomeProviderErrors(t *testing.T) {
	ctx := t.Context()
	o := serve(t, githubFake(t, "read:org"))
	g := gitprovider.NewGitHub(o, gitprovider.GitHubAPI, "t0k3n")
	if account := check(g.Whoami(ctx)).must(t); len(account.Missing) != 1 || account.Missing[0] != "repo" {
		t.Errorf("a classic token without repo: %+v", account)
	}
	fine := gitprovider.NewGitHub(serve(t, githubFake(t, "-")), gitprovider.GitHubAPI, "t0k3n")
	if account := check(fine.Whoami(ctx)).must(t); len(account.Scopes) != 0 || len(account.Missing) != 0 {
		t.Errorf("a fine-grained token tells no scopes: %+v", account)
	}
	_, err := gitprovider.NewGitHub(o, gitprovider.GitHubAPI, "wrong").Whoami(ctx)
	var refused build.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "Bad credentials") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("a wrong token: %v", err)
	}
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	_, err = g.Head(ctx, repo, check(source.ParseBranchName("gone")).must(t))
	var notFound build.NotFound
	if !errors.As(err, &notFound) || notFound.What != "acme/shop@gone" {
		t.Errorf("a missing branch: %v", err)
	}
	_, err = g.Branches(ctx, check(source.ParseRepoName("acme/down")).must(t))
	var unavailable build.Unavailable
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Reason, "502 upstream down") {
		t.Errorf("a failing provider: %v", err)
	}
}

func TestGithubConnectionPushes(t *testing.T) {
	push := `{"ref":"refs/heads/feat/x","after":"` + sha + `","repository":{"full_name":"Acme/Shop"}}`
	got, err := gitprovider.ParseGitHubPush(gitprovider.GitHubPushEvent, []byte(push))
	p, ok := got.Get()
	if err != nil || !ok || p.Repository.String() != "acme/shop" || p.Branch.String() != "feat/x" || p.After != sha || p.Deleted {
		t.Fatalf("push %+v %v", got, err)
	}
	deleted := `{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `","deleted":true,"repository":{"full_name":"acme/shop"}}`
	if got, err := gitprovider.ParseGitHubPush(gitprovider.GitHubPushEvent, []byte(deleted)); err != nil || !got.Or(gitprovider.Push{}).Deleted {
		t.Errorf("a deleted branch: %+v %v", got, err)
	}
	for name, c := range map[string]struct{ event, body string }{
		"a tag":         {gitprovider.GitHubPushEvent, `{"ref":"refs/tags/v1","repository":{"full_name":"acme/shop"}}`},
		"another event": {"pull_request", push},
	} {
		if got, err := gitprovider.ParseGitHubPush(c.event, []byte(c.body)); err != nil || got.IsSome() {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for name, body := range map[string]string{
		"not JSON":      `{`,
		"no repository": `{"ref":"refs/heads/main"}`,
		"a nested path": `{"ref":"refs/heads/main","repository":{"full_name":"a/b/c"}}`,
		"a bad branch":  `{"ref":"refs/heads/a..b","repository":{"full_name":"acme/shop"}}`,
	} {
		if _, err := gitprovider.ParseGitHubPush(gitprovider.GitHubPushEvent, []byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestGithubTokensSetCommitStatuses(t *testing.T) {
	var path, token, body string
	o := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, token, body = r.Method+" "+r.URL.EscapedPath(), r.Header.Get("Authorization"), string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	status := gitprovider.CommitStatus{
		State: "success", Context: "kuben/build", Description: "build.succeeded",
		TargetURL: opt.Some("https://kuben.example.com/projects/shop/prod/web"),
	}
	if err := gitprovider.NewGitHub(o, gitprovider.GitHubAPI, "t0k3n").CommitStatus(t.Context(), repo, sha, status); err != nil {
		t.Fatal(err)
	}
	want := `{"state":"success","context":"kuben/build","description":"build.succeeded",` +
		`"target_url":"https://kuben.example.com/projects/shop/prod/web"}`
	if path != "POST /repos/acme/shop/statuses/"+sha || token != "Bearer t0k3n" || body != want {
		t.Errorf("got %s %q %s", path, token, body)
	}
}

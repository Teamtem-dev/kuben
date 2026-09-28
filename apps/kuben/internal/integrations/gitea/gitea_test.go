package gitea_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitea"
)

const sha = "fedcba9876543210fedcba9876543210fedcba98"

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

// fake is a Forgejo with the repository acme/shop for the token "t0k".
func fake(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "token t0k" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"user does not exist [uid: 0, name: ]"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /api/v1/user", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"login":"bob"}`))
	}))
	mux.HandleFunc("GET /api/v1/user/repos", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "30" {
			t.Errorf("limit %s", r.URL.Query().Get("limit"))
		}
		w.Header().Set("Link", `<https://git.example.com/api/v1/user/repos?page=2&limit=30>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":7,"full_name":"acme/shop","private":true,"default_branch":"main","description":"",
			"html_url":"https://git.example.com/acme/shop","updated_at":"2026-09-01T10:00:00Z"}]`))
	}))
	mux.HandleFunc("GET /api/v1/repos/search", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "sho" {
			t.Errorf("q %s", r.URL.Query().Get("q"))
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":[{"id":7,"full_name":"acme/shop","private":true,"description":"Shop"}]}`))
	}))
	mux.HandleFunc("GET /api/v1/repos/acme/shop", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"full_name":"acme/shop","default_branch":"main"}`))
	}))
	mux.HandleFunc("GET /api/v1/repos/acme/shop/branches", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"main","protected":true,"commit":{"id":"` + sha + `"}},{"name":"dev","commit":{"id":"` + sha + `"}}]`))
	}))
	mux.HandleFunc("GET /api/v1/repos/acme/shop/branches/{branch}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("branch") != "release/1.0" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"release/1.0","commit":{"id":"` + sha + `"}}`))
	}))
	return mux
}

const base = "https://git.example.com"

func TestAGiteaTokenIsCheckedAndBrowsed(t *testing.T) {
	ctx := t.Context()
	c := gitea.New(serve(t, fake(t)), base, "t0k")
	account := check(c.Whoami(ctx)).must(t)
	if account.Username != "bob" || len(account.Scopes) != 0 || len(account.Missing) != 0 {
		t.Errorf("whoami %+v", account)
	}
	page := check(c.Repositories(ctx, "", 1)).must(t)
	if !page.HasMore || len(page.Repositories) != 1 {
		t.Fatalf("page %+v", page)
	}
	if r := page.Repositories[0]; r.ID != "7" || !r.Private || r.Description.IsSome() || r.DefaultBranch.Or("") != "main" ||
		r.UpdatedAt.IsNone() {
		t.Errorf("repository %+v", r)
	}
	found := check(c.Repositories(ctx, "sho", 1)).must(t)
	if found.HasMore || len(found.Repositories) != 1 || found.Repositories[0].Description.Or("") != "Shop" {
		t.Errorf("search %+v", found)
	}
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	branches := check(c.Branches(ctx, repo)).must(t)
	if len(branches) != 2 || !branches[0].Default || !branches[0].Protected || branches[1].Default {
		t.Errorf("branches %+v", branches)
	}
	head := check(c.Head(ctx, repo, check(source.ParseBranchName("release/1.0")).must(t))).must(t)
	if head.Commit.String() != sha || head.RepositoryID != 7 {
		t.Errorf("head %+v", head)
	}
	if got := c.CloneURL(repo); got != base+"/acme/shop.git" {
		t.Errorf("clone %s", got)
	}
	_, err := gitea.New(serve(t, fake(t)), base, "nope").Whoami(ctx)
	var refused build.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "user does not exist") {
		t.Errorf("a wrong token: %v", err)
	}
	_, err = c.Head(ctx, repo, check(source.ParseBranchName("gone")).must(t))
	var notFound build.NotFound
	if !errors.As(err, &notFound) {
		t.Errorf("a missing branch: %v", err)
	}
}

func TestPushWebhooksAreSigned(t *testing.T) {
	secret := []byte("hook-secret")
	body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `","repository":{"id":7,"full_name":"acme/shop"}}`)
	signature := gitea.Sign(secret, body)
	cases := []struct {
		name      string
		secret    []byte
		body      []byte
		signature string
		want      bool
	}{
		{"Gitea's signature", secret, body, signature, true},
		{"with a prefix", secret, body, "sha256=" + signature, true},
		{"upper case hex", secret, body, strings.ToUpper(signature), true},
		{"another body", secret, append([]byte(" "), body...), signature, false},
		{"another secret", []byte("other"), body, signature, false},
		{"no secret", nil, body, gitea.Sign(nil, body), false},
		{"not hex", secret, body, "zz", false},
		{"too short", secret, body, signature[:10], false},
	}
	for _, c := range cases {
		if got := gitea.Verify(c.secret, c.body, c.signature); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	p, ok := check(gitea.ParsePush(gitea.PushEvent, body)).must(t).Get()
	if !ok || p.Repository.String() != "acme/shop" || p.Branch.String() != "main" || p.Deleted {
		t.Errorf("push %+v", p)
	}
	if got := check(gitea.ParsePush("pull_request", body)).must(t); got.IsSome() {
		t.Error("another event")
	}
	tag := []byte(`{"ref":"refs/tags/v1","repository":{"full_name":"acme/shop"}}`)
	if got := check(gitea.ParsePush(gitea.PushEvent, tag)).must(t); got.IsSome() {
		t.Error("a tag")
	}
	for name, bad := range map[string]string{
		"not JSON": `[`, "no repository": `{"ref":"refs/heads/main"}`,
		"a bad repository": `{"ref":"refs/heads/main","repository":{"full_name":"no-slash"}}`,
	} {
		if _, err := gitea.ParsePush(gitea.PushEvent, []byte(bad)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCommitStatusesAreGithubsWords(t *testing.T) {
	var path, token, body string
	o := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, token, body = r.Method+" "+r.URL.EscapedPath(), r.Header.Get("Authorization"), string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	repo := check(source.ParseRepoName("acme/shop")).must(t)
	status := gitprovider.CommitStatus{
		State: "failure", Context: "kuben/prod", Description: strings.Repeat("é", 150),
	}
	if err := gitea.New(o, base, "t0k").CommitStatus(t.Context(), repo, sha, status); err != nil {
		t.Fatal(err)
	}
	want := `{"state":"failure","context":"kuben/prod","description":"` + strings.Repeat("é", 140) + `"}`
	if path != "POST /api/v1/repos/acme/shop/statuses/"+sha || token != "token t0k" || body != want {
		t.Errorf("got %s %q %s", path, token, body)
	}
}

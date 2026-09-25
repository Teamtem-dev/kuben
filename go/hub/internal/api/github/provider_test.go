package github_test

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

const (
	nowMs = 1_700_000_000_000
	sha   = "0123456789abcdef0123456789abcdef01234567"
)

// request is what the fake GitHub received.
type request struct {
	Method, URI, Auth, Accept, Version, ContentType, UserAgent string
	Body                                                       string
}

// fakeGitHub answers like GitHub: tokens for installation 7, repository
// 42 `acme/shop`, and whatever answer returns first.
type fakeGitHub struct {
	mu     sync.Mutex
	seen   []request
	minted int
	// expiresAt is the expiry of minted tokens.
	expiresAt string
	// answer overrides the default answer when it returns a status.
	answer func(r request) (int, string)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := request{
		Method: r.Method, URI: r.RequestURI, Auth: r.Header.Get("Authorization"), Accept: r.Header.Get("Accept"),
		Version: r.Header.Get("X-GitHub-Api-Version"), ContentType: r.Header.Get("Content-Type"),
		UserAgent: r.Header.Get("User-Agent"), Body: string(body),
	}
	f.mu.Lock()
	f.seen = append(f.seen, req)
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		if status, text := answer(req); status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, text)
			return
		}
	}
	status, text := f.defaultAnswer(req)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

func (f *fakeGitHub) defaultAnswer(r request) (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URI == "/app/installations/7/access_tokens":
		f.minted++
		return http.StatusCreated, fmt.Sprintf(`{"token":"ghs_%d","expires_at":%q,"permissions":{}}`, f.minted, f.expiresAt)
	case r.Method == http.MethodGet && r.URI == "/repos/acme/shop":
		return http.StatusOK, `{"id":42,"full_name":"acme/shop"}`
	case r.Method == http.MethodGet && strings.HasPrefix(r.URI, "/repos/acme/shop/git/ref/"):
		return http.StatusOK, `{"ref":"refs/x","object":{"sha":"` + sha + `","type":"commit"}}`
	case r.Method == http.MethodDelete && r.URI == "/installation/token":
		return http.StatusNoContent, ""
	}
	return http.StatusNotFound, `{"message":"Not Found"}`
}

// set changes the fake under its lock.
func (f *fakeGitHub) set(change func(*fakeGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeGitHub) mintedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted
}

func (f *fakeGitHub) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.seen...)
}

// newApp is an App with a fresh key against a fake GitHub.
func newApp(t *testing.T) (*github.App, *fakeGitHub, *rsa.PublicKey) {
	t.Helper()
	fake := &fakeGitHub{expiresAt: "2030-01-01T00:00:00Z"}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	key := rsaKey(t, 2048)
	cfg := config.DefaultGitCfg()
	cfg.GithubAppID = opt.Some[uint64](12345)
	cfg.GithubPrivateKeyFile = opt.Some(keyFile(t, key))
	cfg.GithubWebhookSecret = "s3cret"
	cfg.GithubAPIURL = server.URL + "/"
	app, err := github.New(cfg, clock.Fixed(nowMs))
	if err != nil {
		t.Fatal(err)
	}
	return app, fake, &key.PublicKey
}

func branch(t *testing.T, name string) source.BranchName {
	t.Helper()
	b, err := source.ParseBranchName(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// verifyJWT checks that auth is `Bearer <JWT>` signed by the App's key and
// returns its claims.
func verifyJWT(t *testing.T, auth string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	jwt, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("authorization %q", auth)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the JWT is not signed by the App's key: %v", err)
	}
	return decodeSegment(t, parts[1])
}

func TestFetchTokensAreNarrowedToOneRepository(t *testing.T) {
	app, fake, pub := newApp(t)
	token, err := app.FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "ghs_1" || token.ExpiresAt != 1_893_456_000_000 {
		t.Errorf("token %q, expires %d", token.Token, token.ExpiresAt)
	}
	seen := fake.requests()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	r := seen[0]
	if r.Method != http.MethodPost || r.URI != "/app/installations/7/access_tokens" {
		t.Errorf("%s %s", r.Method, r.URI)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repositories": []any{"shop"}, "permissions": map[string]any{"contents": "read"}}
	if diff := cmp.Diff(want, body); diff != "" {
		t.Errorf("request body (-want +got):\n%s", diff)
	}
	if r.Accept != "application/vnd.github+json" || r.Version != "2022-11-28" || r.ContentType != "application/json" || !strings.HasPrefix(r.UserAgent, "kuben/") {
		t.Errorf("headers %+v", r)
	}
	claims := verifyJWT(t, r.Auth, pub)
	want = map[string]any{"iss": "12345", "iat": float64(1_700_000_000 - 60), "exp": float64(1_700_000_000 + 540)}
	if diff := cmp.Diff(want, claims); diff != "" {
		t.Errorf("claims (-want +got):\n%s", diff)
	}
}

func TestHeadsAreReadWithACachedMetadataToken(t *testing.T) {
	app, fake, _ := newApp(t)
	ctx := context.Background()
	shop := repo(t, "acme/shop")
	head, err := app.Head(ctx, 7, shop, branch(t, "feat/x+y"))
	if err != nil {
		t.Fatal(err)
	}
	if head.Commit.String() != sha || head.RepositoryID != 42 {
		t.Errorf("head %+v", head)
	}
	pull, err := app.PullHead(ctx, 7, shop, 12)
	if err != nil || pull != head {
		t.Errorf("pull head %+v %v", pull, err)
	}
	var got []string
	for _, r := range fake.requests() {
		got = append(got, r.Method+" "+r.URI+" "+r.Auth[:min(len(r.Auth), 12)])
	}
	want := []string{
		"POST /app/installations/7/access_tokens Bearer eyJhb",
		"GET /repos/acme/shop Bearer ghs_1",
		"GET /repos/acme/shop/git/ref/heads/feat/x%2By Bearer ghs_1",
		"GET /repos/acme/shop Bearer ghs_1",
		"GET /repos/acme/shop/git/ref/pull/12/head Bearer ghs_1",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(fake.requests()[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]any{"contents": "read", "metadata": "read"}, body["permissions"]); diff != "" {
		t.Errorf("metadata token permissions (-want +got):\n%s", diff)
	}
}

func TestCachedTokensAreRenewedBeforeTheyExpire(t *testing.T) {
	app, fake, _ := newApp(t)
	// Nine minutes left: less than the ten-minute margin.
	expires := time.UnixMilli(nowMs + (9 * time.Minute).Milliseconds()).UTC().Format(time.RFC3339)
	fake.set(func(f *fakeGitHub) { f.expiresAt = expires })
	ctx := context.Background()
	for range 2 {
		if _, err := app.Head(ctx, 7, repo(t, "acme/shop"), branch(t, "main")); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.mintedCount(); n != 2 {
		t.Errorf("%d tokens minted, want 2", n)
	}
}

func TestAnswersAreClassified(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"missing branch", http.StatusNotFound, build.NotFound{What: "acme/shop@main"}},
		{"gone", http.StatusGone, build.NotFound{What: "acme/shop@main"}},
		{"refused", http.StatusUnauthorized, build.Refused{Reason: "acme/shop@main: HTTP 401 Unauthorized"}},
		{"forbidden", http.StatusForbidden, build.Refused{Reason: "acme/shop@main: HTTP 403 Forbidden"}},
		{"down", http.StatusServiceUnavailable, build.Unavailable{Reason: "acme/shop@main: HTTP 503 Service Unavailable"}},
		{"redirect", http.StatusMovedPermanently, build.Unavailable{Reason: "acme/shop@main: HTTP 301 Moved Permanently"}},
	}
	for _, c := range cases {
		app, fake, _ := newApp(t)
		fake.set(func(f *fakeGitHub) {
			f.answer = func(r request) (int, string) {
				if strings.Contains(r.URI, "/git/ref/") {
					return c.status, `{"message":"x"}`
				}
				return 0, ""
			}
		})
		_, err := app.Head(context.Background(), 7, repo(t, "acme/shop"), branch(t, "main"))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func TestTokenRefusalsAreClassified(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(request) (int, string) { return http.StatusNotFound, `{"message":"Not Found"}` }
	})
	_, err := app.FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if !errors.Is(err, build.NotFound{What: "installation 7"}) || err.Error() != "not found: installation 7" {
		t.Errorf("%v", err)
	}
	fake.set(func(f *fakeGitHub) {
		f.answer = func(request) (int, string) { return http.StatusCreated, `{"token":"ghs_x"}` }
	})
	_, err = app.FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if want := "unavailable: installation 7: unexpected answer: missing field `expires_at`"; err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
	fake.set(func(f *fakeGitHub) {
		f.answer = func(request) (int, string) { return http.StatusCreated, `{"token":"ghs_x","expires_at":"soon"}` }
	})
	token, err := app.FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if err != nil || token.ExpiresAt != nowMs+time.Hour.Milliseconds() {
		t.Errorf("an unreadable expiry: %v %v", token, err)
	}
}

func TestAnInvalidCommitIsUnavailable(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(r request) (int, string) {
			if strings.Contains(r.URI, "/git/ref/") {
				return http.StatusOK, `{"object":{"sha":"abc"}}`
			}
			return 0, ""
		}
	})
	_, err := app.Head(context.Background(), 7, repo(t, "acme/shop"), branch(t, "main"))
	var unavailable build.Unavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "not a full 40-character commit SHA") {
		t.Errorf("%v", err)
	}
}

func TestLargeAnswersAreRefused(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(request) (int, string) {
			return http.StatusOK, `{"pad":"` + strings.Repeat("x", github.MaxBody) + `"}`
		}
	})
	_, err := app.FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if want := "unavailable: length limit exceeded"; err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
}

func TestTokensAreRevoked(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNoContent, nil},
		{http.StatusUnauthorized, nil}, // expired or already revoked
		{http.StatusInternalServerError, build.Unavailable{Reason: "revoking a token: HTTP 500 Internal Server Error"}},
		{http.StatusNotFound, build.Unavailable{Reason: "revoking a token: HTTP 404 Not Found"}},
	}
	for _, c := range cases {
		app, fake, _ := newApp(t)
		fake.set(func(f *fakeGitHub) { f.answer = func(request) (int, string) { return c.status, "" } })
		err := app.Revoke(context.Background(), build.FetchToken{Token: "ghs_x"})
		if !errors.Is(err, c.want) && (err != nil || c.want != nil) {
			t.Errorf("%d: %v, want %v", c.status, err, c.want)
		}
		seen := fake.requests()
		if len(seen) != 1 || seen[0].Method != http.MethodDelete || seen[0].URI != "/installation/token" || seen[0].Auth != "Bearer ghs_x" {
			t.Errorf("requests %+v", seen)
		}
	}
}

func TestInstallationsAreReadWithTheAppJWT(t *testing.T) {
	app, fake, pub := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(r request) (int, string) {
			switch r.URI {
			case "/app/installations/7":
				return http.StatusOK, `{"id":7,"account":{"login":"acme"},"suspended_at":null}`
			case "/app/installations/8":
				return http.StatusOK, `{"id":8,"account":{"login":"beta"},"suspended_at":"2026-01-01T00:00:00Z"}`
			}
			return 0, ""
		}
	})
	ctx := context.Background()
	got, err := app.Installation(ctx, 7)
	if err != nil || got != (github.Installation{Account: "acme"}) {
		t.Errorf("%+v %v", got, err)
	}
	got, err = app.Installation(ctx, 8)
	if err != nil || got != (github.Installation{Account: "beta", Suspended: true}) {
		t.Errorf("%+v %v", got, err)
	}
	verifyJWT(t, fake.requests()[0].Auth, pub)
	if _, err := app.Installation(ctx, 9); !errors.Is(err, build.NotFound{What: "installation 9"}) {
		t.Errorf("%v", err)
	}
}

func TestPullRequestStateIsReadAndItsTokenRevoked(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(r request) (int, string) {
			if r.URI == "/repos/acme/shop/pulls/3" {
				return http.StatusOK, `{"number":3,"state":"open"}`
			}
			return 0, ""
		}
	})
	open, err := app.PullRequestOpen(context.Background(), 7, repo(t, "acme/shop"), 3)
	if err != nil || !open {
		t.Errorf("%v %v", open, err)
	}
	seen := fake.requests()
	var got []string
	for _, r := range seen {
		got = append(got, r.Method+" "+r.URI)
	}
	want := []string{"POST /app/installations/7/access_tokens", "GET /repos/acme/shop/pulls/3", "DELETE /installation/token"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(seen[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]any{"pull_requests": "read", "metadata": "read"}, body["permissions"]); diff != "" {
		t.Errorf("permissions (-want +got):\n%s", diff)
	}
	if _, err := app.PullRequestOpen(context.Background(), 7, repo(t, "acme/shop"), 4); !errors.Is(err, build.NotFound{What: "acme/shop#4"}) {
		t.Errorf("%v", err)
	}
}

func TestCommitStatusesArePosted(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.set(func(f *fakeGitHub) {
		f.answer = func(r request) (int, string) {
			if r.URI == "/repos/acme/shop/statuses/"+sha {
				return http.StatusCreated, `{"id":1}`
			}
			return 0, ""
		}
	})
	long := strings.Repeat("é", 150)
	err := app.CommitStatus(context.Background(), 7, repo(t, "acme/shop"), sha, github.CommitStatus{
		State: "success", Context: "kuben/build", Description: long, TargetURL: opt.Some("https://kuben.example/b/1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := fake.requests()
	var token, status map[string]any
	if err := json.Unmarshal([]byte(seen[0].Body), &token); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]any{"statuses": "write"}, token["permissions"]); diff != "" {
		t.Errorf("permissions (-want +got):\n%s", diff)
	}
	if err := json.Unmarshal([]byte(seen[1].Body), &status); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"state": "success", "context": "kuben/build", "description": strings.Repeat("é", 140),
		"target_url": "https://kuben.example/b/1",
	}
	if diff := cmp.Diff(want, status); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
	if seen[1].Auth != "Bearer ghs_1" {
		t.Errorf("auth %q", seen[1].Auth)
	}
}

func TestPlainHTTPIsRefusedUnlessConfigured(t *testing.T) {
	cfg := config.DefaultGitCfg()
	cfg.GithubAPIURL = "HTTP://127.0.0.1:1"
	app := github.NewWithSigner(1, mirror, nil, cfg, clock.Fixed(nowMs))
	err := app.Revoke(context.Background(), build.FetchToken{Token: "t"})
	if want := "unavailable: invalid URL, scheme is not https"; err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
}

func TestAWebhookOnlyAppCannotCallGitHub(t *testing.T) {
	_, err := github.WebhookOnly([]byte("s")).FetchToken(context.Background(), 7, repo(t, "acme/shop"))
	if !errors.Is(err, build.Refused{Reason: "this App has no private key"}) {
		t.Errorf("%v", err)
	}
}

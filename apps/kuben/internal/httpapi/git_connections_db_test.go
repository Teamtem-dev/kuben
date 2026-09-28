package httpapi_test

// Git connections end to end on PostgreSQL, with an in-memory GitLab: a
// token is checked before it is saved, the webhook secret is shown once and
// rotated, pushes are verified and deduplicated.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// fakeGitLab answers the token "glpat-x" as alice with the api scope.
func fakeGitLab(t *testing.T) http.RoundTripper {
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
	mux.HandleFunc("GET /api/v4/user", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"username":"alice"}`))
	}))
	mux.HandleFunc("GET /api/v4/personal_access_tokens/self", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"scopes":["api"]}`))
	}))
	mux.HandleFunc("GET /api/v4/projects", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":42,"path_with_namespace":"acme/shop","visibility":"private","default_branch":"main"}]`))
	}))
	mux.HandleFunc("GET /api/v4/projects/{project}/repository/branches", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"main","default":true,"protected":true,"commit":{"id":"` + pushSha + `"}}]`))
	}))
	srv := httptest.NewTestServer(t, mux)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	return srv.Client().Transport
}

func newGitServer(t *testing.T) *client {
	t.Helper()
	st := pgtest.Store(t)
	cfg := config.Default()
	cfg.Server.Bind = "127.0.0.1:3000"
	h := health.New(clock.System{})
	h.SetReady(true)
	server, err := httpapi.New(httpapi.Deps{
		Config: cfg, Store: st, Hasher: auth.InsecureForTests(), Health: h,
		Keyring: opt.Some(testKeyring()), GitTransport: fakeGitLab(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}
}

// deliver sends a GitLab push to the connection's webhook.
func (c *client) deliver(t *testing.T, path, token, id string) (int, string) {
	t.Helper()
	body := `{"object_kind":"push","ref":"refs/heads/main","after":"` + pushSha + `","project":{"path_with_namespace":"acme/shop"}}`
	req, err := http.NewRequest("POST", c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Gitlab-Token", token)
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Event-UUID", id)
	resp, err := http.DefaultClient.Do(req) // no session
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var answer struct {
		Outcome string `json:"outcome"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&answer) //nolint:errcheck // an empty outcome fails the caller's check
	return resp.StatusCode, answer.Outcome
}

func TestGitConnectionsAreCheckedSealedAndHooked(t *testing.T) {
	c := newGitServer(t)
	c.setup(t)
	create := map[string]any{"provider": "gitlab", "name": "gitlab-acme", "baseUrl": "https://gitlab.example.com", "token": "glpat-x"}
	status, created, _ := c.do("POST", "/api/v1/git/connections", create)
	secret, _ := created["webhookSecret"].(string)
	if status != 201 || secret == "" || created["hasToken"] != true || created["username"] != "alice" ||
		created["tokenHint"] != "" || created["lastCheckedAt"] == nil {
		t.Fatalf("create: %d %v", status, created)
	}
	id, _ := created["id"].(string)
	hook, _ := created["webhookUrl"].(string)
	if hook != "/api/v1/webhooks/gitlab/"+id {
		t.Fatalf("webhook URL %q", hook)
	}
	wrong := map[string]any{"provider": "gitlab", "name": "other", "baseUrl": "https://gitlab.example.com", "token": "nope"}
	if status, body, _ := c.do("POST", "/api/v1/git/connections", wrong); status != 422 {
		t.Errorf("a refused token is not saved: %d %v", status, body)
	}
	if status, body, _ := c.do("POST", "/api/v1/git/connections", create); status != 409 {
		t.Errorf("a taken name: %d %v", status, body)
	}
	if status, body, _ := c.do("POST", "/api/v1/git/connections/test", wrong); status != 200 || body["ok"] != false {
		t.Errorf("a test of a refused token: %d %v", status, body)
	}

	status, got, _ := c.do("GET", "/api/v1/git/connections/"+id, nil)
	if status != 200 || got["webhookSecret"] != nil || got["name"] != "gitlab-acme" {
		t.Errorf("get: %d %v", status, got)
	}
	if status, body, _ := c.do("POST", "/api/v1/git/connections/"+id+"/test", nil); status != 200 || body["ok"] != true {
		t.Errorf("test: %d %v", status, body)
	}
	if status, body, _ := c.do("GET", "/api/v1/git/connections/"+id+"/repositories?search=shop", nil); status != 200 ||
		body["page"] != float64(1) {
		t.Errorf("repositories: %d %v", status, body)
	}
	resp := c.get("/api/v1/git/connections/" + id + "/branches?repository=acme/shop")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("branches: %d", resp.StatusCode)
	}

	if status, outcome := c.deliver(t, hook, secret, "e-1"); status != 202 || outcome != "noBinding" {
		t.Errorf("a push: %d %s", status, outcome)
	}
	if status, outcome := c.deliver(t, hook, secret, "e-1"); status != 200 || outcome != "duplicate" {
		t.Errorf("the same push again: %d %s", status, outcome)
	}
	if status, outcome := c.deliver(t, hook, "guess", "e-2"); status != 401 || outcome != "badSignature" {
		t.Errorf("a wrong secret: %d %s", status, outcome)
	}

	status, rotated, _ := c.do("POST", "/api/v1/git/connections/"+id+"/webhook-secret", nil)
	fresh, _ := rotated["webhookSecret"].(string)
	if status != 200 || fresh == "" || fresh == secret || rotated["webhookUrl"] != hook {
		t.Fatalf("rotate: %d %v", status, rotated)
	}
	if status, _ := c.deliver(t, hook, secret, "e-3"); status != 401 {
		t.Errorf("the old secret still works: %d", status)
	}
	if status, _ := c.deliver(t, hook, fresh, "e-3"); status != 202 {
		t.Errorf("the new secret: %d", status)
	}

	if status, body, _ := c.do("PATCH", "/api/v1/git/connections/"+id, map[string]any{"token": "nope"}); status != 422 {
		t.Errorf("a refused new token: %d %v", status, body)
	}
	if status, body, _ := c.do("PATCH", "/api/v1/git/connections/"+id, map[string]any{"defaultBranch": "main"}); status != 200 ||
		body["defaultBranch"] != "main" {
		t.Errorf("update: %d %v", status, body)
	}
	if status, _, _ := c.do("DELETE", "/api/v1/git/connections/"+id, nil); status != 204 {
		t.Errorf("delete: %d", status)
	}
	if status, _, _ := c.do("GET", "/api/v1/git/connections/"+id, nil); status != 404 {
		t.Errorf("deleted: %d", status)
	}
	if status, outcome := c.deliver(t, hook, fresh, "e-4"); status != 404 || outcome != "unknownConnection" {
		t.Errorf("a push to a deleted connection: %d %s", status, outcome)
	}
}

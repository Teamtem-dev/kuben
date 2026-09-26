package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/web"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// previewHead is the head every pull request of the scenario points at.
const previewHead = "89abcdef0123456789abcdef0123456789abcdef"

// newGitFixture is newFixture (tests/http.rs setup: alice an owner, bob a
// viewer, project shop with environment prod) on a server whose GitHub App
// verifies deliveries with webhookSecret.
func newGitFixture(t *testing.T) fixture {
	t.Helper()
	ctx := t.Context()
	st := pgtest.Store(t)
	p := projection.New()
	cfg := config.Default()
	cfg.Server.Bind = "127.0.0.1:3000" // loopback: no setup token needed
	h := health.New(clock.System{})
	h.SetReady(true)
	server, err := httpapi.New(httpapi.Deps{
		Config:      cfg,
		Store:       st,
		Hasher:      auth.InsecureForTests(),
		Health:      h,
		Projections: p,
		Images:      privateImages{testImages(t)},
		Keyring:     opt.Some(testKeyring()),
		SSO:         testSSO(t, cfg),
		Console:     web.NewFS(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>console")}}),
		GitHub:      opt.Some(github.WebhookOnly([]byte(webhookSecret))),
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
	org, err := st.CreateOrg(ctx, "acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t: t, c: &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}, store: st, org: org.ID, projections: p}
	f.user("alice@example.com", opt.Some("Alice"), perm.Owner, false)
	f.user("bob@example.com", opt.None[string](), perm.Viewer, false)
	tn, err := st.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	project, err := tn.CreateProject(ctx, "shop", "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.CreateEnvironment(ctx, project, "prod", "prod", false); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// gitApp is tests/http.rs git_app: the test app, built from GitHub through
// a linked installation, with a secret reference and a custom domain.
func (f fixture) gitApp() {
	t := f.t
	ctx := t.Context()
	project, _, tgt := f.sqlApp()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if linked, err := tn.LinkInstallation(ctx, 77, "acme"); err != nil || !linked {
		t.Fatalf("link: %v %v", linked, err)
	}
	config := jsonValue(t, `{"runtime":{"processes":{"web":{"port":8080}}},`+
		`"env":[{"name":"DB","fromSecret":{"name":"db","key":"url"}}],"domains":[{"host":"shop.example.com"}]}`)
	if _, ok, err := tn.CreateConfigRevision(ctx, project, tgt, config, "user:test"); err != nil || !ok {
		t.Fatalf("config: %v %v", ok, err)
	}
	repo, err := source.ParseRepoName("acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := source.ParseBranchName("main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.BindSource(ctx, project, tgt, store.NewBinding{
		InstallationID: 77, Repository: repo, Branch: branch, Recipe: source.BuildRecipe{Strategy: source.Auto},
		ImageRepository: "registry.local/acme/shop",
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// githubDelivery POSTs a signed GitHub delivery of event.
func (f fixture) githubDelivery(event string, body map[string]any) (int, map[string]any) {
	t := f.t
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", f.c.base+httpapi.GithubWebhookPath, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signature(data))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", uuid.Must(uuid.NewV7()).String())
	resp := f.c.send(req)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// pull is a pull_request delivery of acme/shop through installation 77.
func pull(action string, number int, headRepo, updatedAt string) map[string]any {
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	return map[string]any{
		"action": action,
		"number": number,
		"pull_request": map[string]any{
			"number":     number,
			"state":      state,
			"updated_at": updatedAt,
			"head":       map[string]any{"sha": previewHead, "ref": "feature", "repo": map[string]any{"full_name": headRepo}},
		},
		"repository":   map[string]any{"id": 42, "full_name": "acme/shop"},
		"installation": map[string]any{"id": 77},
	}
}

const (
	previewPolicyURL = "/api/v1/projects/shop/previews/policy"
	previewsURL      = "/api/v1/projects/shop/previews"
)

// tests/http.rs m5_previews_follow_pull_requests: pull requests open,
// follow, close and reopen previews; forks need the project's consent and
// never get secrets.
func TestM5PreviewsFollowPullRequests(t *testing.T) {
	f := newGitFixture(t)
	f.gitApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	settings := map[string]any{"enabled": true, "sourceEnvironment": "prod", "ttlHours": 2, "maxActive": 5}
	if status := bob.status("PUT", previewPolicyURL, settings); status != http.StatusForbidden {
		t.Fatalf("a viewer set the preview settings: %d", status)
	}
	if status, saved, _ := alice.do("PUT", previewPolicyURL, settings); status != http.StatusOK || saved["sourceEnvironment"] != "prod" {
		t.Fatalf("settings: %d %v", status, saved)
	}

	if status, answer := f.githubDelivery("pull_request", pull("opened", 12, "acme/shop", "2026-09-17T10:00:00Z")); status != http.StatusAccepted {
		t.Fatalf("opened: %d %v", status, answer)
	}
	_, listed := alice.list(previewsURL)
	if len(listed) == 0 || listed[0]["environment"] != "pr12-1" {
		t.Fatalf("a preview: %v", listed)
	}
	if listed[0]["trusted"] != true || listed[0]["state"] != "active" {
		t.Fatalf("trusted and active: %v", listed[0])
	}
	if remaining, ok := listed[0]["remainingSeconds"].(float64); !ok || remaining <= 7000 {
		t.Fatalf("two hours to live: %v", listed[0]["remainingSeconds"])
	}
	if _, apps := alice.list("/api/v1/projects/shop/environments/pr12-1/apps"); len(apps) != 1 {
		t.Fatalf("the app is copied: %v", apps)
	}
	_, copied, _ := alice.do("GET", "/api/v1/projects/shop/environments/pr12-1/apps/api", nil)
	app, _ := copied["app"].(map[string]any)
	if env, ok := app["env"].([]any); !ok || len(env) != 0 {
		t.Fatalf("no secret references: %v", copied)
	}
	if domains, ok := app["domains"].([]any); !ok || len(domains) != 0 {
		t.Fatalf("no custom domains: %v", copied)
	}

	if _, answer := f.githubDelivery("pull_request", pull("synchronize", 12, "acme/shop", "2026-09-17T09:00:00Z")); answer["outcome"] != "previews" {
		t.Fatalf("stale events are accepted but change nothing: %v", answer)
	}
	if _, answer := f.githubDelivery("pull_request", pull("opened", 13, "mallory/shop", "2026-09-17T10:00:00Z")); answer["outcome"] != "previews" {
		t.Fatalf("a fork's pull request: %v", answer)
	}
	if _, listed := alice.list(previewsURL); len(listed) != 1 {
		t.Fatalf("forks need consent: %v", listed)
	}

	f.closeAndReopen(alice)
	f.forksAndManualActions(alice)
}

// closeAndReopen: closing ends a preview; a late event does not revive it;
// reopening makes a new one.
func (f fixture) closeAndReopen(alice *client) {
	t := f.t
	f.githubDelivery("pull_request", pull("closed", 12, "acme/shop", "2026-09-17T11:00:00Z"))
	_, listed := alice.list(previewsURL + "?all=true")
	if len(listed) == 0 || listed[0]["state"] != "closed" || listed[0]["closeReason"] != "closed" {
		t.Fatalf("closed: %v", listed)
	}
	f.githubDelivery("pull_request", pull("synchronize", 12, "acme/shop", "2026-09-17T10:30:00Z"))
	if status, active := alice.list(previewsURL); status != http.StatusOK || len(active) != 0 {
		t.Fatalf("a late event does not bring it back: %d %v", status, active)
	}
	f.githubDelivery("pull_request", pull("reopened", 12, "acme/shop", "2026-09-17T12:00:00Z"))
	if _, active := alice.list(previewsURL); len(active) == 0 || active[0]["environment"] != "pr12-2" {
		t.Fatalf("a new epoch: %v", active)
	}
}

func (f fixture) forksAndManualActions(alice *client) {
	t := f.t
	alice.do("PUT", previewPolicyURL, map[string]any{"enabled": true, "sourceEnvironment": "prod", "allowForks": true})
	f.githubDelivery("pull_request", pull("opened", 13, "mallory/shop", "2026-09-17T10:00:00Z"))
	_, active := alice.list(previewsURL)
	var fork map[string]any
	for _, p := range active {
		if p["pullRequest"] == 13.0 {
			fork = p
		}
	}
	if fork == nil || fork["environment"] != "pr13-1" || fork["trusted"] != false {
		t.Fatalf("the fork's untrusted preview: %v", active)
	}
	secret := "/api/v1/projects/shop/environments/pr13-1/secrets/db"
	if status, body, _ := alice.do("PUT", secret, map[string]any{"data": map[string]any{"url": "x"}}); status != http.StatusConflict {
		t.Fatalf("no secrets in a fork's preview: %d %v", status, body)
	}

	extend := previewsURL + "/pr13-1/extend"
	status, extended, _ := alice.do("POST", extend, map[string]any{"hours": 5, "keep": true})
	if status != http.StatusOK || extended["autoDelete"] != false {
		t.Fatalf("extend: %d %v", status, extended)
	}
	before, _ := fork["remainingSeconds"].(float64)
	if after, _ := extended["remainingSeconds"].(float64); after <= before {
		t.Fatalf("more time: %v, before %v", after, before)
	}
	if status := alice.status("POST", extend, map[string]any{"hours": 0}); status != http.StatusUnprocessableEntity {
		t.Fatalf("no hours: %d", status)
	}
	destroy := previewsURL + "/pr13-1"
	if status := alice.status("DELETE", destroy, nil); status != http.StatusAccepted {
		t.Fatalf("destroy: %d", status)
	}
	if status := alice.status("DELETE", destroy, nil); status != http.StatusNotFound {
		t.Fatalf("destroyed once: %d", status)
	}
	_, all := alice.list(previewsURL + "?all=true")
	var closed map[string]any
	for _, p := range all {
		if p["environment"] == "pr13-1" {
			closed = p
		}
	}
	if closed == nil || closed["closeReason"] != "manual" {
		t.Fatalf("closed by hand: %v", all)
	}
}

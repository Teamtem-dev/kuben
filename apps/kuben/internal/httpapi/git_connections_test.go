package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitea"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const pushSha = "0123456789abcdef0123456789abcdef01234567"

// bareServer is the API without a database or keyring.
func bareServer(t *testing.T, edit func(*config.Config)) *httpapi.Server {
	t.Helper()
	cfg := config.Default()
	if edit != nil {
		edit(&cfg)
	}
	server, err := httpapi.New(httpapi.Deps{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func connection(provider store.GitProvider) store.GitConnection {
	return store.GitConnection{
		ID: ids.New[ids.GitConnection](), Org: ids.New[ids.Org](), Provider: provider, Name: "acme",
		BaseURL: "https://gitlab.example.com", AuthKind: store.GitAuthToken, TokenHint: "wxyz",
		Token:         store.SealedBytes{Ciphertext: []byte("sealed-token"), WrappedKey: []byte("k"), KeyVersion: 1},
		WebhookSecret: store.SealedBytes{Ciphertext: []byte("sealed-secret"), WrappedKey: []byte("k"), KeyVersion: 1},
		Username:      opt.Some("alice"), CreatedAt: 1, UpdatedAt: 2,
	}
}

func TestAConnectionNeverShowsItsSecrets(t *testing.T) {
	c := connection(store.GitProviderGitLab)
	dto := httpapi.GitConnectionDtoOf(bareServer(t, nil), c)
	raw, err := dto.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"sealed-token", "sealed-secret", "webhookSecret", "c2VhbGVk"} {
		if strings.Contains(text, secret) {
			t.Errorf("the DTO shows %s: %s", secret, text)
		}
	}
	if !dto.HasToken || dto.TokenHint != "wxyz" || dto.Username.Or("") != "alice" ||
		dto.WebhookUrl.Or("") != httpapi.GitlabWebhookPrefix+c.ID.String() {
		t.Errorf("dto %+v", dto)
	}
	public := bareServer(t, func(cfg *config.Config) { cfg.Server.PublicURL = opt.Some("https://kuben.example.com/") })
	gc := connection(store.GitProviderGitea)
	if got := httpapi.GitConnectionDtoOf(public, gc).WebhookUrl.Or(""); got != "https://kuben.example.com"+httpapi.GiteaWebhookPrefix+gc.ID.String() {
		t.Errorf("public webhook URL %q", got)
	}
	gh := connection(store.GitProviderGitHub)
	if got := httpapi.GitConnectionDtoOf(public, gh).WebhookUrl.Or(""); got != "https://kuben.example.com"+httpapi.GithubWebhookPrefix+gh.ID.String() {
		t.Errorf("a GitHub token connection's webhook URL %q", got)
	}
}

func TestGithubConnectionDeliveriesAreSigned(t *testing.T) {
	s := bareServer(t, nil)
	conn := connection(store.GitProviderGitHub)
	secret := []byte("hook-secret")
	push := []byte(`{"ref":"refs/heads/main","after":"` + pushSha + `","deleted":false,
		"repository":{"id":9,"full_name":"Acme/Shop"}}`)
	sign := func(key, body []byte) string {
		mac := hmac.New(sha256.New, key)
		mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	req := func(body []byte, headers ...string) *http.Request {
		r := httptest.NewRequest("POST", httpapi.GithubWebhookPrefix+conn.ID.String(), bytes.NewReader(body))
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		return r
	}
	good := sign(secret, push)
	for name, r := range map[string]*http.Request{
		"unsigned":         req(push, "X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1"),
		"signed otherwise": req(push, "X-Hub-Signature-256", sign([]byte("other"), push), "X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1"),
		"sha1 only":        req(push, "X-Hub-Signature", "sha1=00", "X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1"),
	} {
		w := httptest.NewRecorder()
		if _, _, ok := httpapi.ReadConnectionDelivery(s, w, r, push, conn, secret); ok || w.Code != http.StatusUnauthorized ||
			outcomeOf(t, w) != "badSignature" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	id, got, ok := httpapi.ReadConnectionDelivery(s, httptest.NewRecorder(),
		req(push, "X-Hub-Signature-256", good, "X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1"), push, conn, secret)
	if p, isPush := got.Get(); !ok || id != "d-1" || !isPush || p.Repository.String() != "acme/shop" ||
		p.Branch.String() != "main" || p.After != pushSha || p.Deleted {
		t.Errorf("a push: %v %s %+v", ok, id, got)
	}
	ping := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)
	w := httptest.NewRecorder()
	if _, _, ok := httpapi.ReadConnectionDelivery(s, w,
		req(ping, "X-Hub-Signature-256", sign(secret, ping), "X-GitHub-Event", "ping", "X-GitHub-Delivery", "d-2"),
		ping, conn, secret); ok || w.Code != http.StatusOK || outcomeOf(t, w) != "pong" {
		t.Errorf("a ping: %d %s", w.Code, w.Body.String())
	}
	issue := []byte(`{"action":"opened"}`)
	if _, got, ok := httpapi.ReadConnectionDelivery(s, httptest.NewRecorder(),
		req(issue, "X-Hub-Signature-256", sign(secret, issue), "X-GitHub-Event", "issues", "X-GitHub-Delivery", "d-3"),
		issue, conn, secret); !ok || got.IsSome() {
		t.Errorf("another event is ignored: %v %+v", ok, got)
	}
	for name, r := range map[string]*http.Request{
		"no event":    req(push, "X-Hub-Signature-256", good, "X-GitHub-Delivery", "d-1"),
		"no delivery": req(push, "X-Hub-Signature-256", good, "X-GitHub-Event", "push"),
	} {
		w := httptest.NewRecorder()
		if _, _, ok := httpapi.ReadConnectionDelivery(s, w, r, push, conn, secret); ok || w.Code != http.StatusBadRequest ||
			outcomeOf(t, w) != "missingHeaders" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	bad := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"a/b/c"}}`)
	w = httptest.NewRecorder()
	if _, _, ok := httpapi.ReadConnectionDelivery(s, w,
		req(bad, "X-Hub-Signature-256", sign(secret, bad), "X-GitHub-Event", "push", "X-GitHub-Delivery", "d-4"),
		bad, conn, secret); ok || w.Code != http.StatusBadRequest || outcomeOf(t, w) != "malformed" {
		t.Errorf("a malformed push: %d %s", w.Code, w.Body.String())
	}
}

func TestNewConnectionsAreValidated(t *testing.T) {
	body := func(provider gen.GitProviderDto, name, base, token string) *gen.CreateGitConnection {
		b := &gen.CreateGitConnection{Provider: provider, Name: name, Token: token}
		if base != "" {
			b.BaseUrl = gen.NewOptString(base)
		}
		return b
	}
	provider, base, err := httpapi.ReadNewConnection(body(gen.GitProviderDtoGitlab, "gitlab-acme", "", "glpat-x"), true)
	if err != nil || provider != store.GitProviderGitLab || base != gitprovider.GitLabURL {
		t.Errorf("gitlab.com: %s %s %v", provider, base, err)
	}
	refused := map[string]*gen.CreateGitConnection{
		"a bad name":              body(gen.GitProviderDtoGitlab, "Acme GitLab", "", "glpat-x"),
		"Gitea without its URL":   body(gen.GitProviderDtoGitea, "gitea", "", "t0k"),
		"a URL with credentials":  body(gen.GitProviderDtoGitea, "gitea", "https://u:p@git.example.com", "t0k"),
		"an empty token":          body(gen.GitProviderDtoGitlab, "gitlab", "", ""),
		"a token with a space":    body(gen.GitProviderDtoGitlab, "gitlab", "", "glpat x"),
		"a token with a newline":  body(gen.GitProviderDtoGitlab, "gitlab", "", "glpat\nx"),
		"a token of another text": body(gen.GitProviderDtoGitlab, "gitlab", "", "glpat-é"),
	}
	branch := body(gen.GitProviderDtoGitlab, "gitlab", "", "glpat-x")
	branch.DefaultBranch = gen.NewOptNilString("a..b")
	refused["a bad default branch"] = branch
	for name, b := range refused {
		_, _, err := httpapi.ReadNewConnection(b, true)
		var e *kerrors.Error
		if !errors.As(err, &e) || e.Code != kerrors.Validation {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A test of a token needs no valid name.
	if _, _, err := httpapi.ReadNewConnection(body(gen.GitProviderDtoGitea, "", "https://git.example.com", "t0k"), false); err != nil {
		t.Errorf("unnamed: %v", err)
	}
	if httpapi.TokenHint("glpat-abcdefgh1234") != "1234" || httpapi.TokenHint("short") != "" {
		t.Error("the hint is the last four characters of a long token only")
	}
	if httpapi.CheckGitToken(strings.Repeat("x", 4097)) == nil {
		t.Error("a token of 4097 characters")
	}
}

func TestChecksSayWhatTheProviderSaid(t *testing.T) {
	ok := httpapi.CheckDtoOf(gitprovider.Account{Username: "alice", Scopes: []string{"api"}, Missing: []string{}}, nil, 7)
	if !ok.Ok || ok.Username.Or("") != "alice" || !ok.Error.IsNull() || ok.CheckedAt != 7 || len(ok.Scopes) != 1 {
		t.Errorf("ok %+v", ok)
	}
	short := httpapi.CheckDtoOf(gitprovider.Account{
		Username: "alice", Scopes: []string{"read_user"},
		Missing: []string{"read_api", "read_repository"},
	}, nil, 7)
	if short.Ok || short.Error.Or("") != "the token lacks the scopes read_api, read_repository" || len(short.MissingScopes) != 2 {
		t.Errorf("short %+v", short)
	}
	refused := httpapi.CheckDtoOf(gitprovider.Account{}, build.Refused{Reason: "401 Unauthorized"}, 7)
	if refused.Ok || refused.Error.Or("") != "the Git provider refused the token: 401 Unauthorized" ||
		refused.Scopes == nil || refused.MissingScopes == nil {
		t.Errorf("refused %+v", refused)
	}
	codes := map[error]kerrors.Code{
		build.NotFound{What: "acme/shop"}:      kerrors.NotFound,
		build.Refused{Reason: "401"}:           kerrors.Validation,
		build.Unavailable{Reason: "timed out"}: kerrors.Unavailable,
	}
	for err, want := range codes {
		var e *kerrors.Error
		if !errors.As(httpapi.GitProviderError(err), &e) || e.Code != want {
			t.Errorf("%v: %v", err, httpapi.GitProviderError(err))
		}
	}
}

// outcomeOf is the webhook answer's outcome.
func outcomeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var answer struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer %q: %v", w.Body.String(), err)
	}
	return answer.Outcome
}

func TestConnectionWebhooksAreRoutedOutsideTheSession(t *testing.T) {
	h := bareServer(t, nil).Handler()
	id := ids.New[ids.GitConnection]().String()
	cases := []struct {
		name    string
		method  string
		path    string
		status  int
		outcome string
	}{
		{"GET", "GET", httpapi.GitlabWebhookPrefix + id, http.StatusMethodNotAllowed, ""},
		{"not an id", "POST", httpapi.GitlabWebhookPrefix + "nope", http.StatusNotFound, "unknownConnection"},
		{"no keyring", "POST", httpapi.GiteaWebhookPrefix + id, http.StatusServiceUnavailable, "notConfigured"},
		{"GitHub, GET", "GET", httpapi.GithubWebhookPrefix + id, http.StatusMethodNotAllowed, ""},
		{"GitHub, no keyring", "POST", httpapi.GithubWebhookPrefix + id, http.StatusServiceUnavailable, "notConfigured"},
		// The App's webhook stays where it was; without an App it is 404.
		{"the GitHub App", "POST", httpapi.GithubWebhookPath, http.StatusNotFound, "notConfigured"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		// No cookie and no console header: CSRF does not apply.
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`)))
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
			continue
		}
		if c.outcome != "" && outcomeOf(t, w) != c.outcome {
			t.Errorf("%s: %s", c.name, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	big := bytes.Repeat([]byte("x"), 5<<20+1)
	h.ServeHTTP(w, httptest.NewRequest("POST", httpapi.GitlabWebhookPrefix+id, bytes.NewReader(big)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the limit: %d", w.Code)
	}
}

func TestGitlabDeliveriesCarryTheSecret(t *testing.T) {
	s := bareServer(t, nil)
	conn := connection(store.GitProviderGitLab)
	secret := []byte("hook-secret")
	push := []byte(`{"object_kind":"push","ref":"refs/heads/main","after":"` + pushSha + `",
		"project":{"path_with_namespace":"acme/shop"}}`)
	req := func(body []byte, headers ...string) *http.Request {
		r := httptest.NewRequest("POST", httpapi.GitlabWebhookPrefix+conn.ID.String(), bytes.NewReader(body))
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		return r
	}
	refusals := []struct {
		name    string
		req     *http.Request
		status  int
		outcome string
	}{
		{"no token", req(push, "X-Gitlab-Event", "Push Hook", "X-Gitlab-Event-UUID", "e-1"), 401, "badSignature"},
		{"a wrong token", req(push, "X-Gitlab-Token", "hook-secreT", "X-Gitlab-Event", "Push Hook",
			"X-Gitlab-Event-UUID", "e-1"), 401, "badSignature"},
		{"no event", req(push, "X-Gitlab-Token", "hook-secret", "X-Gitlab-Event-UUID", "e-1"), 400, "missingHeaders"},
		{"no delivery id", req(push, "X-Gitlab-Token", "hook-secret", "X-Gitlab-Event", "Push Hook"), 400, "missingHeaders"},
		{"a long delivery id", req(push, "X-Gitlab-Token", "hook-secret", "X-Gitlab-Event", "Push Hook",
			"X-Gitlab-Event-UUID", strings.Repeat("e", 129)), 400, "badDeliveryId"},
		{"malformed", req([]byte(`{`), "X-Gitlab-Token", "hook-secret", "X-Gitlab-Event", "Push Hook",
			"X-Gitlab-Event-UUID", "e-1"), 400, "malformed"},
	}
	for _, c := range refusals {
		w := httptest.NewRecorder()
		body := push
		if c.name == "malformed" {
			body = []byte(`{`)
		}
		if _, _, ok := httpapi.ReadConnectionDelivery(s, w, c.req, body, conn, secret); ok {
			t.Errorf("%s was accepted", c.name)
			continue
		}
		if w.Code != c.status || outcomeOf(t, w) != c.outcome {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	id, got, ok := httpapi.ReadConnectionDelivery(s, w, req(push, "X-Gitlab-Token", "hook-secret",
		"X-Gitlab-Event", "Push Hook", "X-Gitlab-Event-UUID", "e-1", "Idempotency-Key", "k-1"), push, conn, secret)
	p, isPush := got.Get()
	if !ok || id != "k-1" || !isPush || p.Repository.String() != "acme/shop" || p.Branch.String() != "main" {
		t.Errorf("a push: %v %s %+v", ok, id, got)
	}
	_, got, ok = httpapi.ReadConnectionDelivery(s, httptest.NewRecorder(), req(push, "X-Gitlab-Token", "hook-secret",
		"X-Gitlab-Event", "Note Hook", "X-Gitlab-Event-UUID", "e-2"), push, conn, secret)
	if !ok || got.IsSome() {
		t.Errorf("another event is read and ignored: %v %+v", ok, got)
	}
}

func TestGiteaAndForgejoDeliveriesAreSigned(t *testing.T) {
	s := bareServer(t, nil)
	conn := connection(store.GitProviderGitea)
	secret := []byte("hook-secret")
	push := []byte(`{"ref":"refs/heads/main","after":"` + pushSha + `","repository":{"full_name":"acme/shop"}}`)
	req := func(headers ...string) *http.Request {
		r := httptest.NewRequest("POST", httpapi.GiteaWebhookPrefix+conn.ID.String(), bytes.NewReader(push))
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		return r
	}
	good := gitea.Sign(secret, push)
	for name, r := range map[string]*http.Request{
		"unsigned":         req("X-Gitea-Event", "push", "X-Gitea-Delivery", "d-1"),
		"signed otherwise": req("X-Gitea-Signature", gitea.Sign([]byte("other"), push), "X-Gitea-Event", "push", "X-Gitea-Delivery", "d-1"),
	} {
		w := httptest.NewRecorder()
		if _, _, ok := httpapi.ReadConnectionDelivery(s, w, r, push, conn, secret); ok || w.Code != 401 ||
			outcomeOf(t, w) != "badSignature" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	for name, r := range map[string]*http.Request{
		"Gitea":   req("X-Gitea-Signature", good, "X-Gitea-Event", "push", "X-Gitea-Delivery", "d-1"),
		"Forgejo": req("X-Forgejo-Signature", good, "X-Forgejo-Event", "push", "X-Forgejo-Delivery", "d-1"),
	} {
		id, got, ok := httpapi.ReadConnectionDelivery(s, httptest.NewRecorder(), r, push, conn, secret)
		if p, isPush := got.Get(); !ok || id != "d-1" || !isPush || p.Repository.String() != "acme/shop" {
			t.Errorf("%s: %v %s %+v", name, ok, id, got)
		}
	}
	w := httptest.NewRecorder()
	if _, _, ok := httpapi.ReadConnectionDelivery(s, w, req("X-Gitea-Signature", good, "X-Gitea-Event", "push"),
		push, conn, secret); ok || w.Code != 400 {
		t.Errorf("no delivery id: %d", w.Code)
	}
}

package source_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
)

func body(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pushPayload(gitRef string) map[string]any {
	return map[string]any{
		"ref": gitRef, "before": strings.Repeat("0", 40), "after": sha, "forced": true, "deleted": false,
		"repository":   map[string]any{"id": 42, "full_name": "Acme/Shop"},
		"installation": map[string]any{"id": 7},
	}
}

func pullPayload(action string, headRepo any) map[string]any {
	state := "open"
	if action == "closed" {
		state = "closed"
	}
	return map[string]any{
		"action": action,
		"number": 12,
		"pull_request": map[string]any{
			"number": 12, "state": state, "draft": false, "updated_at": "2026-09-17T10:00:00Z",
			"head": map[string]any{"sha": sha, "ref": "feature/x", "repo": headRepo},
			"base": map[string]any{"ref": "main"},
		},
		"repository":   map[string]any{"id": 42, "full_name": "Acme/Shop"},
		"installation": map[string]any{"id": 7},
	}
}

func without(payload map[string]any, key string) map[string]any {
	delete(payload, key)
	return payload
}

func with(payload map[string]any, key string, v any) map[string]any {
	payload[key] = v
	return payload
}

func mustCommit(t *testing.T) source.CommitSha {
	t.Helper()
	c, err := source.ParseCommitSha(sha)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustRepo(t *testing.T, s string) source.RepoName {
	t.Helper()
	r, err := source.ParseRepoName(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPushesToBranchesAreParsed(t *testing.T) {
	event, err := source.ParseGitHub("push", body(t, pushPayload("refs/heads/main")))
	if err != nil {
		t.Fatal(err)
	}
	branch, _ := source.BranchFromRef("refs/heads/main")
	want := source.PushEvent{
		InstallationID: 7,
		RepositoryID:   42,
		Repository:     mustRepo(t, "acme/shop"),
		Branch:         branch,
		After:          mustCommit(t),
		Forced:         true,
	}
	if diff := cmp.Diff(source.WebhookEvent(want), event, cmpValues); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestTagsAndUnknownEventsAreIgnored(t *testing.T) {
	cases := []struct {
		event string
		body  []byte
		want  source.WebhookEvent
	}{
		{"push", body(t, pushPayload("refs/tags/v1")), source.Ignored{}},
		{"push", body(t, pushPayload("refs/heads/a..b")), source.Ignored{}},
		{"push", body(t, without(pushPayload("refs/tags/v1"), "installation")), source.Ignored{}},
		{"issues", []byte("{}"), source.Ignored{}},
		{"issues", []byte("not json"), source.Ignored{}},
		{"ping", []byte("{}"), source.Ping{}},
		{"ping", nil, source.Ping{}},
		{"installation", []byte(`{"action":"new_permissions_accepted","installation":{"id":9}}`), source.Ignored{}},
	}
	for _, c := range cases {
		got, err := source.ParseGitHub(c.event, c.body)
		if err != nil || got != c.want {
			t.Errorf("%s %s: got %v, %v", c.event, c.body, got, err)
		}
	}
}

func TestMalformedPayloadsAreRefused(t *testing.T) {
	const malformed = "the webhook payload is malformed: "
	main := func() map[string]any { return pushPayload("refs/heads/main") }
	cases := []struct {
		name  string
		event string
		body  []byte
		kind  source.InvalidKind
		want  string
	}{
		{"not json", "push", []byte("not json"), source.InvalidPayload, ""},
		{"an array", "push", []byte("[]"), source.InvalidPayload, ""},
		{"no ref", "push", body(t, without(main(), "ref")), source.InvalidPayload, malformed + "missing field `ref`"},
		// serde reads the whole payload before the ref is looked at.
		{
			"a tag without after", "push", body(t, without(pushPayload("refs/tags/v1"), "after")),
			source.InvalidPayload, malformed + "missing field `after`",
		},
		{
			"no repository", "push", body(t, without(main(), "repository")),
			source.InvalidPayload, malformed + "missing field `repository`",
		},
		{
			"no repository id", "push", body(t, with(main(), "repository", map[string]any{"full_name": "a/b"})),
			source.InvalidPayload, malformed + "missing field `id`",
		},
		{
			"a negative id", "push", body(t, with(main(), "repository", map[string]any{"id": -1, "full_name": "a/b"})),
			source.InvalidPayload, "",
		},
		{"forced is no bool", "push", body(t, with(main(), "forced", "yes")), source.InvalidPayload, ""},
		{
			"no installation", "push", body(t, without(main(), "installation")),
			source.InvalidPayload, malformed + "a push without an installation",
		},
		{
			"a null installation", "push", body(t, with(main(), "installation", nil)),
			source.InvalidPayload, malformed + "a push without an installation",
		},
		{
			"an installation without id", "push", body(t, with(main(), "installation", map[string]any{})),
			source.InvalidPayload, malformed + "missing field `id`",
		},
		{
			"a bad repository", "push", body(t, with(main(), "repository", map[string]any{"id": 1, "full_name": "acme"})),
			source.InvalidRepository, "not an `owner/name` repository: \"acme\"",
		},
		{
			"a short sha", "push", body(t, with(main(), "after", "0123456")),
			source.InvalidCommit, `not a full 40-character commit SHA: "0123456"`,
		},
		{
			"pull without installation", "pull_request", body(t, without(pullPayload("opened", nil), "installation")),
			source.InvalidPayload, malformed + "a pull request without an installation",
		},
		{
			"pull without pull_request", "pull_request", body(t, without(pullPayload("opened", nil), "pull_request")),
			source.InvalidPayload, malformed + "missing field `pull_request`",
		},
		{
			"a head repo without name", "pull_request", body(t, pullPayload("opened", map[string]any{})),
			source.InvalidPayload, malformed + "missing field `full_name`",
		},
		{
			"installation without action", "installation", []byte(`{"installation":{"id":9}}`),
			source.InvalidPayload, malformed + "missing field `action`",
		},
		{
			"installation without installation", "installation", []byte(`{"action":"created"}`),
			source.InvalidPayload, malformed + "missing field `installation`",
		},
		{
			"an account without login", "installation", []byte(`{"action":"created","installation":{"id":9,"account":{}}}`),
			source.InvalidPayload, malformed + "missing field `login`",
		},
	}
	for _, c := range cases {
		got, err := source.ParseGitHub(c.event, c.body)
		var inv *source.Invalid
		if !errors.As(err, &inv) || inv.Kind != c.kind || got != nil || !errors.Is(err, kerr.ErrValidation) {
			t.Errorf("%s: got %v, %v", c.name, got, err)
			continue
		}
		if c.want != "" && err.Error() != c.want {
			t.Errorf("%s: got %q, want %q", c.name, err, c.want)
		}
	}
}

func TestInstallationEventsCarryTheAccount(t *testing.T) {
	cases := []struct {
		body string
		want source.InstallationEvent
	}{
		{
			`{"action":"created","installation":{"id":9,"account":{"login":"acme"}}}`,
			source.InstallationEvent{Action: source.InstallationCreated, InstallationID: 9, Account: "acme"},
		},
		{
			`{"action":"deleted","installation":{"id":9}}`,
			source.InstallationEvent{Action: source.InstallationDeleted, InstallationID: 9},
		},
		{
			`{"action":"suspend","installation":{"id":9,"account":null}}`,
			source.InstallationEvent{Action: source.InstallationSuspended, InstallationID: 9},
		},
		{
			`{"action":"unsuspend","installation":{"id":9}}`,
			source.InstallationEvent{Action: source.InstallationUnsuspended, InstallationID: 9},
		},
	}
	for _, c := range cases {
		got, err := source.ParseGitHub("installation", []byte(c.body))
		if err != nil || got != source.WebhookEvent(c.want) {
			t.Errorf("%s: got %v, %v", c.body, got, err)
		}
	}
}

func pull(t *testing.T, payload map[string]any) source.PullEvent {
	t.Helper()
	event, err := source.ParseGitHub("pull_request", body(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := event.(source.PullEvent)
	if !ok {
		t.Fatalf("not a pull request: %v", event)
	}
	return p
}

func TestPullRequestsBecomePreviewEvents(t *testing.T) {
	got := pull(t, pullPayload("synchronize", map[string]any{"full_name": "acme/shop"}))
	want := source.PullEvent{
		Action:         source.PullUpdated,
		InstallationID: 7,
		Repository:     mustRepo(t, "acme/shop"),
		RepositoryID:   42,
		Number:         12,
		HeadRepository: "acme/shop",
		HeadBranch:     "feature/x",
		Head:           mustCommit(t),
		Open:           true,
		UpdatedAt:      1_789_639_200_000,
	}
	if diff := cmp.Diff(want, got, cmpValues); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got.FromFork() {
		t.Error("the base repository is no fork")
	}
	if pull(t, pullPayload("opened", map[string]any{"full_name": "ACME/Shop"})).FromFork() {
		t.Error("repositories compare without case")
	}

	fork := pull(t, pullPayload("opened", map[string]any{"full_name": "mallory/shop"}))
	if fork.Action != source.PullOpened || !fork.FromFork() {
		t.Errorf("got %+v", fork)
	}
	for _, action := range []string{"reopened", "ready_for_review"} {
		if p := pull(t, pullPayload(action, map[string]any{"full_name": "acme/shop"})); p.Action != source.PullOpened {
			t.Errorf("%s: got %s", action, p.Action)
		}
	}

	// A deleted fork is still a fork.
	gone := pull(t, pullPayload("closed", nil))
	if !gone.FromFork() || gone.Open || gone.Action != source.PullClosed || gone.HeadRepository != "(deleted)" {
		t.Errorf("got %+v", gone)
	}

	labeled, err := source.ParseGitHub("pull_request", body(t, pullPayload("labeled", map[string]any{"full_name": "acme/shop"})))
	if err != nil || labeled != (source.Ignored{}) {
		t.Errorf("got %v, %v", labeled, err)
	}
}

func withUpdatedAt(t *testing.T, text string) map[string]any {
	t.Helper()
	payload := pullPayload("opened", nil)
	pr, ok := payload["pull_request"].(map[string]any)
	if !ok {
		t.Fatal("the payload has no pull request")
	}
	pr["updated_at"] = text
	return payload
}

func TestUpdatedAtIsReadAsUnixMilliseconds(t *testing.T) {
	cases := []struct {
		text string
		want int64
	}{
		{"2026-09-17T10:00:00Z", 1_789_639_200_000},
		{"2026-09-17T12:00:00+02:00", 1_789_639_200_000},
		{"2026-09-17T10:00:00.123Z", 1_789_639_200_123},
		{"2026-09-17T10:00:00.123999Z", 1_789_639_200_123},
		{"1970-01-01T00:00:00Z", 0},
	}
	for _, c := range cases {
		if got := pull(t, withUpdatedAt(t, c.text)).UpdatedAt; got != c.want {
			t.Errorf("%s: got %d, want %d", c.text, got, c.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "2026-09-17", "2026-09-17T10:00:00", "2026-13-17T10:00:00Z"} {
		_, err := source.ParseGitHub("pull_request", body(t, withUpdatedAt(t, bad)))
		if invalidKind(err) != source.InvalidPayload || !strings.Contains(err.Error(), "malformed: updated_at: ") {
			t.Errorf("%q: got %v", bad, err)
		}
	}
}

func TestActionsParse(t *testing.T) {
	for _, a := range []source.PullAction{source.PullOpened, source.PullUpdated, source.PullClosed} {
		if got, err := source.ParsePullAction(string(a)); err != nil || got != a {
			t.Errorf("%q: got %q, %v", a, got, err)
		}
	}
	if _, err := source.ParsePullAction("synchronize"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("got %v", err)
	}
	all := []source.InstallationAction{
		source.InstallationCreated, source.InstallationDeleted, source.InstallationSuspended, source.InstallationUnsuspended,
	}
	for _, a := range all {
		if got, err := source.ParseInstallationAction(string(a)); err != nil || got != a {
			t.Errorf("%q: got %q, %v", a, got, err)
		}
	}
	if _, err := source.ParseInstallationAction("suspend"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("got %v", err)
	}
}

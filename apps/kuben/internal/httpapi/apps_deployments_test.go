package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

const (
	deployDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	deployments  = "/api/v1/projects/shop/environments/prod/apps/api/deployments"
)

func deployBody(expected int) map[string]any {
	return map[string]any{
		"image":               "ghcr.io/acme/api@" + deployDigest,
		"config":              map[string]any{"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}}},
		"expected_generation": expected,
	}
}

func (c *client) deploy(expected int, key string) (int, map[string]any, http.Header) {
	c.t.Helper()
	if key == "" {
		return c.do("POST", deployments, deployBody(expected))
	}
	return c.do("POST", deployments, deployBody(expected), "Idempotency-Key", key)
}

// tests/http.rs a_deployment_is_accepted_once_and_can_be_polled.
func TestADeploymentIsAcceptedOnceAndCanBePolled(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)

	status, first, headers := alice.deploy(0, "deploy-1")
	if status != http.StatusAccepted || first["generation"] != float64(1) || first["phase"] != "planned" {
		t.Fatalf("first: %d %v", status, first)
	}
	location := headers.Get("Location")
	runID, _ := first["run"].(string)
	if runID == "" || !strings.HasSuffix(location, runID) {
		t.Fatalf("location %q for %v", location, first)
	}
	if status, polled, _ := alice.do("GET", location, nil); status != 200 || polled["run"] != runID {
		t.Fatalf("poll: %d %v", status, polled)
	}
	if status, replay, _ := alice.deploy(0, "deploy-1"); status != http.StatusAccepted || replay["run"] != runID {
		t.Fatalf("same key and request: the first run: %d %v", status, replay)
	}
	if status, _, _ := alice.deploy(1, "deploy-1"); status != http.StatusConflict {
		t.Fatalf("the same key for another request: %d", status)
	}
	if status, problem, _ := alice.deploy(0, ""); status != http.StatusConflict {
		t.Fatalf("a stale generation: %d %v", status, problem)
	}
	status, second, _ := alice.deploy(1, "")
	if status != http.StatusAccepted || second["generation"] != float64(2) {
		t.Fatalf("second: %d %v", status, second)
	}
	if _, now, _ := alice.do("GET", location, nil); now["phase"] != "superseded" {
		t.Fatalf("the newer run owns the app: %v", now)
	}

	// M2.16: the runs, newest first, each with the phases it went through.
	status, runs := alice.list(deployments + "?limit=5")
	if status != 200 || len(runs) != 2 || runs[0]["generation"] != float64(2) || runs[1]["run"] != runID ||
		runs[0]["requested_by"] != "alice@example.com" {
		t.Fatalf("list: %d %v", status, runs)
	}
	timeline, _ := runs[1]["timeline"].([]any)
	phase := func(i int) any {
		step, _ := timeline[i].(map[string]any)
		return step["phase"]
	}
	if len(timeline) == 0 || phase(0) != "planned" || phase(len(timeline)-1) != "superseded" {
		t.Fatalf("timeline: %v", timeline)
	}
}

// tests/http.rs a_lost_answer_is_given_again_without_a_second_run.
func TestALostAnswerIsGivenAgainWithoutASecondRun(t *testing.T) {
	f := newFixture(t)
	_, _, tgt := f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	if status, _, _ := alice.deploy(0, "lost-1"); status != http.StatusAccepted {
		t.Fatalf("lost: %d", status)
	}
	status, retry, _ := alice.deploy(0, "lost-1")
	if status != http.StatusAccepted || retry["generation"] != float64(1) {
		t.Fatalf("retry: %d %v", status, retry)
	}
	tn, err := f.store.Tenant(t.Context(), f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(t.Context()) //nolint:errcheck // read only
	runs, err := tn.Runs(t.Context(), tgt, 10)
	if err != nil || len(runs) != 1 || retry["run"] != runs[0].Run.String() {
		t.Fatalf("one intent, one run: %v %v", runs, err)
	}
}

// tests/http.rs deployments_need_deploy_rights_a_pinned_image_and_an_app_in_sql.
func TestDeploymentsNeedDeployRightsAPinnedImageAndAnAppInSQL(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	if status, _, _ := bob.deploy(0, ""); status != http.StatusForbidden {
		t.Fatalf("a viewer cannot deploy: %d", status)
	}
	byTag := map[string]any{"image": "ghcr.io/acme/api:1.2", "config": map[string]any{}, "expected_generation": 0}
	if status := alice.status("POST", deployments, byTag); status != http.StatusUnprocessableEntity {
		t.Fatalf("a tag is not a digest: %d", status)
	}
	if status := alice.status("POST", "/api/v1/projects/shop/environments/prod/apps/nope/deployments", byTag); status != http.StatusNotFound {
		t.Fatalf("no such app: %d", status)
	}
	noConfig := map[string]any{"image": "ghcr.io/acme/api@" + deployDigest, "expected_generation": 0}
	if status := alice.status("POST", deployments, noConfig); status != http.StatusUnprocessableEntity {
		t.Fatalf("the first deploy brings its configuration: %d", status)
	}
}

// tests/http.rs scenario5_releases_are_newest_first_and_rollback_is_authorized.
func TestScenario5ReleasesAreNewestFirstAndRollbackIsAuthorized(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	const base = "/api/v1/projects/shop/environments/prod/apps/api"
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	if status := alice.status("PATCH", base, map[string]any{"image": "nginx:1.26"}); status != 200 {
		t.Fatalf("patch: %d", status)
	}
	revisions := func(list []map[string]any) []any {
		out := []any{}
		for _, r := range list {
			out = append(out, r["revision"])
		}
		return out
	}
	status, list := bob.list(base + "/releases")
	if status != 200 || len(list) != 2 || list[0]["revision"] != float64(2) || list[1]["revision"] != float64(1) ||
		list[0]["current"] != true || list[0]["image"] != "nginx:1.26" || list[0]["reason"] != "deploy" ||
		list[1]["reason"] != "create" {
		t.Fatalf("releases: %d %v", status, list)
	}
	to := func(revision int) map[string]any { return map[string]any{"revision": revision} }
	if status := bob.status("POST", base+"/rollback", to(1)); status != http.StatusForbidden {
		t.Fatalf("a viewer rolls back: %d", status)
	}
	if status := alice.status("POST", base+"/rollback", to(9)); status != http.StatusNotFound {
		t.Fatalf("no such revision: %d", status)
	}
	if status := alice.status("POST", base+"/rollback", to(1)); status != 200 {
		t.Fatalf("a rollback is a new run: SQL needs no cluster: %d", status)
	}
	_, list = alice.list(base + "/releases")
	if len(list) != 3 || list[0]["reason"] != "rollback" || list[0]["image"] != "nginx:1.27" {
		t.Fatalf("after the rollback: %v %v", revisions(list), list)
	}
}

// deployments.rs images_must_be_pinned_by_digest.
func TestImagesMustBePinnedByDigest(t *testing.T) {
	repository, digest, err := httpapi.PinnedImage("ghcr.io/acme/api@" + deployDigest)
	if err != nil || repository != "ghcr.io/acme/api" || digest.String() != deployDigest {
		t.Fatalf("%s %s %v", repository, digest, err)
	}
	for _, bad := range []string{"ghcr.io/acme/api:1.2", "ghcr.io/acme/api@latest", "@" + deployDigest} {
		if _, _, err := httpapi.PinnedImage(bad); err == nil {
			t.Errorf("%s is not pinned", bad)
		}
	}
}

// deployments.rs idempotency_keys_are_bounded.
func TestIdempotencyKeysAreBounded(t *testing.T) {
	if key, err := httpapi.IdempotencyKey(gen.OptNilString{}, "user:a"); err != nil || key.IsSome() {
		t.Fatalf("none: %v %v", key, err)
	}
	key, err := httpapi.IdempotencyKey(gen.NewOptNilString("deploy-42"), "user:a")
	if k, ok := key.Get(); err != nil || !ok || k.Key != "deploy-42" {
		t.Fatalf("key: %v %v", key, err)
	}
	if _, err := httpapi.IdempotencyKey(gen.NewOptNilString(strings.Repeat("x", 201)), "user:a"); err == nil {
		t.Fatal("too long")
	}
}

// releases.rs rollback_keeps_domains_and_volumes.
func TestRollbackKeepsDomainsAndVolumes(t *testing.T) {
	current := sampleSpec(t)
	current.Domains = httpapi.ToDomains([]string{"api.acme.com"})
	old := sampleSpec(t)
	old.Source = v1alpha1.SourceFromImage("nginx:1.25")
	restored := httpapi.RollbackSpec(current, old)
	if restored.Source.Image == nil || *restored.Source.Image != "nginx:1.25" || restored.Domains[0].Host != "api.acme.com" {
		t.Fatalf("%+v", restored)
	}
}

// releases.rs reasons_follow_the_runs.
func TestReasonsFollowTheRuns(t *testing.T) {
	record := func(generation uint64, reason string, release ids.ReleaseID) store.RunRecord {
		return store.RunRecord{
			Run: ids.New[ids.DeploymentRun](), Generation: target.Generation(generation), Reason: reason, Phase: run.Succeeded,
			RequestedBy: "user:x", Release: release, ConfigRevision: ids.New[ids.ConfigRevision](),
		}
	}
	first, second := ids.New[ids.Release](), ids.New[ids.Release]()
	created, scaled, upgraded := record(1, "deploy", first), record(2, "deploy", first), record(3, "deploy", second)
	none := opt.None[store.RunRecord]()
	cases := []struct {
		got, want string
	}{
		{httpapi.ReleaseReason(created, none), "create"},
		{httpapi.ReleaseReason(scaled, opt.Some(created)), "config"},
		{httpapi.ReleaseReason(upgraded, opt.Some(scaled)), "deploy"},
		{httpapi.ReleaseReason(record(4, "rollback", first), opt.Some(upgraded)), "rollback"},
		{httpapi.ReleaseReason(record(1, "promotion", first), none), "promote"},
		{httpapi.ReleaseReason(record(2, "restart", first), opt.Some(record(1, "deploy", first))), "restart"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("%d: %s, want %s", i, c.got, c.want)
		}
	}
}

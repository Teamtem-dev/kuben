package api_test

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// tests/http.rs m5_image_policies_deploy_new_digests.
func TestM5ImagePoliciesDeployNewDigests(t *testing.T) {
	f := newFixture(t)
	_, _, target := f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	if status, body, _ := alice.deploy(0, ""); status != http.StatusAccepted {
		t.Fatalf("deploy: %d %v", status, body)
	}
	if status := alice.status("PUT", policyURL, policyJSON(1)); status != http.StatusOK {
		t.Fatalf("environment policy: %d", status)
	}
	const path = "/api/v1/projects/shop/environments/prod/apps/api/image-policy"
	policy := map[string]any{"repository": "nginx", "pattern": "semver:>=1.26", "intervalSecs": 300}
	if status := bob.status("PUT", path, policy); status != http.StatusForbidden {
		t.Fatalf("a viewer set a policy: %d", status)
	}
	if status := alice.status("PUT", path, map[string]any{"repository": "nginx", "pattern": "semver:nope"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("a bad range: %d", status)
	}
	fast := map[string]any{"repository": "nginx", "pattern": "latest", "intervalSecs": 5}
	if status := alice.status("PUT", path, fast); status != http.StatusUnprocessableEntity {
		t.Fatalf("too often: %d", status)
	}
	if status, saved, _ := alice.do("PUT", path, policy); status != http.StatusOK || saved["repository"] != "docker.io/library/nginx" {
		t.Fatalf("save: %d %v", status, saved)
	}

	watcher := api.NewImageWatcher(f.store, testImages(t), opt.Some(testKeyring()), clock.System{}, slog.Default())
	pass := func() int {
		n, err := watcher.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := pass(); n != 1 {
		t.Fatalf("checked %d", n)
	}
	_, followed, _ := alice.do("GET", path, nil)
	if followed["lastTag"] != "1.27" || followed["lastDigest"] != nginx127 {
		t.Fatalf("followed: %v", followed)
	}
	if _, isRun := followed["lastRun"].(string); !isRun || followed["lastError"] != nil {
		t.Fatalf("followed: %v", followed)
	}
	_, runs := alice.list(deployments)
	if len(runs) == 0 || runs[0]["phase"] != "awaitingApproval" {
		t.Fatalf("production still needs an approval: %v", runs)
	}
	if runs[0]["requested_by"] != target.String() {
		t.Fatalf("the policy asked: %v", runs[0])
	}
	if n := pass(); n != 0 {
		t.Fatalf("not due yet: %d", n)
	}
	alice.do("PUT", path, policy)
	if n := pass(); n != 1 {
		t.Fatalf("checked again: %d", n)
	}
	if _, again := alice.list(deployments); len(again) != len(runs) {
		t.Fatalf("the same digest is not deployed twice: %d → %d", len(runs), len(again))
	}
	if status := alice.status("DELETE", path, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status := alice.status("GET", path, nil); status != http.StatusNotFound {
		t.Fatalf("after delete: %d", status)
	}
}

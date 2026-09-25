package api_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
)

// routes/secrets.rs values_are_bounded.
func TestValuesAreBounded(t *testing.T) {
	one := func(k, v string) map[string]string { return map[string]string{k: v} }
	if err := api.CheckValues(one("url", "postgres://db")); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := range api.MaxSecretKeys + 1 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, bad := range map[string]map[string]string{
		"empty":   {},
		"bad key": one("bad key", "x"),
		"big":     one("big", strings.Repeat("x", api.MaxSecretBytes+1)),
		// Escaping grows the sealed object beyond the raw size.
		"escaped": one("escaped", strings.Repeat("\x01", api.MaxSecretBytes/2)),
		"many":    many,
	} {
		if err := api.CheckValues(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// routes/secrets.rs put_rolls_out_unless_told_not_to.
func TestPutRollsOutUnlessToldNotTo(t *testing.T) {
	var body gen.PutSecret
	if err := body.UnmarshalJSON([]byte(`{"data":{"a":"b"}}`)); err != nil || !body.Rollout.Or(true) {
		t.Fatalf("by default: %v %v", body.Rollout, err)
	}
	body = gen.PutSecret{}
	if err := body.UnmarshalJSON([]byte(`{"data":{"a":"b"},"rollout":false}`)); err != nil || body.Rollout.Or(true) {
		t.Fatalf("told not to: %v %v", body.Rollout, err)
	}
	body = gen.PutSecret{}
	if err := body.UnmarshalJSON([]byte(`{"data":{},"value":1}`)); err == nil {
		t.Fatal("an unknown member is accepted")
	}
	if got := api.LegacySelector(); got != "app.kubernetes.io/managed-by=kuben,!kuben.dev/secret-id" {
		t.Fatal(got)
	}
}

const secretsPath = "/api/v1/projects/shop/environments/prod/secrets"

// raw is the body of a GET.
func (c *client) raw(method, path string) string {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	resp := c.send(req)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

// putSecret is tests/http.rs put_secret: secret db with key url.
func (c *client) putSecret(url string) (int, map[string]any) {
	c.t.Helper()
	status, out, _ := c.do("PUT", secretsPath+"/db", map[string]any{"data": map[string]any{"url": url}})
	return status, out
}

// deployWithSecret is tests/http.rs deploy_with_secret: a deploy of shop's
// app reading DATABASE_URL from secret db.
func (c *client) deployWithSecret(expected int) (int, map[string]any) {
	c.t.Helper()
	body := map[string]any{
		"image": "ghcr.io/acme/api@" + deployDigest,
		"config": map[string]any{
			"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}},
			"env":     []any{map[string]any{"name": "DATABASE_URL", "fromSecret": map[string]any{"name": "db", "key": "url"}}},
		},
		"expected_generation": expected,
	}
	status, out, _ := c.do("POST", deployments, body)
	return status, out
}

// boundRevisions is tests/http.rs bound_revision.
func (f fixture) boundRevisions(run any) []uint64 {
	f.t.Helper()
	var out []uint64
	for _, b := range f.runBindings(run) {
		out = append(out, b.Revision)
	}
	return out
}

// tests/http.rs m4_secret_values_are_revisions_rolled_out_to_their_apps.
func TestM4SecretValuesAreRevisionsRolledOutToTheirApps(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)

	status, first := alice.putSecret("postgres://one")
	if status != http.StatusOK || first["revision"] != float64(1) || first["storage"] != "encrypted" {
		t.Fatalf("first: %d %v", status, first)
	}
	if _, ok := first["rollouts"]; ok {
		t.Fatalf("no app uses it yet: %v", first)
	}
	if status, _ := bob.putSecret("x"); status != http.StatusForbidden {
		t.Fatalf("a viewer: %d", status)
	}
	status, listed := alice.list(secretsPath)
	if status != http.StatusOK || len(listed) != 1 || fmt.Sprint(listed[0]["keys"]) != "[url]" {
		t.Fatalf("listed: %d %v", status, listed)
	}
	if raw := alice.raw("GET", secretsPath); strings.Contains(raw, "postgres://") {
		t.Fatalf("values are never returned: %s", raw)
	}

	status, run := alice.deployWithSecret(0)
	if status != http.StatusAccepted {
		t.Fatalf("deploy: %d %v", status, run)
	}
	if diff := cmp.Diff([]uint64{1}, f.boundRevisions(run["run"])); diff != "" {
		t.Fatal(diff)
	}

	status, second := alice.putSecret("postgres://two")
	if status != http.StatusOK || second["revision"] != float64(2) {
		t.Fatalf("second: %d %v", status, second)
	}
	rollouts, _ := second["rollouts"].([]any)
	if len(rollouts) != 1 {
		t.Fatalf("rollouts: %v", second)
	}
	rollout, _ := rollouts[0].(map[string]any)
	if skipped, present := rollout["skipped"]; rollout["app"] != "api" || !present || skipped != nil {
		t.Fatalf("rollout: %v", rollout)
	}
	if diff := cmp.Diff([]uint64{2}, f.boundRevisions(rollout["run"])); diff != "" {
		t.Fatal(diff)
	}

	revisions := secretsPath + "/db/revisions"
	status, history := alice.list(revisions)
	if status != http.StatusOK {
		t.Fatalf("history: %d", status)
	}
	var seen []string
	for _, r := range history {
		seen = append(seen, fmt.Sprint(r["revision"], r["current"]))
	}
	if diff := cmp.Diff([]string{"2 true", "1 false"}, seen); diff != "" {
		t.Fatal(diff)
	}
	if status := alice.status("DELETE", secretsPath+"/db", nil); status != http.StatusConflict {
		t.Fatalf("the app references it: %d", status)
	}

	for _, tc := range []struct{ revision, want int }{
		{2, http.StatusNoContent}, {2, http.StatusConflict}, {9, http.StatusNotFound},
	} {
		if status := alice.status("POST", fmt.Sprintf("%s/%d/revoke", revisions, tc.revision), nil); status != tc.want {
			t.Errorf("revision %d: %d, want %d", tc.revision, status, tc.want)
		}
	}
	if status, refused := alice.deployWithSecret(2); status != http.StatusConflict {
		t.Fatalf("a revoked value is never deployed: %d %v", status, refused)
	}
	if status, third := alice.putSecret("postgres://three"); status != http.StatusOK || third["revoked"] != false {
		t.Fatalf("third: %d %v", status, third)
	}
}

// tests/http.rs m4_production_rotations_wait_for_approval.
func TestM4ProductionRotationsWaitForApproval(t *testing.T) {
	f := newFixture(t)
	alice, bob, _ := protected(t, f)
	alice.putSecret("postgres://one")
	status, run := alice.deployWithSecret(0)
	if status != http.StatusAccepted || run["phase"] != "awaitingApproval" {
		t.Fatalf("deploy: %d %v", status, run)
	}
	status, secret := alice.putSecret("postgres://two")
	if status != http.StatusOK {
		t.Fatalf("put: %d %v", status, secret)
	}
	rollouts, _ := secret["rollouts"].([]any)
	if len(rollouts) != 1 {
		t.Fatalf("rollouts: %v", secret)
	}
	if rollout, _ := rollouts[0].(map[string]any); rollout["approvals_required"] != float64(1) {
		t.Fatalf("rollout: %v", rollout)
	}
	body := map[string]any{"data": map[string]any{"url": "x"}, "rollout": false}
	if status := bob.status("PUT", secretsPath+"/db", body); status != http.StatusForbidden {
		t.Fatalf("a viewer: %d", status)
	}
}

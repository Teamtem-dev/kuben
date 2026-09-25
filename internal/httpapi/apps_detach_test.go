package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// deliverWithPlan is tests/http.rs deliver_with_plan: freeze a plan with
// a Deployment and a route for run, and succeed.
func (f fixture) deliverWithPlan(run map[string]any) {
	t := f.t
	ctx := t.Context()
	runText, _ := run["run"].(string)
	runID, err := ids.Parse[ids.DeploymentRun](runText)
	if err != nil {
		t.Fatalf("run %v: %v", run, err)
	}
	claim, ok, err := f.store.ClaimOperation(ctx, "test", []string{store.RunKind}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	resources := jsonValue(t, `[
		{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api-web"}},
		{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute",
		 "metadata":{"name":"api"},"spec":{"hostnames":["api.example.com"]}}]`)
	capabilities := jsonValue(t, `{"sizes":[],"gateway":"kuben-system/kuben"}`)
	f.withTenant(func(ctx context.Context, tn *store.Tenant, _ store.Project) {
		if _, frozen, err := tn.FreezeRunPlan(ctx, claim, runID, "kuben-renderer/2", capabilities, resources); err != nil || !frozen {
			t.Fatalf("freeze: %v %v", frozen, err)
		}
	})
	if finished, err := f.store.FinishOperation(ctx, claim, "succeeded", opt.None[string]()); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	f.succeed(run)
}

// detachedHeld is the unreleased detached apps of prod.
func (f fixture) detachedHeld() uint64 {
	var held uint64
	f.withTenant(func(ctx context.Context, tn *store.Tenant, project store.Project) {
		env, _, err := tn.Environment(ctx, project.ID, "prod")
		if err != nil {
			f.t.Fatal(err)
		}
		if held, err = tn.DetachedHeld(ctx, env.ID); err != nil {
			f.t.Fatal(err)
		}
	})
	return held
}

// tests/http.rs m4_export_detach_and_release.
func TestM4ExportDetachAndRelease(t *testing.T) {
	f := newFixture(t)
	_, _, target := f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	const base = prodPath + "/apps/api"
	if status, body, _ := alice.do("GET", base+"/export", nil); status != http.StatusConflict {
		t.Fatalf("nothing delivered yet: %d %v", status, body)
	}
	status, run, _ := alice.deploy(0, "")
	if status != http.StatusAccepted {
		t.Fatalf("deploy: %d %v", status, run)
	}
	f.deliverWithPlan(run)
	status, export, _ := alice.do("GET", base+"/export", nil)
	if status != http.StatusOK || export["format"] != api.ExportFormat {
		t.Fatalf("export: %d %v", status, export)
	}
	if items, _ := jsonAt(export, "manifests", "items").([]any); len(items) != 2 {
		t.Errorf("items: %v", items)
	}
	if diff := cmp.Diff([]any{"api.example.com"}, jsonAt(export, "references", "hostnames")); diff != "" {
		t.Errorf("hostnames:\n%s", diff)
	}
	if _, ok := jsonAt(export, "release", "artifacts").(map[string]any); !ok {
		t.Errorf("artifacts: %v", jsonAt(export, "release"))
	}

	const detach = base + "/detach"
	body := map[string]any{"confirm": "api", "reason": "moving to our own GitOps"}
	if status := bob.status("POST", detach, body); status != http.StatusForbidden {
		t.Errorf("a viewer detached: %d", status)
	}
	if status := alice.status("POST", detach, map[string]any{"confirm": "web", "reason": "x"}); status != http.StatusUnprocessableEntity {
		t.Errorf("the wrong name: %d", status)
	}
	status, detached, _ := alice.do("POST", detach, body)
	if status != http.StatusAccepted {
		t.Fatalf("detach: %d %v", status, detached)
	}
	if diff := cmp.Diff(export["manifests"], jsonAt(detached, "export", "manifests")); diff != "" {
		t.Errorf("the frozen export:\n%s", diff)
	}
	if status := alice.status("POST", detach, body); status != http.StatusConflict {
		t.Errorf("detached twice: %d", status)
	}
	id, _ := detached["id"].(string)
	if id != target.String() {
		t.Errorf("id %s, target %s", id, target)
	}

	status, list := alice.list(prodPath + "/detached")
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %v", status, list)
	}
	if _, has := list[0]["export"]; list[0]["completedAt"] != nil || has {
		t.Errorf("listed: %v", list[0])
	}
	release := prodPath + "/detached/" + id + "/release"
	if status := alice.status("POST", release, map[string]any{}); status != http.StatusConflict {
		t.Errorf("not finished: %d", status)
	}
	if held := f.detachedHeld(); held != 1 {
		t.Errorf("held: %d", held)
	}
	f.withTenant(func(ctx context.Context, tn *store.Tenant, _ store.Project) {
		if done, err := tn.CompleteDetach(ctx, target); err != nil || !done {
			t.Fatalf("complete: %v %v", done, err)
		}
	})
	if status := bob.status("POST", release, map[string]any{}); status != http.StatusForbidden {
		t.Errorf("a viewer released: %d", status)
	}
	if status := alice.status("POST", release, map[string]any{}); status != http.StatusNoContent {
		t.Errorf("release: %d", status)
	}
	if status := alice.status("POST", release, map[string]any{}); status != http.StatusConflict {
		t.Errorf("released twice: %d", status)
	}
	status, one, _ := alice.do("GET", prodPath+"/detached/"+id, nil)
	if status != http.StatusOK || jsonAt(one, "export", "format") != api.ExportFormat {
		t.Fatalf("one: %d %v", status, one)
	}
	if by, _ := one["releasedBy"].(string); !strings.HasPrefix(by, "user:") {
		t.Errorf("released by: %v", one["releasedBy"])
	}
	if held := f.detachedHeld(); held != 0 {
		t.Errorf("held after the release: %d", held)
	}
}

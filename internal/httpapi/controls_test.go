package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const prodPath = "/api/v1/projects/shop/environments/prod"

func windowBody(starts, ends string) *gen.CreateWindow {
	body := &gen.CreateWindow{Reason: "release freeze", EndsAt: ends}
	if starts != "" {
		body.StartsAt.SetTo(starts)
	}
	return body
}

// controls.rs windows_are_bounded.
func TestWindowsAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC).UnixMilli()
	starts, ends, err := api.ControlWindow(windowBody("", "2026-09-18T12:00:00Z"), now, api.MaxFreezeMs)
	if err != nil || starts != now || ends != now+86_400_000 {
		t.Fatalf("a day from now: %d %d %v", starts, ends, err)
	}
	if _, _, err := api.ControlWindow(windowBody("2026-09-20T00:00:00Z", "2026-09-21T00:00:00Z"), now, api.MaxFreezeMs); err != nil {
		t.Fatalf("a freeze to come: %v", err)
	}
	for _, bad := range []*gen.CreateWindow{
		windowBody("", "2026-09-17T11:00:00Z"),
		windowBody("2026-09-19T00:00:00Z", "2026-09-18T00:00:00Z"),
		windowBody("", "2026-12-01T00:00:00Z"),
		windowBody("", "tomorrow"),
	} {
		if _, _, err := api.ControlWindow(bad, now, api.MaxFreezeMs); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if _, _, err := api.ControlWindow(windowBody("", "2026-09-26T00:00:00Z"), now, api.MaxSilenceMs); err == nil {
		t.Error("a silence of more than a week was accepted")
	}
}

// controls.rs owners_and_reasons_are_checked.
func TestOwnersAndReasonsAreChecked(t *testing.T) {
	owner := func(name, runbook string) *gen.OwnerDto {
		o := &gen.OwnerDto{Owner: name}
		o.Contact.SetTo("#payments")
		if runbook != "" {
			o.RunbookUrl.SetTo(runbook)
		}
		return o
	}
	if _, err := api.CheckOwner(owner("payments", "https://wiki/pay")); err != nil {
		t.Errorf("a valid owner: %v", err)
	}
	if _, err := api.CheckOwner(owner(" ", "")); err == nil {
		t.Error("a blank owner was accepted")
	}
	if _, err := api.CheckOwner(owner("payments", "javascript:alert(1)")); err == nil {
		t.Error("a script URL was accepted")
	}
	if got, err := api.ControlText("reason", "  outage  ", 10); err != nil || got != "outage" {
		t.Errorf("trimmed: %q %v", got, err)
	}
	if _, err := api.ControlText("reason", "a\nb", 10); err == nil {
		t.Error("a control character was accepted")
	}
}

// hoursAhead is RFC 3339 time n hours from now.
func hoursAhead(n int) string {
	return time.Now().Add(time.Duration(n) * time.Hour).UTC().Format(time.RFC3339)
}

// succeed marks the run in body as succeeded; its release (http.rs succeed).
func (f fixture) succeed(body map[string]any) string {
	t := f.t
	runText, _ := body["run"].(string)
	runID, err := ids.Parse[ids.DeploymentRun](runText)
	if err != nil {
		t.Fatalf("run %v: %v", body, err)
	}
	var release ids.ReleaseID
	f.withTenant(func(ctx context.Context, tn *store.Tenant, _ store.Project) {
		if err := tn.ForceRunPhase(ctx, runID, run.Succeeded); err != nil {
			t.Fatal(err)
		}
		r, found, err := tn.RunRelease(ctx, runID)
		if err != nil || !found {
			t.Fatalf("release: %v %v", found, err)
		}
		release = r
	})
	return release.String()
}

// tests/http.rs m4_freezes_pauses_and_emergency_rollbacks.
func TestM4FreezesPausesAndEmergencyRollbacks(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	status, first, _ := alice.deploy(0, "")
	if status != http.StatusAccepted {
		t.Fatalf("deploy: %d %v", status, first)
	}
	firstRelease := f.succeed(first)
	freezes := prodPath + "/freezes"
	freeze := map[string]any{"reason": "launch", "endsAt": hoursAhead(2)}
	if status := bob.status("POST", freezes, freeze); status != http.StatusForbidden {
		t.Fatalf("a viewer froze: %d", status)
	}
	status, frozen, _ := alice.do("POST", freezes, freeze)
	if status != http.StatusCreated || frozen["active"] != true {
		t.Fatalf("freeze: %d %v", status, frozen)
	}
	redeploy := map[string]any{
		"image":               "ghcr.io/acme/api@sha256:9999999999999999999999999999999999999999999999999999999999999999",
		"expected_generation": 1,
	}
	status, refused, _ := alice.do("POST", deployments, redeploy)
	if detail, _ := refused["detail"].(string); status != http.StatusConflict || !contains(detail, "frozen") {
		t.Fatalf("a deploy during a freeze: %d %v", status, refused)
	}

	const app = prodPath + "/apps/api"
	pause := map[string]any{"reason": "incident 42"}
	if status := alice.status("POST", app+"/pause", pause); status != http.StatusNoContent {
		t.Fatalf("pause: %d", status)
	}
	if status := alice.status("POST", app+"/pause", pause); status != http.StatusConflict {
		t.Fatalf("pause twice: %d", status)
	}
	if _, shown, _ := alice.do("GET", app, nil); shown["app"].(map[string]any)["paused"] != "incident 42" { //nolint:forcetypeassert // a test
		t.Fatalf("shown: %v", shown)
	}
	emergency := app + "/emergency-rollback"
	why := map[string]any{"reason": "checkout is down"}
	if status := alice.status("POST", emergency, why); status != http.StatusNotFound {
		t.Fatalf("no release before the only one: %d", status)
	}
	chosen := map[string]any{"reason": "checkout is down", "release": firstRelease}
	status, started, _ := alice.do("POST", emergency, chosen)
	if status != http.StatusAccepted || started["approvals_required"] != float64(0) {
		t.Fatalf("emergency: %d %v", status, started)
	}
	if status := bob.status("POST", emergency, why); status != http.StatusForbidden {
		t.Fatalf("a viewer rolled back: %d", status)
	}
	if status := alice.status("POST", app+"/resume", nil); status != http.StatusNoContent {
		t.Fatalf("resume: %d", status)
	}
	lift := freezes + "/" + frozen["id"].(string) //nolint:forcetypeassert // a test
	if status := alice.status("DELETE", lift, nil); status != http.StatusNoContent {
		t.Fatalf("lift: %d", status)
	}
	if status := alice.status("DELETE", lift, nil); status != http.StatusNotFound {
		t.Fatalf("lift twice: %d", status)
	}
}

// tests/http.rs m4_owners_and_silences_are_kept.
func TestM4OwnersAndSilencesAreKept(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	owner := map[string]any{"owner": "shop-team", "contact": "#shop", "runbookUrl": "https://wiki.example.com/shop"}
	const path = "/api/v1/projects/shop/applications/api/owner"
	if status, saved, _ := alice.do("PUT", path, owner); status != http.StatusOK || saved["owner"] != "shop-team" {
		t.Fatalf("save: %d %v", status, saved)
	}
	if _, read, _ := alice.do("GET", path, nil); read["runbookUrl"] != "https://wiki.example.com/shop" {
		t.Fatalf("read: %v", read)
	}
	silences := prodPath + "/silences"
	silence := map[string]any{"reason": "maintenance", "endsAt": hoursAhead(1), "app": "api"}
	if status, made, _ := alice.do("POST", silences, silence); status != http.StatusCreated || made["active"] != true {
		t.Fatalf("silence: %d %v", status, made)
	}
	tooLong := map[string]any{"reason": "forever", "endsAt": hoursAhead(24 * 8)}
	if status := alice.status("POST", silences, tooLong); status != http.StatusUnprocessableEntity {
		t.Fatalf("a silence of eight days: %d", status)
	}
}

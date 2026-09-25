package api_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// approvals.rs refusals_map_to_http.
func TestRefusalsMapToHTTP(t *testing.T) {
	if err := api.RefusalErr(policy.ErrSelfApproval); !errors.Is(err, kerrors.ErrForbidden) {
		t.Errorf("self-approval = %v, want forbidden", err)
	}
	for _, e := range []policy.ApprovalError{
		policy.ErrNotAwaiting,
		policy.ErrExpired,
		policy.ErrAlreadyDecided,
		policy.ErrStalePlan,
	} {
		var k *kerrors.Error
		if err := api.RefusalErr(e); !errors.As(err, &k) {
			t.Errorf("%s: %v is not a kerrors.Error", e, err)
			continue
		}
		if k.Code != kerrors.Conflict || k.Detail != string(e) {
			t.Errorf("%s: got %s %q, want a conflict with its message", e, k.Code, k.Detail)
		}
	}
}

// approvals.rs decisions_reject_unknown_fields.
func TestDecisionsRejectUnknownFields(t *testing.T) {
	var body gen.DecideRequest
	if err := body.UnmarshalJSON([]byte(`{"planHash":"00ff"}`)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.PlanHash != "00ff" {
		t.Errorf("planHash = %q, want 00ff", body.PlanHash)
	}
	var unknown gen.DecideRequest
	if err := unknown.UnmarshalJSON([]byte(`{"planHash":"00","approve":true}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}

// Rust writes expiresAt, planHash and a decision's comment as null when
// absent, never leaves them out.
func TestApprovalAbsentValuesAreNull(t *testing.T) {
	runID := uuid.MustParse("0192f3a1-0000-7000-8000-000000000001")
	dto := api.ApprovalDtoOf(runID, store.RunApproval{
		Phase:       run.Planned,
		RequestedBy: "user:alice",
		Decisions:   []store.ApprovalRecord{{Approver: "bob", Decision: policy.Approve, DecidedAt: 7}},
	}, map[string]string{"alice": "alice@example.com"}, false)
	data, err := dto.MarshalJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"run":         runID.String(),
		"phase":       "planned",
		"requestedBy": "alice@example.com",
		"required":    float64(0),
		"approved":    float64(1),
		"expiresAt":   nil,
		"planHash":    nil,
		"canDecide":   false,
		"decisions": []any{map[string]any{
			"approver": "bob", "decision": "approved", "comment": nil, "decidedAt": float64(7),
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("approval JSON (-want +got):\n%s", diff)
	}

	// Present values, including an empty plan hash (Some(empty) in Rust).
	dto = api.ApprovalDtoOf(runID, store.RunApproval{
		Phase:     run.AwaitingApproval,
		ExpiresAt: opt.Some[int64](42),
		PlanHash:  opt.Some([]byte{}),
	}, nil, true)
	if v, ok := dto.ExpiresAt.Get(); !ok || v != 42 {
		t.Errorf("expiresAt = %v, want 42", dto.ExpiresAt)
	}
	if v, ok := dto.PlanHash.Get(); !ok || v != "" {
		t.Errorf("planHash = %v, want the empty hash", dto.PlanHash)
	}
}

func TestApprovedCountSaturates(t *testing.T) {
	decisions := make([]store.ApprovalRecord, 300)
	for i := range decisions {
		decisions[i] = store.ApprovalRecord{Decision: policy.Approve}
	}
	if got := (store.RunApproval{Decisions: decisions}).Approved(); got != 255 {
		t.Errorf("approved = %d, want 255", got)
	}
}

func TestEligibleToDecide(t *testing.T) {
	aliceID := ids.New[ids.User]()
	bobID := ids.New[ids.User]()
	carolID := ids.New[ids.User]()
	as := func(id ids.UserID) access.Access {
		return access.Access{Current: httpx.CurrentUser{User: model.User{ID: id}}}
	}
	tokenAccess := access.Access{Current: httpx.CurrentUser{
		User:  model.User{ID: bobID},
		Token: opt.Some(httpx.TokenGrant{Org: ids.New[ids.Org]()}),
	}}

	appr := store.RunApproval{
		Phase:       run.AwaitingApproval,
		RequestedBy: "user:" + aliceID.String(),
		Required:    1,
	}
	cancelled := appr
	cancelled.Phase = run.Cancelled
	decided := appr
	decided.Decisions = []store.ApprovalRecord{{Approver: bobID.String(), Decision: policy.Approve}}

	for _, tc := range []struct {
		name       string
		acc        access.Access
		appr       store.RunApproval
		mayApprove bool
		want       bool
	}{
		{"API tokens cannot decide", tokenAccess, appr, true, false},
		{"must be allowed to approve", as(bobID), appr, false, false},
		{"the requester never decides", as(aliceID), appr, true, false},
		{"only a waiting run", as(bobID), cancelled, true, false},
		{"one decision per approver", as(bobID), decided, true, false},
		{"another approver decides", as(bobID), appr, true, true},
		{"a second approver decides", as(carolID), decided, true, true},
	} {
		if got := api.Eligible(tc.acc, tc.appr, tc.mayApprove); got != tc.want {
			t.Errorf("%s: eligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// tests/http.rs m4_protected_deploys_wait_for_another_approver.
func TestM4ProtectedDeploysWaitForAnotherApprover(t *testing.T) {
	alice, bob, carol := protected(t, newFixture(t))
	status, runMap, _ := alice.deploy(0, "")
	if status != 202 {
		t.Fatalf("deploy = %d %v", status, runMap)
	}
	if runMap["phase"] != "awaitingApproval" || runMap["approvals_required"] != float64(1) {
		t.Fatalf("deploy = %v, want a run awaiting one approval", runMap)
	}
	planHash, _ := runMap["plan_hash"].(string)
	runID, _ := runMap["run"].(string)
	if planHash == "" || runID == "" {
		t.Fatalf("deploy = %v, want a run and a plan hash", runMap)
	}

	base := deployments + "/" + runID
	approveURL := base + "/approve"
	decide := func(h string) map[string]any {
		return map[string]any{"planHash": h, "comment": "ship it"}
	}
	expect := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: status %d, want %d", what, got, want)
		}
	}

	expect("the requester never approves", alice.status("POST", approveURL, decide(planHash)), 403)
	expect("viewers cannot approve", bob.status("POST", approveURL, decide(planHash)), 403)

	status, seen, _ := carol.do("GET", base+"/approval", nil)
	expect("approval", status, 200)
	if seen["canDecide"] != true || seen["requestedBy"] != "alice@example.com" || seen["planHash"] != planHash {
		t.Errorf("approval = %v", seen)
	}

	expect("a plan other than the one shown", carol.status("POST", approveURL, decide("00")), 409)
	expect("an empty hash is a changed plan", carol.status("POST", approveURL, decide("")), 409)
	expect("longer than a plan hash", carol.status("POST", approveURL, decide(strings.Repeat("00", 65))), 422)
	expect("not hex", carol.status("POST", approveURL, decide("zz")), 422)

	status, approved, _ := carol.do("POST", approveURL, decide(planHash))
	expect("approve", status, 200)
	if approved["phase"] != "pendingDelivery" {
		t.Errorf("phase = %v, want pendingDelivery", approved["phase"])
	}
	decisions, _ := approved["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("decisions = %v, want one", approved["decisions"])
	}
	first, _ := decisions[0].(map[string]any)
	if first["approver"] != "carol@example.com" || first["comment"] != "ship it" {
		t.Errorf("decision = %v", first)
	}

	expect("decided already", carol.status("POST", base+"/reject", decide(planHash)), 409)
}

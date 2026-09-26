package httpapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

func putBody(approvals int32, deploy, approve string) *gen.PutPolicy {
	return &gen.PutPolicy{
		RequiredApprovals: approvals,
		DeployRole:        deploy,
		ApproveRole:       approve,
		ApprovalTtlSecs:   gen.NewOptInt32(int32(policy.DefaultApprovalTTLSecs)),
		Scan:              gen.OptNilScanGateDto{},
	}
}

// policy.rs bodies_become_valid_policies.
func TestBodiesBecomeValidPolicies(t *testing.T) {
	p, err := httpapi.PolicyOf(putBody(2, "admin", "owner"), scan.Production())
	if err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if p.RequiredApprovals != 2 || p.DeployRole != perm.Admin || p.ApproveRole != perm.Owner {
		t.Errorf("policy = %+v", p)
	}
	if p.Scan != scan.Production() {
		t.Errorf("an omitted gate is kept: %+v", p.Scan)
	}

	gated := putBody(0, "developer", "admin")
	gated.Scan = gen.NewOptNilScanGateDto(gen.ScanGateDto{
		Mode:        "warn",
		Severity:    "high",
		RequireScan: gen.NewOptBool(true),
		MaxAgeSecs:  gen.NewOptInt32(86_400),
	})
	p, err = httpapi.PolicyOf(gated, scan.Off())
	if err != nil {
		t.Fatalf("gated body: %v", err)
	}
	want := scan.Gate{Mode: scan.ModeWarn, Severity: scan.SeverityHigh, RequireScan: true, MaxAgeSecs: 86_400}
	if diff := cmp.Diff(want, p.Scan); diff != "" {
		t.Errorf("gate (-want +got):\n%s", diff)
	}

	for _, tc := range []struct {
		mode, severity string
		age            int32
		message        string
	}{
		{"strict", "high", 86_400, "unknown scan gate mode `strict`"},
		{"block", "low", 86_400, "the scan gate counts `high` or `critical` findings, with a maximum age of an hour to 90 days"},
		{"block", "high", 60, "the scan gate counts `high` or `critical` findings, with a maximum age of an hour to 90 days"},
		{"block", "severe", 86_400, "unknown severity `severe`"},
	} {
		bad := putBody(0, "developer", "admin")
		bad.Scan = gen.NewOptNilScanGateDto(gen.ScanGateDto{
			Mode:        tc.mode,
			Severity:    tc.severity,
			RequireScan: gen.NewOptBool(false),
			MaxAgeSecs:  gen.NewOptInt32(tc.age),
		})
		_, err := httpapi.PolicyOf(bad, scan.Off())
		expectValidation(t, err, tc.message)
	}

	for _, tc := range []struct {
		body    *gen.PutPolicy
		message string
	}{
		{putBody(6, "developer", "admin"), "at most 5 approvals can be required"},
		{putBody(255, "developer", "admin"), "at most 5 approvals can be required"},
		{putBody(256, "developer", "admin"), "requiredApprovals: invalid value: integer `256`, expected u8"},
		{putBody(1, "viewer", "admin"), "the deploy role `viewer` cannot deploy"},
		{putBody(1, "developer", "developer"), "the approve role `developer` cannot approve releases"},
		{putBody(1, "root", "admin"), ""},
	} {
		_, err := httpapi.PolicyOf(tc.body, scan.Off())
		expectValidation(t, err, tc.message)
	}

	var parsed gen.PutPolicy
	if err := parsed.UnmarshalJSON([]byte(`{"requiredApprovals":1,"deployRole":"developer","approveRole":"admin"}`)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p, err = httpapi.PolicyOf(&parsed, scan.Off())
	if err != nil {
		t.Fatalf("parsed body: %v", err)
	}
	if p.ApprovalTTLSecs != policy.DefaultApprovalTTLSecs {
		t.Errorf("ttl = %d, want the default", p.ApprovalTTLSecs)
	}
}

// expectValidation checks err is a validation error; with a message, that
// it is its detail.
func expectValidation(t *testing.T, err error, message string) {
	t.Helper()
	var k *kerrors.Error
	if !errors.As(err, &k) || k.Code != kerrors.Validation {
		t.Errorf("%v is not a validation error (want %q)", err, message)
		return
	}
	if message != "" && k.Detail != message {
		t.Errorf("detail = %q, want %q", k.Detail, message)
	}
}

// policy.rs environments_without_a_policy_show_the_open_one, plus the null
// updatedBy and updatedAt Rust writes then.
func TestEnvironmentsWithoutPolicyShowOpenOne(t *testing.T) {
	dto := httpapi.PolicyDtoOf(opt.None[store.PolicyRevision]())
	if dto.Revision != 0 || dto.RequiredApprovals != 0 || dto.DeployRole != "developer" {
		t.Errorf("open policy = %+v", dto)
	}
	if !dto.UpdatedBy.IsNull() || !dto.UpdatedAt.IsNull() {
		t.Errorf("updatedBy/updatedAt = %+v %+v, want null", dto.UpdatedBy, dto.UpdatedAt)
	}
	data, err := dto.MarshalJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, field := range []string{`"updatedBy":null`, `"updatedAt":null`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("%s lacks %s", data, field)
		}
	}

	dto = httpapi.PolicyDtoOf(opt.Some(store.PolicyRevision{
		Revision:  3,
		Policy:    policy.Production(),
		CreatedBy: "user:a",
		CreatedAt: 7,
	}))
	if dto.Revision != 3 || dto.RequiredApprovals != 1 {
		t.Errorf("revision = %+v", dto)
	}
	if by, ok := dto.UpdatedBy.Get(); !ok || by != "user:a" {
		t.Errorf("updatedBy = %+v", dto.UpdatedBy)
	}
	if at, ok := dto.UpdatedAt.Get(); !ok || at != 7 {
		t.Errorf("updatedAt = %+v", dto.UpdatedAt)
	}
}

const policyURL = "/api/v1/projects/shop/environments/prod/policy"

// policyJSON is tests/http.rs policy.
func policyJSON(approvals int) map[string]any {
	return map[string]any{"requiredApprovals": approvals, "deployRole": "developer", "approveRole": "admin"}
}

// protected is tests/http.rs protected: shop's production app with a
// policy of one approval set by carol (an admin), and the clients of alice
// (owner), bob (viewer) and carol.
func protected(t *testing.T, f fixture) (alice, bob, carol *client) {
	t.Helper()
	f.sqlApp()
	alice = f.signIn("alice@example.com", seedPassword)
	bob = f.signIn("bob@example.com", seedPassword)
	carol, _ = f.member("carol@example.com", perm.Admin)
	status, open, _ := alice.do("GET", policyURL, nil)
	if status != 200 || open["revision"] != float64(0) {
		t.Fatalf("open policy = %d %v", status, open)
	}
	if by, ok := open["updatedBy"]; !ok || by != nil {
		t.Fatalf("open policy = %v, want updatedBy null", open)
	}
	if status := bob.status("PUT", policyURL, policyJSON(1)); status != 403 {
		t.Fatalf("viewers cannot change protection: %d", status)
	}
	status, set, _ := carol.do("PUT", policyURL, policyJSON(1))
	if status != 200 || set["revision"] != float64(1) {
		t.Fatalf("set policy = %d %v", status, set)
	}
	return alice, bob, carol
}

// tests/http.rs m4_weakening_protection_takes_an_owner.
func TestM4WeakeningProtectionTakesAnOwner(t *testing.T) {
	f := newFixture(t)
	alice, _, carol := protected(t, f)
	expect := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: status %d, want %d", what, got, want)
		}
	}

	expect("approvals > 5 rejected", carol.status("PUT", policyURL, policyJSON(9)), 422)

	status, set2, _ := carol.do("PUT", policyURL, policyJSON(2))
	expect("stricter is an admin's call", status, 200)
	if set2["revision"] != float64(2) || set2["requiredApprovals"] != float64(2) {
		t.Errorf("stricter = %v", set2)
	}

	expect("weaker is an owner's", carol.status("PUT", policyURL, policyJSON(0)), 403)

	status, created, _ := alice.do("POST", "/api/v1/tokens", map[string]any{"name": "admin-tok", "role": "admin"})
	secret, _ := created["token"].(string)
	if status != 201 || secret == "" {
		t.Fatalf("token = %d %v", status, created)
	}
	status, _ = f.bearer(secret, "PUT", policyURL, policyJSON(0))
	expect("tokens cannot change policies", status, 403)

	status, set3, _ := alice.do("PUT", policyURL, policyJSON(0))
	expect("the owner weakens", status, 200)
	if set3["revision"] != float64(3) || set3["requiredApprovals"] != float64(0) {
		t.Errorf("weaker = %v", set3)
	}

	status, runMap, _ := alice.deploy(0, "")
	if status != 202 || runMap["phase"] != "planned" || runMap["approvals_required"] != float64(0) {
		t.Fatalf("an open environment deploys at once: %d %v", status, runMap)
	}

	gateBody := map[string]any{
		"requiredApprovals": 0,
		"deployRole":        "developer",
		"approveRole":       "admin",
		"scan":              map[string]any{"mode": "block", "severity": "critical"},
	}
	status, gated, _ := alice.do("PUT", policyURL, gateBody)
	expect("gate", status, 200)
	scanMap, _ := gated["scan"].(map[string]any)
	if gated["revision"] != float64(4) || scanMap["mode"] != "block" || scanMap["severity"] != "critical" {
		t.Errorf("gated = %v", gated)
	}

	status, same, _ := alice.do("PUT", policyURL, gateBody)
	expect("same policy", status, 200)
	if same["revision"] != gated["revision"] {
		t.Errorf("the same policy made revision %v", same["revision"])
	}

	badMode := map[string]any{
		"requiredApprovals": 0, "deployRole": "developer", "approveRole": "admin",
		"scan": map[string]any{"mode": "strict", "severity": "critical"},
	}
	status, problem, _ := alice.do("PUT", policyURL, badMode)
	expect("unknown mode", status, 422)
	if problem["detail"] != "validation failed: unknown scan gate mode `strict`" {
		t.Errorf("unknown mode = %v", problem)
	}
}

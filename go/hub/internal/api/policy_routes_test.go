package api_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
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

func TestBodiesBecomeValidPolicies(t *testing.T) {
	// Standard body
	p, err := api.PolicyOf(putBody(2, "admin", "owner"), scan.Production())
	require.NoError(t, err)
	assert.Equal(t, uint8(2), p.RequiredApprovals)
	assert.Equal(t, perm.Admin, p.DeployRole)
	assert.Equal(t, perm.Owner, p.ApproveRole)
	assert.Equal(t, scan.Production(), p.Scan, "an omitted gate is kept")

	// Gated body
	gated := putBody(0, "developer", "admin")
	gated.Scan = gen.NewOptNilScanGateDto(gen.ScanGateDto{
		Mode:        "warn",
		Severity:    "high",
		RequireScan: gen.NewOptBool(true),
		MaxAgeSecs:  gen.NewOptInt32(86_400),
	})
	p, err = api.PolicyOf(gated, scan.Off())
	require.NoError(t, err)
	assert.Equal(t, scan.ModeWarn, p.Scan.Mode)
	assert.Equal(t, scan.SeverityHigh, p.Scan.Severity)
	assert.True(t, p.Scan.RequireScan)
	assert.Equal(t, uint32(86_400), p.Scan.MaxAgeSecs)

	// Bad scan configurations
	for _, tc := range []struct {
		mode, severity string
		age            int32
	}{
		{"strict", "high", 86_400},
		{"block", "low", 86_400},
		{"block", "high", 60},
	} {
		bad := putBody(0, "developer", "admin")
		bad.Scan = gen.NewOptNilScanGateDto(gen.ScanGateDto{
			Mode:        tc.mode,
			Severity:    tc.severity,
			RequireScan: gen.NewOptBool(false),
			MaxAgeSecs:  gen.NewOptInt32(tc.age),
		})
		_, err := api.PolicyOf(bad, scan.Off())
		assert.Error(t, err, "bad scan: %s %s %d", tc.mode, tc.severity, tc.age)
	}

	// Bad role or approvals combinations
	for _, bad := range []*gen.PutPolicy{
		putBody(6, "developer", "admin"),
		putBody(1, "viewer", "admin"),
		putBody(1, "developer", "developer"),
		putBody(1, "root", "admin"),
	} {
		_, err := api.PolicyOf(bad, scan.Off())
		assert.Error(t, err, "bad body: %+v", bad)
	}

	// Parsed JSON with default TTL when omitted
	var parsed gen.PutPolicy
	err = json.Unmarshal([]byte(`{"requiredApprovals":1,"deployRole":"developer","approveRole":"admin"}`), &parsed)
	require.NoError(t, err)
	assert.False(t, parsed.ApprovalTtlSecs.IsSet(), "ttl is not set in input")
	p, err = api.PolicyOf(&parsed, scan.Off())
	require.NoError(t, err)
	assert.Equal(t, policy.DefaultApprovalTTLSecs, p.ApprovalTTLSecs, "defaults to DEFAULT_APPROVAL_TTL_SECS")
}

func TestEnvironmentsWithoutPolicyShowOpenOne(t *testing.T) {
	dto := api.PolicyDtoOf(opt.None[store.PolicyRevision]())
	assert.Equal(t, int64(0), dto.Revision)
	assert.Equal(t, int32(0), dto.RequiredApprovals)
	assert.Equal(t, "developer", dto.DeployRole)
	assert.False(t, dto.UpdatedBy.Set)
	assert.False(t, dto.UpdatedAt.Set)

	revision := store.PolicyRevision{
		Revision:  3,
		Policy:    policy.Production(),
		CreatedBy: "user:a",
		CreatedAt: 7,
	}
	dto = api.PolicyDtoOf(opt.Some(revision))
	assert.Equal(t, int64(3), dto.Revision)
	assert.Equal(t, int32(1), dto.RequiredApprovals)
	by, ok := dto.UpdatedBy.Get()
	assert.True(t, ok)
	assert.Equal(t, "user:a", by)
	at, ok := dto.UpdatedAt.Get()
	assert.True(t, ok)
	assert.Equal(t, int64(7), at)
}

func TestM4WeakeningProtectionTakesAnOwner(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()

	alice := f.signIn("alice@example.com", seedPassword)
	bob, _ := f.member("bob@example.com", perm.Viewer)
	carol, _ := f.member("carol@example.com", perm.Admin)

	const policyURL = "/api/v1/projects/shop/environments/prod/policy"
	policyJSON := func(approvals int) map[string]any {
		return map[string]any{
			"requiredApprovals": approvals,
			"deployRole":        "developer",
			"approveRole":       "admin",
		}
	}

	// 1. Initial state: environment has open policy with revision 0
	status, seen, _ := alice.do("GET", policyURL, nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, float64(0), seen["revision"])
	assert.Equal(t, float64(0), seen["requiredApprovals"])
	assert.Equal(t, "developer", seen["deployRole"])

	// 2. Viewers cannot change protection
	assert.Equal(t, 403, bob.status("PUT", policyURL, policyJSON(1)), "viewers cannot change protection")

	// 3. Invalid policy (too many approvals) returns 422 Unprocessable Entity
	assert.Equal(t, 422, carol.status("PUT", policyURL, policyJSON(9)), "approvals > 5 rejected")

	// 4. Admin (carol) sets policy(1) -> 200 OK, revision 1
	status, set1, _ := carol.do("PUT", policyURL, policyJSON(1))
	assert.Equal(t, 200, status)
	assert.Equal(t, float64(1), set1["revision"])
	assert.Equal(t, float64(1), set1["requiredApprovals"])

	// 5. Admin sets stricter policy(2) -> 200 OK, revision 2
	status, set2, _ := carol.do("PUT", policyURL, policyJSON(2))
	assert.Equal(t, 200, status, "stricter is an admin's call")
	assert.Equal(t, float64(2), set2["revision"])
	assert.Equal(t, float64(2), set2["requiredApprovals"])

	// 6. Admin attempts to weaken policy(0) -> 403 Forbidden
	assert.Equal(t, 403, carol.status("PUT", policyURL, policyJSON(0)), "weaker is an owner's")

	// 7. Tokens cannot change policies even for an owner
	status, createdToken, _ := alice.do("POST", "/api/v1/tokens", map[string]any{"name": "admin-tok", "role": "admin"})
	require.Equal(t, 201, status)
	tok, _ := createdToken["token"].(string)
	require.NotEmpty(t, tok)
	status, _ = f.bearer(tok, "PUT", policyURL, policyJSON(0))
	assert.Equal(t, 403, status, "tokens cannot change policies")

	// 8. Owner (alice) weakens policy to 0 -> 200 OK, revision 3
	status, set3, _ := alice.do("PUT", policyURL, policyJSON(0))
	assert.Equal(t, 200, status)
	assert.Equal(t, float64(3), set3["revision"])
	assert.Equal(t, float64(0), set3["requiredApprovals"])

	// 9. Deploying in an open environment goes straight to planned (no approval wait)
	const deployURL = "/api/v1/projects/shop/environments/prod/apps/api/deployments"
	status, runMap, _ := alice.do("POST", deployURL, map[string]any{
		"expectedGeneration": 0,
		"image":              "docker.io/library/nginx@" + nginx127,
	})
	assert.Equal(t, 202, status)
	assert.Equal(t, "planned", runMap["phase"])
	assert.Equal(t, float64(0), runMap["approvals_required"])

	// 10. PUT with scan gate
	gateBody := map[string]any{
		"requiredApprovals": 0,
		"deployRole":        "developer",
		"approveRole":       "admin",
		"scan": map[string]any{
			"mode":     "block",
			"severity": "critical",
		},
	}
	status, gated, _ := alice.do("PUT", policyURL, gateBody)
	assert.Equal(t, 200, status)
	assert.Equal(t, float64(4), gated["revision"])
	scanMap, ok := gated["scan"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "block", scanMap["mode"])
	assert.Equal(t, "critical", scanMap["severity"])

	// 11. Idempotent PUT (same policy returns current revision without incrementing)
	status, same, _ := alice.do("PUT", policyURL, gateBody)
	assert.Equal(t, 200, status)
	assert.Equal(t, gated["revision"], same["revision"], "same policy does not create new revision")
}

package api_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func TestRefusalsMapToHTTP(t *testing.T) {
	err := api.RefusalErr(policy.ErrSelfApproval)
	assert.True(t, errors.Is(err, kerr.ErrForbidden), "self-approval maps to forbidden")

	for _, e := range []policy.ApprovalError{
		policy.ErrNotAwaiting,
		policy.ErrExpired,
		policy.ErrAlreadyDecided,
		policy.ErrStalePlan,
	} {
		err := api.RefusalErr(e)
		var k *kerr.Error
		if assert.True(t, errors.As(err, &k), "%s should be a kerr.Error", e) {
			assert.Equal(t, kerr.Conflict, k.Code, "%s should map to Conflict", e)
			assert.Equal(t, string(e), k.Detail)
		}
	}
}

func TestEligibleToDecide(t *testing.T) {
	aliceID := ids.New[ids.User]()
	bobID := ids.New[ids.User]()
	carolID := ids.New[ids.User]()

	aliceAccess := access.Access{
		Current: httpx.CurrentUser{
			User: model.User{ID: aliceID},
		},
	}
	bobAccess := access.Access{
		Current: httpx.CurrentUser{
			User: model.User{ID: bobID},
		},
	}
	tokenAccess := access.Access{
		Current: httpx.CurrentUser{
			User:  model.User{ID: bobID},
			Token: opt.Some(httpx.TokenGrant{Org: ids.New[ids.Org]()}),
		},
	}

	appr := store.RunApproval{
		Phase:       run.AwaitingApproval,
		RequestedBy: "user:" + aliceID.String(),
		Required:    1,
		Decisions:   nil,
	}

	// 1. Caller with API token cannot decide
	assert.False(t, api.Eligible(tokenAccess, appr, true), "API tokens cannot decide")

	// 2. Caller without mayApprove cannot decide
	assert.False(t, api.Eligible(bobAccess, appr, false), "must have mayApprove")

	// 3. Requester cannot decide on their own run
	assert.False(t, api.Eligible(aliceAccess, appr, true), "requester cannot decide on own run")

	// 4. Run not in awaitingApproval cannot be decided
	cancelledAppr := appr
	cancelledAppr.Phase = run.Cancelled
	assert.False(t, api.Eligible(bobAccess, cancelledAppr, true), "cannot decide on non-awaiting run")

	// 5. Approver who already decided cannot decide again
	alreadyDecidedAppr := appr
	alreadyDecidedAppr.Decisions = []store.ApprovalRecord{
		{Approver: bobID.String(), Decision: policy.Approve},
	}
	assert.False(t, api.Eligible(bobAccess, alreadyDecidedAppr, true), "already decided approver cannot decide again")

	// 6. Eligible third-party approver with mayApprove can decide
	assert.True(t, api.Eligible(bobAccess, appr, true), "eligible approver can decide")

	// 7. Carol (another distinct approver) can also decide
	carolAccess := access.Access{
		Current: httpx.CurrentUser{
			User: model.User{ID: carolID},
		},
	}
	assert.True(t, api.Eligible(carolAccess, alreadyDecidedAppr, true), "carol can decide even if bob already did")
}

// tests/http.rs m4_protected_deploys_wait_for_another_approver.
func TestM4ProtectedDeploysWaitForAnotherApprover(t *testing.T) {
	alice, bob, carol := protected(t, newFixture(t))
	status, runMap, _ := alice.deploy(0, "")
	require.Equal(t, 202, status, "%v", runMap)
	assert.Equal(t, "awaitingApproval", runMap["phase"])
	assert.Equal(t, float64(1), runMap["approvals_required"])
	planHash, _ := runMap["plan_hash"].(string)
	require.NotEmpty(t, planHash)
	runID, _ := runMap["run"].(string)
	require.NotEmpty(t, runID)

	base := deployments + "/" + runID
	approveURL := base + "/approve"
	decide := func(h string) map[string]any {
		return map[string]any{"planHash": h, "comment": "ship it"}
	}

	// Requester (alice) cannot approve
	assert.Equal(t, 403, alice.status("POST", approveURL, decide(planHash)), "the requester never approves")
	// Viewer (bob) cannot approve
	assert.Equal(t, 403, bob.status("POST", approveURL, decide(planHash)), "viewers cannot approve")

	// Check approval as carol
	status, seen, _ := carol.do("GET", base+"/approval", nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, true, seen["canDecide"])
	assert.Equal(t, "alice@example.com", seen["requestedBy"])

	// Carol approves with wrong plan hash -> 409 Conflict
	assert.Equal(t, 409, carol.status("POST", approveURL, decide("00")), "a plan other than the one shown")

	// Carol approves with correct plan hash -> 200 OK
	status, approved, _ := carol.do("POST", approveURL, decide(planHash))
	assert.Equal(t, 200, status)
	assert.Equal(t, "pendingDelivery", approved["phase"])
	decisions, _ := approved["decisions"].([]any)
	require.Len(t, decisions, 1)
	firstDec, _ := decisions[0].(map[string]any)
	assert.Equal(t, "carol@example.com", firstDec["approver"])

	// Carol rejects after already decided -> 409 Conflict
	assert.Equal(t, 409, carol.status("POST", base+"/reject", decide(planHash)), "decided already")
}

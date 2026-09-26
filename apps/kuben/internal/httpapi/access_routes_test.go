package httpapi_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
)

// routes/access.rs nobody_grants_or_removes_above_their_own_role.
func TestNobodyGrantsOrRemovesAboveTheirOwnRole(t *testing.T) {
	some := opt.Some[perm.Role]
	none := opt.None[perm.Role]()
	cases := []struct {
		caller, current, granted opt.Val[perm.Role]
		want                     bool
		why                      string
	}{
		{some(perm.Admin), none, some(perm.Developer), true, "grant below"},
		{some(perm.Admin), some(perm.Developer), some(perm.Admin), true, "raise to one's own"},
		{some(perm.Admin), none, some(perm.Owner), false, "escalation"},
		{some(perm.Admin), some(perm.Owner), some(perm.Viewer), false, "demoting a stronger member"},
		{some(perm.Admin), some(perm.Owner), none, false, "removing a stronger member"},
		{some(perm.Owner), some(perm.Owner), none, true, "an owner removes an owner"},
		{none, none, some(perm.Viewer), false, "no role here"},
	}
	for _, c := range cases {
		if got := httpapi.MayAssign(c.caller, c.current, c.granted); got != c.want {
			t.Errorf("%s: %v", c.why, got)
		}
	}
}

// routes/access.rs member_ids_are_user_ids.
func TestMemberIDsAreUserIDs(t *testing.T) {
	if _, err := httpapi.MemberID("0192f3a1-0000-7000-8000-00000000000a"); err != nil {
		t.Fatal(err)
	}
	if _, err := httpapi.MemberID("bob"); kerrors.CodeOf(err) != kerrors.NotFound {
		t.Fatalf("bob: %v", err)
	}
	var body gen.PutScopedRole
	if err := body.UnmarshalJSON([]byte(`{"role":"admin","scope":"org"}`)); err == nil {
		t.Fatal("unknown members are refused")
	}
}

// tests/http.rs m4_project_roles_are_granted_without_escalation. The
// deployment steps wait for the deployment routes; what a project role
// grants is shown by the project's member list and by removal instead.
func TestM4ProjectRolesAreGrantedWithoutEscalation(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	carol, carolID := f.member("carol@example.com", perm.Admin)
	dave, daveID := f.member("dave@example.com", perm.Viewer)
	const members = "/api/v1/projects/shop/members"
	davePath := members + "/" + daveID

	if status := carol.status("PUT", davePath, map[string]any{"role": "owner"}); status != 403 {
		t.Fatalf("an admin cannot grant owner: %d", status)
	}
	if status, body, _ := carol.do("PUT", members+"/"+carolID, map[string]any{"role": "viewer"}); status != 409 ||
		body["detail"] != "conflict: you cannot change your own role" {
		t.Fatalf("nobody changes their own role: %d %v", status, body)
	}
	status, granted, _ := carol.do("PUT", davePath, map[string]any{"role": "developer"})
	if status != 200 || granted["role"] != "developer" || granted["email"] != "dave@example.com" {
		t.Fatalf("grant: %d %v", status, granted)
	}
	if status, listed := dave.list(members); status != 200 || len(listed) != 1 || listed[0]["id"] != daveID {
		t.Fatalf("listed: %d %v", status, listed)
	}
	if status := dave.status("PUT", members+"/"+carolID, map[string]any{"role": "viewer"}); status != 403 {
		t.Fatalf("a project developer manages no roles: %d", status)
	}

	if status, body, _ := alice.do("PUT", members+"/"+uuid.Must(uuid.NewV7()).String(), map[string]any{"role": "viewer"}); status != 404 {
		t.Fatalf("not a member of the organization: %d %v", status, body)
	}
	if status := alice.status("DELETE", "/api/v1/members/"+daveID, nil); status != 204 {
		t.Fatalf("remove dave: %d", status)
	}
	if status := dave.status("GET", "/api/v1/me", nil); status != 401 && status != 403 {
		t.Fatalf("removed members lose every role: %d", status)
	}
	if status, body, _ := alice.do("DELETE", davePath, nil); status != 404 || body["detail"] != "not found: a role of member `"+daveID+"` here" {
		t.Fatalf("the project role went with the membership: %d %v", status, body)
	}
}

func TestEnvironmentRoles(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	carol, carolID := f.member("carol@example.com", perm.Admin)
	const members = "/api/v1/projects/shop/environments/prod/members"

	if status, listed := alice.list(members); status != 200 || len(listed) != 0 {
		t.Fatalf("nobody yet: %d %v", status, listed)
	}
	status, granted, _ := alice.do("PUT", members+"/"+carolID, map[string]any{"role": "owner"})
	if status != 200 || granted["role"] != "owner" {
		t.Fatalf("grant: %d %v", status, granted)
	}
	if status, listed := carol.list(members); status != 200 || len(listed) != 1 || listed[0]["role"] != "owner" {
		t.Fatalf("listed: %d %v", status, listed)
	}
	if status, _ := carol.list("/api/v1/projects/shop/members"); status != 200 {
		t.Fatalf("the project list: %d", status)
	}
	if status, body, _ := carol.do("DELETE", members+"/"+carolID, nil); status != 409 ||
		body["detail"] != "conflict: you cannot remove your own role" {
		t.Fatalf("own role: %d %v", status, body)
	}
	if status, body, _ := alice.do("PUT", members+"/"+carolID, map[string]any{"role": "chief"}); status != 422 ||
		body["detail"] != "validation failed: unknown role `chief`" {
		t.Fatalf("unknown role: %d %v", status, body)
	}
	for _, path := range []string{
		"/api/v1/projects/shop/environments/nope/members",
		"/api/v1/projects/nope/environments/prod/members",
		"/api/v1/projects/secret-project/members",
	} {
		if status, _ := alice.list(path); status != 404 {
			t.Errorf("%s: %d", path, status)
		}
	}

	status, created, _ := alice.do("POST", "/api/v1/tokens", map[string]any{"name": "admin", "role": "admin"})
	token, _ := created["token"].(string)
	if status != 201 {
		t.Fatalf("token: %d %v", status, created)
	}
	if status, _ := f.bearer(token, "DELETE", members+"/"+carolID, nil); status != 403 {
		t.Fatalf("tokens manage no roles: %d", status)
	}
	if status := alice.status("DELETE", members+"/"+carolID, nil); status != 204 {
		t.Fatalf("remove: %d", status)
	}
	if status := alice.status("DELETE", members+"/"+carolID, nil); status != 404 {
		t.Fatalf("removed already: %d", status)
	}
}

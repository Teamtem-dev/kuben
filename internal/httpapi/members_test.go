package api_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
)

func TestEmailAddresses(t *testing.T) {
	valid := []string{"carol@example.com", "a@b.c", "x+tag@sub.example.org"}
	invalid := []string{
		"", "carol", "@example.com", "carol@example", "carol@exa@mple.com", "ca rol@example.com",
		"carol@example.com\t", strings.Repeat("a", 250) + "@b.cd",
	}
	for _, e := range valid {
		if err := api.ValidEmail(e); err != nil {
			t.Errorf("%q: %v", e, err)
		}
	}
	for _, e := range invalid {
		if err := api.ValidEmail(e); kerr.CodeOf(err) != kerr.Validation {
			t.Errorf("%q: %v", e, err)
		}
	}
	if err := api.ValidEmail("carol"); err == nil || err.Error() != "validation failed: `carol` is not a valid email address" {
		t.Fatal(err)
	}
}

func invite(c *client, email, role string) (int, map[string]any) {
	c.t.Helper()
	status, body, _ := c.do("POST", "/api/v1/members", map[string]any{"email": email, "role": role})
	return status, body
}

func changePassword(c *client, current, next string) int {
	c.t.Helper()
	return c.status("POST", "/api/v1/me/password", map[string]any{"current_password": current, "new_password": next})
}

// tests/http.rs scenario4_team_members_follow_the_role_rules.
func TestScenario4TeamMembersFollowTheRoleRules(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	_, me, _ := alice.do("GET", "/api/v1/me", nil)
	aliceID, _ := me["id"].(string)

	status, invited := invite(alice, "carol@example.com", "developer")
	if status != 201 {
		t.Fatalf("invite: %d %v", status, invited)
	}
	temp, _ := invited["temporary_password"].(string)
	member, _ := invited["member"].(map[string]any)
	carolID, _ := member["id"].(string)
	if temp == "" || member["role"] != "developer" || member["must_change_password"] != true || member["display_name"] != nil {
		t.Fatalf("invited: %v", invited)
	}
	carol, status, me := f.trySignIn("carol@example.com", temp)
	if status != 200 || me["must_change_password"] != true {
		t.Fatalf("carol signs in: %d %v", status, me)
	}
	if status := carol.status("GET", "/api/v1/projects", nil); status != 403 {
		t.Fatalf("the temporary password must be replaced first: %d", status)
	}
	if status := changePassword(carol, temp, "short"); status != 422 {
		t.Fatalf("a short password: %d", status)
	}
	if status := changePassword(carol, "wrong", "correct horse battery"); status != 422 {
		t.Fatalf("a wrong current password: %d", status)
	}
	if status := changePassword(carol, temp, "correct horse battery"); status != 204 {
		t.Fatalf("change: %d", status)
	}
	if status := carol.status("GET", "/api/v1/projects", nil); status != 200 {
		t.Fatalf("projects after the change: %d", status)
	}

	status, changed, _ := alice.do("PATCH", "/api/v1/members/"+carolID, map[string]any{"role": "viewer"})
	if status != 200 || changed["role"] != "viewer" || changed["email"] != "carol@example.com" {
		t.Fatalf("role change: %d %v", status, changed)
	}
	if status, body, _ := alice.do("PATCH", "/api/v1/members/"+aliceID, map[string]any{"role": "admin"}); status != 409 ||
		body["detail"] != "conflict: you cannot change your own role" {
		t.Fatalf("own role: %d %v", status, body)
	}
	if status, _ := invite(carol, "eve@example.com", "viewer"); status != 403 {
		t.Fatalf("viewers cannot invite: %d", status)
	}

	_, dave := invite(alice, "dave@example.com", "admin")
	daveTemp, _ := dave["temporary_password"].(string)
	daveClient := f.signIn("dave@example.com", daveTemp)
	if status := changePassword(daveClient, daveTemp, "another long password"); status != 204 {
		t.Fatalf("dave's password: %d", status)
	}
	if status, _ := invite(daveClient, "mallory@example.com", "owner"); status != 403 {
		t.Fatalf("no escalation: %d", status)
	}
	if status := daveClient.status("DELETE", "/api/v1/members/"+aliceID, nil); status != 403 {
		t.Fatalf("only owners touch owners: %d", status)
	}
	if status, _ := invite(daveClient, "erin@example.com", "developer"); status != 201 {
		t.Fatalf("admins can invite: %d", status)
	}

	if status := alice.status("DELETE", "/api/v1/members/"+carolID, nil); status != 204 {
		t.Fatalf("remove: %d", status)
	}
	if status := carol.status("GET", "/api/v1/me", nil); status != 401 {
		t.Fatalf("removal revokes sessions: %d", status)
	}
	status, members := alice.list("/api/v1/members")
	emails := make([]string, 0, len(members))
	for _, m := range members {
		email, _ := m["email"].(string)
		emails = append(emails, email)
	}
	if status != 200 || !slices.Contains(emails, "erin@example.com") || slices.Contains(emails, "carol@example.com") {
		t.Fatalf("members: %d %v", status, emails)
	}
}

func TestMemberRules(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	_, me, _ := alice.do("GET", "/api/v1/me", nil)
	aliceID, _ := me["id"].(string)

	status, members := alice.list("/api/v1/members")
	if status != 200 || len(members) != 2 || members[0]["email"] != "alice@example.com" || members[0]["display_name"] != "Alice" ||
		members[0]["role"] != "owner" || members[0]["active"] != true || members[1]["display_name"] != nil {
		t.Fatalf("members: %d %v", status, members)
	}
	bob := f.signIn("bob@example.com", seedPassword)
	if status, _ := bob.list("/api/v1/members"); status != 200 {
		t.Fatalf("viewers read members: %d", status)
	}

	refused := []struct {
		body   map[string]any
		status int
		detail string
	}{
		{map[string]any{"email": "not-an-address", "role": "viewer"}, 422, "validation failed: `not-an-address` is not a valid email address"},
		{map[string]any{"email": "x@example.com", "role": "root"}, 422, "validation failed: unknown role `root`"},
		{map[string]any{"email": " BOB@example.com ", "role": "viewer"}, 409, "conflict: `bob@example.com` is already a member"},
	}
	for _, c := range refused {
		if status, body, _ := alice.do("POST", "/api/v1/members", c.body); status != c.status || body["detail"] != c.detail {
			t.Errorf("%v: %d %v", c.body, status, body)
		}
	}
	status, invited, _ := alice.do("POST", "/api/v1/members", map[string]any{"email": "Frank@Example.com", "display_name": "  Frank  "})
	member, _ := invited["member"].(map[string]any)
	if status != 201 || member["email"] != "frank@example.com" || member["display_name"] != "Frank" || member["role"] != "developer" {
		t.Fatalf("invite with the default role: %d %v", status, invited)
	}
	frankID, _ := member["id"].(string)

	// Someone with an account elsewhere joins without a new password.
	if err := f.store.RemoveMember(t.Context(), f.org, mustUserID(t, frankID)); err != nil {
		t.Fatal(err)
	}
	status, again, _ := alice.do("POST", "/api/v1/members", map[string]any{"email": "frank@example.com", "role": "viewer"})
	if status != 201 || again["temporary_password"] != nil {
		t.Fatalf("an existing account: %d %v", status, again)
	}

	for _, c := range []struct{ path, detail string }{
		{"/api/v1/members/bob", "not found: member `bob`"},
		{"/api/v1/members/0192f3a1-0000-7000-8000-00000000000a", "not found: member `0192f3a1-0000-7000-8000-00000000000a`"},
	} {
		if status, body, _ := alice.do("PATCH", c.path, map[string]any{"role": "viewer"}); status != 404 || body["detail"] != c.detail {
			t.Errorf("%s: %d %v", c.path, status, body)
		}
	}
	if status, body, _ := alice.do("DELETE", "/api/v1/members/"+aliceID, nil); status != 409 || body["detail"] != "conflict: you cannot remove yourself" {
		t.Fatalf("remove yourself: %d %v", status, body)
	}
	if status := bob.status("PATCH", "/api/v1/members/"+frankID, map[string]any{"role": "developer"}); status != 403 {
		t.Fatalf("viewers change no roles: %d", status)
	}
}

func mustUserID(t *testing.T, id string) ids.UserID {
	t.Helper()
	u, err := ids.Parse[ids.User](id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

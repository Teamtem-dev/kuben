package perm_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

func TestRolesFormAStrictHierarchy(t *testing.T) {
	ordered := []perm.Role{perm.Viewer, perm.Developer, perm.Admin, perm.Owner}
	for i := 1; i < len(ordered); i++ {
		lower, higher := ordered[i-1], ordered[i]
		if lower.Rank() >= higher.Rank() {
			t.Fatalf("%s must rank below %s", lower, higher)
		}
		for _, p := range lower.Perms() {
			if !higher.Grants(p) {
				t.Errorf("%s must grant %s (from %s)", higher, p, lower)
			}
		}
	}
	if got := perm.Owner.Weaker(perm.Developer); got != perm.Developer {
		t.Errorf("got %s", got)
	}
	if got := perm.Viewer.Weaker(perm.Admin); got != perm.Viewer {
		t.Errorf("got %s", got)
	}
}

func TestPermissionCounts(t *testing.T) {
	// The exact grants are the contract; a new permission must be placed on purpose.
	want := map[perm.Role]int{perm.Viewer: 5, perm.Developer: 9, perm.Admin: 16, perm.Owner: 19}
	for role, n := range want {
		if got := len(role.Perms()); got != n {
			t.Errorf("%s grants %d permissions, want %d", role, got, n)
		}
	}
}

func TestViewerCannotExecOrWrite(t *testing.T) {
	if perm.Viewer.Grants(perm.AppExec) || perm.Viewer.Grants(perm.AppWrite) {
		t.Fatal("viewers only read")
	}
	if !perm.Viewer.Grants(perm.AppLogsRead) {
		t.Fatal("viewers read logs")
	}
}

func TestOnlyOwnersWeakenProtection(t *testing.T) {
	if !perm.Owner.Grants(perm.EnvProtect) {
		t.Fatal("owner")
	}
	for _, role := range []perm.Role{perm.Admin, perm.Developer, perm.Viewer} {
		if role.Grants(perm.EnvProtect) {
			t.Errorf("%s must not weaken protection", role)
		}
	}
	if !perm.Admin.Grants(perm.ReleaseApprove) || perm.Developer.Grants(perm.ReleaseApprove) {
		t.Fatal("admins approve, developers do not")
	}
}

func TestRoleParses(t *testing.T) {
	if got, err := perm.ParseRole("developer"); err != nil || got != perm.Developer {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := perm.ParseRole("root"); err == nil {
		t.Fatal("root is not a role")
	}
	if perm.Role("root").Grants(perm.OrgRead) || perm.Role("root").Weaker(perm.Owner) != "root" {
		t.Fatal("an unknown role grants nothing and is the weakest")
	}
}

func TestRolesDecodeStrictly(t *testing.T) {
	var got struct {
		Role perm.Role `json:"Role"`
	}
	if err := json.Unmarshal([]byte(`{"Role":"admin"}`), &got); err != nil || got.Role != perm.Admin {
		t.Fatalf("got %v, %v", got.Role, err)
	}
	for _, bad := range []string{`"root"`, `"Admin"`, `""`} {
		if err := json.Unmarshal([]byte(`{"Role":`+bad+`}`), &got); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

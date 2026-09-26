package authz_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
)

func project(id uuid.UUID) authz.ScopeRef {
	return authz.ScopeRef{Level: authz.LevelProject, ID: id}
}

func subject(role perm.Role, scope authz.ScopeRef) authz.Subject {
	return authz.Subject{User: ids.New[ids.User](), Bindings: []authz.Binding{{Scope: scope, Role: role}}}
}

func newUUID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func TestOrgBindingAppliesToNestedProject(t *testing.T) {
	org, p := ids.New[ids.Org](), newUUID()
	s := subject(perm.Developer, authz.OrgScope(org))
	proof, err := authz.Check(authz.RolePolicy{}, s, perm.AppDeploy, authz.ProjectChain(org, p))
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Valid() || proof.Scope() != project(p) || proof.User() != s.User || proof.Perm() != perm.AppDeploy {
		t.Fatalf("got %+v", proof)
	}
}

func TestBindingOnOtherOrgIsRejected(t *testing.T) {
	s := subject(perm.Owner, authz.OrgScope(ids.New[ids.Org]()))
	proof, err := authz.Check(authz.RolePolicy{}, s, perm.OrgRead, authz.OrgChain(ids.New[ids.Org]()))
	if !errors.Is(err, kerrors.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if proof.Valid() {
		t.Fatal("a refused check mints nothing")
	}
}

func TestViewerCannotExec(t *testing.T) {
	org := ids.New[ids.Org]()
	s := subject(perm.Viewer, authz.OrgScope(org))
	if (authz.RolePolicy{}).Allowed(s, perm.AppExec, authz.OrgChain(org)) {
		t.Fatal("viewers do not exec")
	}
}

func TestTheStrongestRoleOnTheChainCounts(t *testing.T) {
	org, p, other := ids.New[ids.Org](), newUUID(), newUUID()
	s := authz.Subject{User: ids.New[ids.User](), Bindings: []authz.Binding{
		{Scope: authz.OrgScope(org), Role: perm.Viewer},
		{Scope: project(p), Role: perm.Admin},
		{Scope: project(other), Role: perm.Owner},
	}}
	if got := authz.EffectiveRole(s, authz.ProjectChain(org, p)); got != opt.Some(perm.Admin) {
		t.Errorf("got %v", got)
	}
	if got := authz.EffectiveRole(s, authz.OrgChain(org)); got != opt.Some(perm.Viewer) {
		t.Errorf("got %v", got)
	}
	if got := authz.EffectiveRole(s, authz.OrgChain(ids.New[ids.Org]())); got.IsSome() {
		t.Errorf("got %v", got)
	}
}

func TestAliasesKeepBindingsOnImportedResourcesApplying(t *testing.T) {
	org, sql, legacy := ids.New[ids.Org](), newUUID(), newUUID()
	chain := authz.ProjectChain(org, sql)
	chain.Aliases = []authz.ScopeRef{project(legacy)}
	if !chain.Contains(project(sql)) || !chain.Contains(project(legacy)) {
		t.Fatal("the name of record and the alias are on the chain")
	}
	if chain.Contains(project(newUUID())) {
		t.Fatal("a stranger is not")
	}
	if chain.Contains(authz.ScopeRef{Level: authz.LevelEnvironment, ID: legacy}) {
		t.Fatal("an alias keeps its level")
	}
	if chain.Leaf() != project(sql) {
		t.Fatal("the SQL id is the name of record")
	}
}

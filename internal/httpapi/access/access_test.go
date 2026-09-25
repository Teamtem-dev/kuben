package access_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
)

func TestTokenScopeCapsAndReRootsAuthority(t *testing.T) {
	org, project, other := ids.New[ids.Org](), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	owner := []authz.Binding{{Scope: authz.OrgScope(org), Role: perm.Owner}}
	subject := func(b []authz.Binding) authz.Subject { return authz.Subject{User: ids.New[ids.User](), Bindings: b} }
	policy := authz.RolePolicy{}

	s := subject(access.Restrict(owner, model.TokenScope{Role: perm.Viewer}))
	if !policy.Allowed(s, perm.AppRead, authz.ProjectChain(org, project)) || policy.Allowed(s, perm.AppDeploy, authz.ProjectChain(org, project)) {
		t.Fatal("a viewer token reads and does not deploy")
	}
	s = subject(access.Restrict(owner, model.TokenScope{Role: perm.Developer, Project: opt.Some(project)}))
	if !policy.Allowed(s, perm.AppDeploy, authz.ProjectChain(org, project)) {
		t.Fatal("a project token deploys in its project")
	}
	if policy.Allowed(s, perm.AppDeploy, authz.ProjectChain(org, other)) {
		t.Fatal("and not in another")
	}
	if policy.Allowed(s, perm.OrgRead, authz.OrgChain(org)) {
		t.Fatal("a project token has no org-wide authority")
	}
}

type bindings []model.RoleBinding

func (b bindings) BindingsForUser(context.Context, ids.UserID) ([]model.RoleBinding, error) {
	return b, nil
}

func TestResolve(t *testing.T) {
	orgA, orgB := ids.New[ids.Org](), ids.New[ids.Org]()
	project := uuid.Must(uuid.NewV7())
	rows := bindings{
		{OrgID: orgA, Role: perm.Admin, ScopeKind: model.ScopeOrg},
		{OrgID: orgA, Role: perm.Owner, ScopeKind: model.ScopeProject, ScopeUID: opt.Some(project.String())},
		{OrgID: orgB, Role: perm.Viewer, ScopeKind: model.ScopeOrg},
		{OrgID: orgB, Role: perm.Viewer, ScopeKind: model.ScopeApp, ScopeUID: opt.Some("not-a-uuid")},
	}
	user := model.User{ID: ids.New[ids.User](), IsActive: true}
	ctx := context.Background()
	if _, err := access.Resolve(ctx, rows, authz.RolePolicy{}); !errors.Is(err, kerr.ErrUnauthorized) {
		t.Fatalf("no principal: %v", err)
	}
	a, err := access.Resolve(httpx.WithUser(ctx, httpx.CurrentUser{User: user, Via: httpx.ViaSession}), rows, authz.RolePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.OrgIDs()) != 2 || len(a.Subject.Bindings) != 3 || a.OrgRole(orgA) != opt.Some(perm.Admin) || a.ForbidToken() != nil {
		t.Fatalf("got %+v", a)
	}
	if kind, ref := a.Actor(); kind != "user" || ref != "user:"+user.ID.String() {
		t.Fatalf("actor %s %s", kind, ref)
	}
	token := httpx.CurrentUser{User: user, Via: httpx.ViaToken, Token: opt.Some(httpx.TokenGrant{Org: orgB, Scope: model.TokenScope{Role: perm.Owner}})}
	a, err = access.Resolve(httpx.WithUser(ctx, token), rows, authz.RolePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.OrgIDs()) != 1 || a.OrgIDs()[0] != orgB || !errors.Is(a.ForbidToken(), kerr.ErrForbidden) {
		t.Fatalf("a token sees its own org only: %+v", a.OrgIDs())
	}
	if _, err := a.Require(perm.OrgRead, authz.OrgChain(orgA)); !errors.Is(err, kerr.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	invited := user
	invited.MustChangePassword = true
	if _, err := access.Resolve(httpx.WithUser(ctx, httpx.CurrentUser{User: invited}), rows, authz.RolePolicy{}); !errors.Is(err, kerr.ErrForbidden) {
		t.Fatalf("a temporary password comes first: %v", err)
	}
}

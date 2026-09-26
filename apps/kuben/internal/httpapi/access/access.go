// Package access resolves who may do what on a request
// (crates/kuben-api/src/authz.rs): the caller's role bindings become an
// authz.Subject; an API token's bindings are limited to its organization,
// capped at its role and re-rooted at its project or environment.
package access

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
)

// Bindings reads a user's role bindings.
type Bindings interface {
	BindingsForUser(ctx context.Context, user ids.UserID) ([]model.RoleBinding, error)
}

// Access is the caller of a request with its resolved authority.
type Access struct {
	Current httpx.CurrentUser
	Subject authz.Subject
	orgs    []ids.OrgID
	policy  authz.Policy
}

// Resolve is the access of the request in ctx: unauthorized without a
// principal, forbidden while the principal must still replace a temporary
// password (only /me and /me/password work then, and they do not resolve
// access).
func Resolve(ctx context.Context, store Bindings, policy authz.Policy) (Access, error) {
	current, ok := httpx.UserFrom(ctx)
	if !ok {
		return Access{}, kerrors.ErrUnauthorized
	}
	if current.User.MustChangePassword {
		return Access{}, kerrors.ErrForbidden
	}
	rows, err := store.BindingsForUser(ctx, current.User.ID)
	if err != nil {
		return Access{}, fmt.Errorf("role bindings: %w", err)
	}
	grant, isToken := current.Token.Get()
	if isToken {
		rows = slices.DeleteFunc(rows, func(b model.RoleBinding) bool { return b.OrgID != grant.Org })
	}
	orgs := make([]ids.OrgID, 0, len(rows))
	bindings := make([]authz.Binding, 0, len(rows))
	for _, b := range rows {
		orgs = append(orgs, b.OrgID)
		if scope, ok := scopeOf(b); ok {
			bindings = append(bindings, authz.Binding{Scope: scope, Role: b.Role})
		}
	}
	slices.SortFunc(orgs, ids.OrgID.Compare)
	orgs = slices.Compact(orgs)
	if isToken {
		bindings = Restrict(bindings, grant.Scope)
	}
	return Access{
		Current: current,
		Subject: authz.Subject{User: current.User.ID, Bindings: bindings},
		orgs:    orgs,
		policy:  policy,
	}, nil
}

func scopeOf(b model.RoleBinding) (authz.ScopeRef, bool) {
	if b.ScopeKind == model.ScopeOrg {
		return authz.OrgScope(b.OrgID), true
	}
	text, ok := b.ScopeUID.Get()
	if !ok {
		return authz.ScopeRef{}, false
	}
	id, err := uuid.Parse(text)
	if err != nil {
		return authz.ScopeRef{}, false
	}
	switch b.ScopeKind {
	case model.ScopeProject:
		return authz.ScopeRef{Level: authz.LevelProject, ID: id}, true
	case model.ScopeEnvironment:
		return authz.ScopeRef{Level: authz.LevelEnvironment, ID: id}, true
	case model.ScopeApp:
		return authz.ScopeRef{Level: authz.LevelApp, ID: id}, true
	case model.ScopeOrg:
	}
	return authz.ScopeRef{}, false
}

// Require is a proof that the caller holds p on chain, or forbidden.
func (a Access) Require(p perm.Perm, chain authz.ScopeChain) (authz.Proof, error) {
	return authz.Check(a.policy, a.Subject, p, chain) //nolint:wrapcheck // a kerrors already
}

// OrgIDs are the caller's organizations (for a token: exactly its own).
func (a Access) OrgIDs() []ids.OrgID { return slices.Clone(a.orgs) }

// OrgRole is the caller's strongest org-level role in org (none for a
// scoped token).
func (a Access) OrgRole(org ids.OrgID) opt.Val[perm.Role] {
	best := opt.None[perm.Role]()
	for _, b := range a.Subject.Bindings {
		if b.Scope != authz.OrgScope(org) {
			continue
		}
		if cur, ok := best.Get(); !ok || b.Role.Rank() > cur.Rank() {
			best = opt.Some(b.Role)
		}
	}
	return best
}

// ForbidToken refuses account management through an API token: a leaked
// token must not mint tokens, add members or change passwords.
func (a Access) ForbidToken() error {
	if a.Current.Token.IsSome() {
		return kerrors.ErrForbidden
	}
	return nil
}

// Actor is the caller as operations and audit records name it: its kind
// (`user` or `token`) and `kind:<user id>`.
func (a Access) Actor() (kind, ref string) {
	kind = "user"
	if a.Current.Token.IsSome() {
		kind = "token"
	}
	return kind, kind + ":" + a.Current.User.ID.String()
}

// Restrict is an API token's effective bindings: every role capped at the
// token's role; with a project or environment scope, the owner's org-level
// authority re-rooted at that node and bindings below org level dropped.
func Restrict(bindings []authz.Binding, scope model.TokenScope) []authz.Binding {
	narrowed := opt.None[authz.ScopeRef]()
	if env, ok := scope.Environment.Get(); ok {
		narrowed = opt.Some(authz.ScopeRef{Level: authz.LevelEnvironment, ID: env})
	} else if project, ok := scope.Project.Get(); ok {
		narrowed = opt.Some(authz.ScopeRef{Level: authz.LevelProject, ID: project})
	}
	out := make([]authz.Binding, 0, len(bindings))
	for _, b := range bindings {
		role := b.Role.Weaker(scope.Role)
		node, isNarrowed := narrowed.Get()
		switch {
		case !isNarrowed:
			out = append(out, authz.Binding{Scope: b.Scope, Role: role})
		case b.Scope.Level == authz.LevelOrg:
			out = append(out, authz.Binding{Scope: node, Role: role})
		}
	}
	return out
}

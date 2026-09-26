package httpapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
)

// Roles on one project or environment (routes/access.rs, M4.1). A scoped
// role adds to what a member holds on the organization; it never makes
// anyone a member. Only people manage roles (no API tokens), nobody grants
// or removes a role stronger than their own on that node, and nobody
// changes their own.

// node is a project or environment, with what authorizes changes there.
type node struct {
	org   ids.OrgID
	kind  model.ScopeKind
	id    uuid.UUID
	chain authz.ScopeChain
}

func (s *Server) projectNode(ctx context.Context, a access.Access, project string) (node, error) {
	p, err := s.findProject(ctx, a, project)
	if err != nil {
		return node{}, err
	}
	return node{org: p.org, kind: model.ScopeProject, id: p.project.ID.UUID(), chain: p.chain()}, nil
}

func (s *Server) environmentNode(ctx context.Context, a access.Access, project, env string) (node, error) {
	e, err := s.findEnvironment(ctx, a, project, env)
	if err != nil {
		return node{}, err
	}
	return node{org: e.project.org, kind: model.ScopeEnvironment, id: e.env.ID.UUID(), chain: e.chain()}, nil
}

// MemberID reads a member path segment: a user id, or not found.
func MemberID(member string) (ids.UserID, error) {
	id, err := ids.Parse[ids.User](member)
	if err != nil {
		return ids.UserID{}, kerrors.New(kerrors.NotFound, "member `%s`", member)
	}
	return id, nil
}

// MayAssign reports whether a caller holding caller on a node may grant
// granted there over someone who holds current there now (none: no role).
func MayAssign(caller, current, granted opt.Val[perm.Role]) bool {
	c, ok := caller.Get()
	if !ok {
		return false
	}
	within := func(r opt.Val[perm.Role]) bool {
		role, ok := r.Get()
		return !ok || role.Rank() <= c.Rank()
	}
	return within(current) && within(granted)
}

func (s *Server) listScoped(ctx context.Context, a access.Access, n node) ([]gen.MemberDto, error) {
	if _, err := a.Require(perm.OrgRead, n.chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	members, err := s.deps.Store.ScopedMembers(ctx, n.org, n.kind, n.id)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return memberDtos(members), nil
}

// scopedRole is user's role bound on n, if any.
func (s *Server) scopedRole(ctx context.Context, n node, user ids.UserID) (model.Member, bool, error) {
	members, err := s.deps.Store.ScopedMembers(ctx, n.org, n.kind, n.id)
	if err != nil {
		return model.Member{}, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, m := range members {
		if m.User.ID == user {
			return m, true, nil
		}
	}
	return model.Member{}, false, nil
}

func (s *Server) putScoped(ctx context.Context, a access.Access, n node, member, requested string) (gen.MemberDto, error) {
	if err := a.ForbidToken(); err != nil {
		return gen.MemberDto{}, err //nolint:wrapcheck // a kerrors already
	}
	if _, err := a.Require(perm.UserAdmin, n.chain); err != nil {
		return gen.MemberDto{}, err //nolint:wrapcheck // a kerrors already
	}
	user, err := MemberID(member)
	if err != nil {
		return gen.MemberDto{}, err
	}
	if user == a.Current.User.ID {
		return gen.MemberDto{}, kerrors.New(kerrors.Conflict, "you cannot change your own role")
	}
	role, err := perm.ParseRole(requested)
	if err != nil {
		return gen.MemberDto{}, err //nolint:wrapcheck // a kerrors already
	}
	current, found, err := s.scopedRole(ctx, n, user)
	if err != nil {
		return gen.MemberDto{}, err
	}
	currentRole := opt.None[perm.Role]()
	if found {
		currentRole = opt.Some(current.Role)
	}
	if !MayAssign(authz.EffectiveRole(a.Subject, n.chain), currentRole, opt.Some(role)) {
		return gen.MemberDto{}, kerrors.ErrForbidden
	}
	bound, err := s.deps.Store.BindScopedRole(ctx, n.org, user, n.kind, n.id, role)
	if err != nil {
		return gen.MemberDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !bound {
		return gen.MemberDto{}, kerrors.New(kerrors.NotFound, "member `%s`", member)
	}
	now, found, err := s.scopedRole(ctx, n, user)
	if err != nil {
		return gen.MemberDto{}, err
	}
	if !found {
		return gen.MemberDto{}, kerrors.New(kerrors.Internal, "the bound role is missing")
	}
	return memberDto(now), nil
}

func (s *Server) removeScoped(ctx context.Context, a access.Access, n node, member string) error {
	if err := a.ForbidToken(); err != nil {
		return err //nolint:wrapcheck // a kerrors already
	}
	if _, err := a.Require(perm.UserAdmin, n.chain); err != nil {
		return err //nolint:wrapcheck // a kerrors already
	}
	user, err := MemberID(member)
	if err != nil {
		return err
	}
	if user == a.Current.User.ID {
		return kerrors.New(kerrors.Conflict, "you cannot remove your own role")
	}
	current, found, err := s.scopedRole(ctx, n, user)
	if err != nil {
		return err
	}
	if !found {
		return kerrors.New(kerrors.NotFound, "a role of member `%s` here", member)
	}
	if !MayAssign(authz.EffectiveRole(a.Subject, n.chain), opt.Some(current.Role), opt.None[perm.Role]()) {
		return kerrors.ErrForbidden
	}
	if _, err := s.deps.Store.UnbindScopedRole(ctx, n.org, user, n.kind, n.id); err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	return nil
}

// ListProjectMembers is the members with a role on this project.
func (s *Server) ListProjectMembers(ctx context.Context, params gen.ListProjectMembersParams) (gen.ListProjectMembersRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.projectNode(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	members, err := s.listScoped(ctx, a, n)
	if err != nil {
		return nil, err
	}
	out := gen.ListProjectMembersOKApplicationJSON(members)
	return &out, nil
}

// PutProjectMember gives an organization member a role on this project,
// or changes it.
func (s *Server) PutProjectMember(
	ctx context.Context, req *gen.PutScopedRole, params gen.PutProjectMemberParams,
) (gen.PutProjectMemberRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.projectNode(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	dto, err := s.putScoped(ctx, a, n, params.Member, req.Role)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// RemoveProjectMember removes a member's role on this project; their
// organization role stays.
func (s *Server) RemoveProjectMember(ctx context.Context, params gen.RemoveProjectMemberParams) (gen.RemoveProjectMemberRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.projectNode(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	if err := s.removeScoped(ctx, a, n, params.Member); err != nil {
		return nil, err
	}
	return &gen.RemoveProjectMemberNoContent{}, nil
}

// ListEnvironmentMembers is the members with a role on this environment.
func (s *Server) ListEnvironmentMembers(
	ctx context.Context, params gen.ListEnvironmentMembersParams,
) (gen.ListEnvironmentMembersRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.environmentNode(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	members, err := s.listScoped(ctx, a, n)
	if err != nil {
		return nil, err
	}
	out := gen.ListEnvironmentMembersOKApplicationJSON(members)
	return &out, nil
}

// PutEnvironmentMember gives an organization member a role on this
// environment, or changes it.
func (s *Server) PutEnvironmentMember(
	ctx context.Context, req *gen.PutScopedRole, params gen.PutEnvironmentMemberParams,
) (gen.PutEnvironmentMemberRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.environmentNode(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	dto, err := s.putScoped(ctx, a, n, params.Member, req.Role)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// RemoveEnvironmentMember removes a member's role on this environment.
func (s *Server) RemoveEnvironmentMember(
	ctx context.Context, params gen.RemoveEnvironmentMemberParams,
) (gen.RemoveEnvironmentMemberRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.environmentNode(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if err := s.removeScoped(ctx, a, n, params.Member); err != nil {
		return nil, err
	}
	return &gen.RemoveEnvironmentMemberNoContent{}, nil
}

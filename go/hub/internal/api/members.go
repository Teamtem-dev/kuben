package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

// Organization members and roles (routes/members.rs, scenario 4). Nobody
// grants a role above their own; only owners touch owners; the last owner
// can be neither demoted nor removed; removing a member revokes their
// sessions and tokens at once.

// memberDto is MemberDto::from.
func memberDto(m model.Member) gen.MemberDto {
	return gen.MemberDto{
		ID:                 m.User.ID.String(),
		Email:              m.User.Email,
		DisplayName:        optNilString(m.User.DisplayName),
		Role:               m.Role.String(),
		MustChangePassword: m.User.MustChangePassword,
		Active:             m.User.IsActive,
	}
}

func memberDtos(members []model.Member) []gen.MemberDto {
	out := make([]gen.MemberDto, 0, len(members))
	for _, m := range members {
		out = append(out, memberDto(m))
	}
	return out
}

// ValidEmail is validate::email: at most 254 bytes, no whitespace, a
// non-empty local part and a domain with a dot and no second `@`.
func ValidEmail(value string) error {
	local, domain, found := strings.Cut(value, "@")
	ok := len(value) <= 254 && !strings.ContainsFunc(value, unicode.IsSpace) &&
		found && local != "" && strings.Contains(domain, ".") && !strings.Contains(domain, "@")
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid email address", value)
	}
	return nil
}

// orgOf is the organization member management acts on: the caller's
// first.
func orgOf(a access.Access) (ids.OrgID, error) {
	orgs := a.OrgIDs()
	if len(orgs) == 0 {
		return ids.OrgID{}, kerr.ErrForbidden
	}
	return orgs[0], nil
}

// callerRole is the caller's own org role, required for every change.
func callerRole(a access.Access, org ids.OrgID) (perm.Role, error) {
	role, ok := a.OrgRole(org).Get()
	if !ok {
		return "", kerr.ErrForbidden
	}
	return role, nil
}

// memberAdmin is the access, organization and proof of user-admin every
// member change starts with; tokens never manage members.
func (s *Server) memberAdmin(ctx context.Context) (access.Access, ids.OrgID, error) {
	a, err := s.access(ctx)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if err := a.ForbidToken(); err != nil {
		return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerr already
	}
	org, err := orgOf(a)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if _, err := a.Require(perm.UserAdmin, authz.OrgChain(org)); err != nil {
		return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerr already
	}
	return a, org, nil
}

func (s *Server) findMember(ctx context.Context, org ids.OrgID, id string) (model.Member, error) {
	notFound := kerr.New(kerr.NotFound, "member `%s`", id)
	user, err := ids.Parse[ids.User](id)
	if err != nil {
		return model.Member{}, notFound
	}
	members, err := s.deps.Store.ListMembers(ctx, org)
	if err != nil {
		return model.Member{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, m := range members {
		if m.User.ID == user {
			return m, nil
		}
	}
	return model.Member{}, notFound
}

// ListMembers is the members of the caller's organization.
func (s *Server) ListMembers(ctx context.Context) (gen.ListMembersRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	org, err := orgOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.OrgRead, authz.OrgChain(org)); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	members, err := s.deps.Store.ListMembers(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := gen.ListMembersOKApplicationJSON(memberDtos(members))
	return &out, nil
}

// InviteMember invites a member. New accounts get a one-time temporary
// password.
func (s *Server) InviteMember(ctx context.Context, req *gen.InviteMember) (gen.InviteMemberRes, error) {
	a, org, err := s.memberAdmin(ctx)
	if err != nil {
		return nil, err
	}
	role, err := perm.ParseRole(req.Role.Or(string(perm.Developer)))
	if err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	caller, err := callerRole(a, org)
	if err != nil {
		return nil, err
	}
	if role.Rank() > caller.Rank() {
		return nil, kerr.ErrForbidden
	}
	email := ascii.Lower(strings.TrimSpace(req.Email))
	if err := ValidEmail(email); err != nil {
		return nil, err
	}
	user, temporary, err := s.inviteeAccount(ctx, org, email, req.DisplayName)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Store.AddMembership(ctx, org, user.ID); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := s.deps.Store.BindOrgRole(ctx, org, user.ID, role); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.InvitedMember{
		Member:            memberDto(model.Member{User: user, Role: role}),
		TemporaryPassword: optNilString(temporary),
	}, nil
}

// inviteeAccount is the existing account of email (not yet a member of
// org), or a new one with a temporary password it must replace at the
// first login.
func (s *Server) inviteeAccount(
	ctx context.Context, org ids.OrgID, email string, displayName gen.OptNilString,
) (model.User, opt.Val[string], error) {
	existing, found, err := s.deps.Store.FindUserByEmail(ctx, email)
	if err != nil {
		return model.User{}, opt.None[string](), err //nolint:wrapcheck // a store error, answered as internal
	}
	if found {
		members, err := s.deps.Store.ListMembers(ctx, org)
		if err != nil {
			return model.User{}, opt.None[string](), err //nolint:wrapcheck // a store error, answered as internal
		}
		for _, m := range members {
			if m.User.ID == existing.User.ID {
				return model.User{}, opt.None[string](), kerr.New(kerr.Conflict, "`%s` is already a member", email)
			}
		}
		return existing.User, opt.None[string](), nil
	}
	var b [18]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails (Go ≥ 1.24)
	password := base64.RawURLEncoding.EncodeToString(b[:])
	var hash string
	var hashErr error
	if err := s.withPermit(ctx, func() { hash, hashErr = s.deps.Hasher.Hash(password) }); err != nil {
		return model.User{}, opt.None[string](), err
	}
	if hashErr != nil {
		return model.User{}, opt.None[string](), kerr.Wrap(hashErr, "hash the temporary password")
	}
	name := opt.None[string]()
	if n, ok := displayName.Get(); ok {
		if n = strings.TrimSpace(n); n != "" {
			name = opt.Some(n)
		}
	}
	user, err := s.deps.Store.CreateInvitedUser(ctx, email, name, opt.Some(hash))
	if err != nil {
		return model.User{}, opt.None[string](), err //nolint:wrapcheck // a store error, answered as internal
	}
	return user, opt.Some(password), nil
}

// UpdateMember changes a member's role.
func (s *Server) UpdateMember(ctx context.Context, req *gen.UpdateMember, params gen.UpdateMemberParams) (gen.UpdateMemberRes, error) {
	a, org, err := s.memberAdmin(ctx)
	if err != nil {
		return nil, err
	}
	role, err := perm.ParseRole(req.Role)
	if err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	target, err := s.findMember(ctx, org, params.Member)
	if err != nil {
		return nil, err
	}
	if target.User.ID == a.Current.User.ID {
		return nil, kerr.New(kerr.Conflict, "you cannot change your own role")
	}
	caller, err := callerRole(a, org)
	if err != nil {
		return nil, err
	}
	touchesOwner := target.Role == perm.Owner || role == perm.Owner
	if (touchesOwner && caller != perm.Owner) || role.Rank() > caller.Rank() {
		return nil, kerr.ErrForbidden
	}
	if target.Role == perm.Owner && role != perm.Owner {
		owners, err := s.deps.Store.CountOwners(ctx, org)
		if err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		if owners <= 1 {
			return nil, kerr.New(kerr.Conflict, "the last owner cannot be demoted")
		}
	}
	if err := s.deps.Store.SetOrgRole(ctx, org, target.User.ID, role); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := memberDto(model.Member{User: target.User, Role: role})
	return &dto, nil
}

// RemoveMember removes a member: bindings, sessions and tokens are revoked
// immediately.
func (s *Server) RemoveMember(ctx context.Context, params gen.RemoveMemberParams) (gen.RemoveMemberRes, error) {
	a, org, err := s.memberAdmin(ctx)
	if err != nil {
		return nil, err
	}
	target, err := s.findMember(ctx, org, params.Member)
	if err != nil {
		return nil, err
	}
	if target.User.ID == a.Current.User.ID {
		return nil, kerr.New(kerr.Conflict, "you cannot remove yourself")
	}
	if target.Role == perm.Owner {
		caller, err := callerRole(a, org)
		if err != nil {
			return nil, err
		}
		if caller != perm.Owner {
			return nil, kerr.ErrForbidden
		}
		owners, err := s.deps.Store.CountOwners(ctx, org)
		if err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		if owners <= 1 {
			return nil, kerr.New(kerr.Conflict, "the last owner cannot be removed")
		}
	}
	// Bindings, membership and the member's tokens of this org go together.
	if err := s.deps.Store.RemoveMember(ctx, org, target.User.ID); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if _, err := s.deps.Store.RevokeAllSessions(ctx, target.User.ID); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	s.sessionCache.Purge()
	return &gen.RemoveMemberNoContent{}, nil
}

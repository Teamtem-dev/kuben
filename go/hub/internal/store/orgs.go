package store

// Organizations, memberships and role bindings; the port of repo/orgs.rs.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

const (
	selectMembers = "SELECT u.id AS id, u.email AS email, u.display_name AS display_name, " +
		"u.is_active AS is_active, u.must_change_password AS must_change_password, u.created_at AS created_at, " +
		"rb.role AS role FROM role_bindings rb JOIN users u ON u.id = rb.subject_id " +
		"WHERE rb.org_id = $1 AND rb.subject_kind = 'user' AND rb.scope_kind = 'org' ORDER BY u.email"
	updateOrgRole = "UPDATE role_bindings SET role = $3 " +
		"WHERE org_id = $1 AND subject_kind = 'user' AND subject_id = $2 AND scope_kind = 'org'"
	deleteUserBindings = "DELETE FROM role_bindings WHERE org_id = $1 AND subject_kind = 'user' AND subject_id = $2"
	deleteMembership   = "DELETE FROM memberships WHERE org_id = $1 AND user_id = $2"
	revokeMemberTokens = "UPDATE api_tokens SET revoked_at = $3 " +
		"WHERE org_id = $1 AND owner_user_id = $2 AND revoked_at IS NULL"
	upsertScopedBinding = "INSERT INTO role_bindings " +
		"(id, org_id, subject_kind, subject_id, role, scope_kind, scope_uid, created_at) " +
		"SELECT $1, $2, 'user', $3, $4, $5, $6, $7 " +
		"WHERE EXISTS (SELECT 1 FROM memberships WHERE org_id = $2 AND user_id = $3) " +
		"ON CONFLICT (org_id, subject_kind, subject_id, scope_kind, (COALESCE(scope_uid, ''))) " +
		"DO UPDATE SET role = EXCLUDED.role"
	deleteScopedBinding = "DELETE FROM role_bindings " +
		"WHERE org_id = $1 AND subject_kind = 'user' AND subject_id = $2 AND scope_kind = $3 AND scope_uid = $4"
	selectScopedMembers = "SELECT u.id AS id, u.email AS email, u.display_name AS display_name, " +
		"u.is_active AS is_active, u.must_change_password AS must_change_password, u.created_at AS created_at, " +
		"rb.role AS role FROM role_bindings rb JOIN users u ON u.id = rb.subject_id " +
		"WHERE rb.org_id = $1 AND rb.subject_kind = 'user' AND rb.scope_kind = $2 AND rb.scope_uid = $3 " +
		"ORDER BY u.email"
	countOwners = "SELECT COUNT(*) FROM role_bindings " +
		"WHERE org_id = $1 AND subject_kind = 'user' AND scope_kind = 'org' AND role = 'owner'"

	insertOrg        = "INSERT INTO organizations (id, slug, name, created_at) VALUES ($1, $2, $3, $4)"
	selectOrgBySlug  = "SELECT id, slug, name, created_at FROM organizations WHERE slug = $1"
	insertMembership = "INSERT INTO memberships (org_id, user_id) VALUES ($1, $2)"
	insertBinding    = "INSERT INTO role_bindings (id, org_id, subject_kind, subject_id, role, scope_kind, scope_uid, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"
	selectBindingsForUser = "SELECT org_id, subject_kind, subject_id, role, scope_kind, scope_uid FROM role_bindings " +
		"WHERE subject_kind = 'user' AND subject_id = $1"
)

// errOrgRoleNotScoped refuses an org-level role where a scoped one is
// meant (Rust: sqlx::Error::Protocol).
var errOrgRoleNotScoped = errors.New("encountered unexpected or invalid data: an org role is not scoped")

// parseRole reads a stored role; an unknown one is a decode error.
func parseRole(op, s string) (perm.Role, error) {
	r, err := perm.ParseRole(s)
	if err != nil {
		return "", decodeErr(op, "%v", err)
	}
	return r, nil
}

// CreateOrg creates an organization.
func (s *Store) CreateOrg(ctx context.Context, slug, name string) (model.Organization, error) {
	org := model.Organization{ID: ids.New[ids.Org](), Slug: slug, Name: name, CreatedAt: s.now()}
	_, err := exec(ctx, s.db, "create an organization", insertOrg, org.ID.String(), org.Slug, org.Name, org.CreatedAt)
	if err != nil {
		return model.Organization{}, err
	}
	return org, nil
}

// FindOrgBySlug is the organization slug.
func (s *Store) FindOrgBySlug(ctx context.Context, slug string) (model.Organization, bool, error) {
	return queryOpt(ctx, s.db, "find an organization", selectOrgBySlug,
		func(row pgx.CollectableRow) (model.Organization, error) {
			var o model.Organization
			err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
			return o, err
		}, slug)
}

// AddMembership makes user a member of org.
func (s *Store) AddMembership(ctx context.Context, org ids.OrgID, user ids.UserID) error {
	_, err := exec(ctx, s.db, "add a membership", insertMembership, org.String(), user.String())
	return err
}

// BindOrgRole binds role for user at org scope.
func (s *Store) BindOrgRole(ctx context.Context, org ids.OrgID, user ids.UserID, role perm.Role) error {
	id, err := uuid.NewV7()
	if err != nil {
		return dbErr("bind an org role", err)
	}
	_, err = exec(ctx, s.db, "bind an org role", insertBinding,
		id.String(), org.String(), model.SubjectUser.String(), user.String(), role.String(),
		model.ScopeOrg.String(), (*string)(nil), s.now())
	return err
}

// BindingsForUser is every role binding whose subject is user.
func (s *Store) BindingsForUser(ctx context.Context, user ids.UserID) ([]model.RoleBinding, error) {
	const op = "list a user's role bindings"
	return queryAll(ctx, s.db, op, selectBindingsForUser, func(row pgx.CollectableRow) (model.RoleBinding, error) {
		var b model.RoleBinding
		var subjectKind, role, scopeKind string
		var scopeUID *string
		if err := row.Scan(&b.OrgID, &subjectKind, &b.SubjectID, &role, &scopeKind, &scopeUID); err != nil {
			return model.RoleBinding{}, err
		}
		// Unknown kinds read as the default, as in Rust.
		switch subjectKind {
		case "team":
			b.SubjectKind = model.SubjectTeam
		case "token":
			b.SubjectKind = model.SubjectToken
		default:
			b.SubjectKind = model.SubjectUser
		}
		var err error
		if b.Role, err = parseRole(op, role); err != nil {
			return model.RoleBinding{}, err
		}
		switch scopeKind {
		case "project":
			b.ScopeKind = model.ScopeProject
		case "environment":
			b.ScopeKind = model.ScopeEnvironment
		case "app":
			b.ScopeKind = model.ScopeApp
		default:
			b.ScopeKind = model.ScopeOrg
		}
		b.ScopeUID = opt.FromPtr(scopeUID)
		return b, nil
	}, user.String())
}

// ListMembers is the members of org with their org-level role, ordered by
// email.
func (s *Store) ListMembers(ctx context.Context, org ids.OrgID) ([]model.Member, error) {
	return queryAll(ctx, s.db, "list members", selectMembers, scanMember, org.String())
}

// BindScopedRole binds role for user on one project or environment
// (M4.1), or changes the role bound there. False when user is not a member
// of org: a scoped role never makes anyone a member.
func (s *Store) BindScopedRole(
	ctx context.Context, org ids.OrgID, user ids.UserID, scope model.ScopeKind, node uuid.UUID, role perm.Role,
) (bool, error) {
	const op = "bind a scoped role"
	if scope == model.ScopeOrg {
		return false, dbErr(op, errOrgRoleNotScoped)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return false, dbErr(op, err)
	}
	n, err := exec(ctx, s.db, op, upsertScopedBinding,
		id.String(), org.String(), user.String(), role.String(), scope.String(), node.String(), s.now())
	return n == 1, err
}

// UnbindScopedRole removes user's role on one project or environment.
// False when there was none.
func (s *Store) UnbindScopedRole(ctx context.Context, org ids.OrgID, user ids.UserID, scope model.ScopeKind, node uuid.UUID) (bool, error) {
	n, err := exec(ctx, s.db, "unbind a scoped role", deleteScopedBinding,
		org.String(), user.String(), scope.String(), node.String())
	return n > 0, err
}

// ScopedMembers is the members with a role bound on one project or
// environment, by email.
func (s *Store) ScopedMembers(ctx context.Context, org ids.OrgID, scope model.ScopeKind, node uuid.UUID) ([]model.Member, error) {
	return queryAll(ctx, s.db, "list scoped members", selectScopedMembers, scanMember,
		org.String(), scope.String(), node.String())
}

// SetOrgRole changes a member's org-level role (creating the binding if
// missing).
func (s *Store) SetOrgRole(ctx context.Context, org ids.OrgID, user ids.UserID, role perm.Role) error {
	updated, err := exec(ctx, s.db, "set an org role", updateOrgRole, org.String(), user.String(), role.String())
	if err != nil {
		return err
	}
	if updated == 0 {
		return s.BindOrgRole(ctx, org, user, role)
	}
	return nil
}

// RemoveMember removes every binding and the membership of user in org and
// revokes their API tokens of org, atomically (S04): nothing they held there
// keeps working after the commit.
func (s *Store) RemoveMember(ctx context.Context, org ids.OrgID, user ids.UserID) (err error) {
	const op = "remove a member"
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return dbErr(op, err)
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, dbErr("roll back", rbErr))
			}
		}
	}()
	if _, err := exec(ctx, tx, op, revokeMemberTokens, org.String(), user.String(), s.now()); err != nil {
		return err
	}
	if _, err := exec(ctx, tx, op, deleteUserBindings, org.String(), user.String()); err != nil {
		return err
	}
	if _, err := exec(ctx, tx, op, deleteMembership, org.String(), user.String()); err != nil {
		return err
	}
	return dbErr(op, tx.Commit(ctx))
}

// CountOwners is the number of org-level owners of org.
func (s *Store) CountOwners(ctx context.Context, org ids.OrgID) (int64, error) {
	var n int64
	err := queryOne(ctx, s.db, "count owners", countOwners, []any{&n}, org.String())
	return n, err
}

func scanMember(row pgx.CollectableRow) (model.Member, error) {
	var m model.Member
	var displayName *string
	var role string
	err := row.Scan(&m.User.ID, &m.User.Email, &displayName, &m.User.IsActive,
		&m.User.MustChangePassword, &m.User.CreatedAt, &role)
	if err != nil {
		return model.Member{}, err
	}
	m.User.DisplayName = opt.FromPtr(displayName)
	if m.Role, err = parseRole("read a member", role); err != nil {
		return model.Member{}, err
	}
	return m, nil
}

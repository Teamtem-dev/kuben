package store

// Users; the port of repo/users.rs.

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const (
	insertUser = "INSERT INTO users " +
		"(id, email, display_name, password_hash, is_active, must_change_password, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7)"
	selectUserCols    = "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users"
	selectUserByEmail = "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users WHERE email = $1"
	selectUserByID    = "SELECT id, email, display_name, password_hash, is_active, must_change_password, created_at FROM users WHERE id = $1"
	countUsers        = "SELECT COUNT(*) FROM users"
	updatePassword    = "UPDATE users SET password_hash = $2, must_change_password = FALSE WHERE id = $1" //nolint:gosec // SQL, not a credential
)

// scanUser reads a users row. The id column is text holding a UUID; a text
// that is not one is a decode error, as in Rust.
func scanUser(row pgx.CollectableRow) (model.UserCredentials, error) {
	var c model.UserCredentials
	var displayName, passwordHash *string
	err := row.Scan(&c.User.ID, &c.User.Email, &displayName, &passwordHash,
		&c.User.IsActive, &c.User.MustChangePassword, &c.User.CreatedAt)
	if err != nil {
		return model.UserCredentials{}, err
	}
	c.User.DisplayName = opt.FromPtr(displayName)
	c.PasswordHash = opt.FromPtr(passwordHash)
	return c, nil
}

// normalizeEmail is how emails are stored and looked up: trimmed and ASCII
// lowercase.
func normalizeEmail(email string) string { return ascii.Lower(strings.TrimSpace(email)) }

// CreateUser creates a user. passwordHash is a PHC string produced by the
// API layer (Argon2id): the store never sees plaintext.
func (s *Store) CreateUser(ctx context.Context, email string, displayName, passwordHash opt.Val[string]) (model.User, error) {
	return s.insertUser(ctx, email, displayName, passwordHash, false)
}

// CreateInvitedUser creates a user who must replace the (temporary)
// password on first login.
func (s *Store) CreateInvitedUser(ctx context.Context, email string, displayName, passwordHash opt.Val[string]) (model.User, error) {
	return s.insertUser(ctx, email, displayName, passwordHash, true)
}

func (s *Store) insertUser(
	ctx context.Context, email string, displayName, passwordHash opt.Val[string], mustChangePassword bool,
) (model.User, error) {
	user := model.User{
		ID:                 ids.New[ids.User](),
		Email:              normalizeEmail(email),
		DisplayName:        displayName,
		IsActive:           true,
		MustChangePassword: mustChangePassword,
		CreatedAt:          s.now(),
	}
	_, err := exec(ctx, s.db, "create a user", insertUser,
		user.ID.String(), user.Email, user.DisplayName.Ptr(), passwordHash.Ptr(),
		user.IsActive, user.MustChangePassword, user.CreatedAt)
	if err != nil {
		return model.User{}, err
	}
	return user, nil
}

// FindUserByEmail is the user with email (normalised) and its password
// hash.
func (s *Store) FindUserByEmail(ctx context.Context, email string) (model.UserCredentials, bool, error) {
	return queryOpt(ctx, s.db, "find a user by email", selectUserByEmail, scanUser, normalizeEmail(email))
}

// FindUserByID is the user id.
func (s *Store) FindUserByID(ctx context.Context, id ids.UserID) (model.User, bool, error) {
	c, ok, err := queryOpt(ctx, s.db, "find a user", selectUserByID, scanUser, id.String())
	return c.User, ok, err
}

// ListUsers is every user.
func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	creds, err := queryAll(ctx, s.db, "list users", selectUserCols, scanUser)
	if err != nil {
		return nil, err
	}
	users := make([]model.User, 0, len(creds))
	for _, c := range creds {
		users = append(users, c.User)
	}
	return users, nil
}

// CountUsers is the number of users.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := queryOne(ctx, s.db, "count users", countUsers, []any{&n})
	return n, err
}

// SetPasswordHash replaces a user's password hash and clears
// must_change_password.
func (s *Store) SetPasswordHash(ctx context.Context, id ids.UserID, passwordHash string) error {
	_, err := exec(ctx, s.db, "set a password", updatePassword, id.String(), passwordHash)
	return err
}

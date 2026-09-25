package store

// Single sign-on state and accounts (M4.3, migration 0021); the port of
// repo/sso.rs.
//
// A sign-in links the provider subject to one user (created, or found by
// the verified email), makes them a member of the organization and sets
// their organization role to the one the provider's groups earn, all in
// one transaction.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/sso"
)

const (
	ssoPurge = "DELETE FROM sso_logins WHERE expires_at < $1"
	ssoBegin = "INSERT INTO sso_logins (state_hash, nonce, verifier, return_to, created_at, expires_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6)"
	ssoTake = "DELETE FROM sso_logins WHERE state_hash = $1 AND expires_at > $2 " +
		"RETURNING nonce, verifier, return_to"
	ssoIdentity    = "SELECT user_id FROM identities WHERE provider = $1 AND subject = $2 FOR UPDATE"
	ssoUserByEmail = "SELECT id FROM users WHERE email = $1 FOR UPDATE"
	ssoInsertUser  = "INSERT INTO users " +
		"(id, email, display_name, password_hash, is_active, must_change_password, created_at) " +
		"VALUES ($1, $2, $3, NULL, TRUE, FALSE, $4)"
	ssoLink = "INSERT INTO identities (id, user_id, provider, subject, email, created_at, last_login_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $6)"
	ssoTouch = "UPDATE identities SET email = $3, last_login_at = $4 WHERE provider = $1 AND subject = $2"
	ssoUser  = "SELECT id, email, display_name, is_active, must_change_password, created_at " +
		"FROM users WHERE id = $1"
	ssoJoin    = "INSERT INTO memberships (org_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING"
	ssoSetRole = "INSERT INTO role_bindings " +
		"(id, org_id, subject_kind, subject_id, role, scope_kind, scope_uid, created_at) " +
		"VALUES ($1, $2, 'user', $3, $4, 'org', NULL, $5) " +
		"ON CONFLICT (org_id, subject_kind, subject_id, scope_kind, (COALESCE(scope_uid, ''))) " +
		"DO UPDATE SET role = EXCLUDED.role"
	ssoHasIdentity = "SELECT EXISTS (SELECT 1 FROM identities WHERE user_id = $1)"
)

// PendingSSO is what a started sign-in remembers.
type PendingSSO struct {
	Nonce    string
	Verifier string
	ReturnTo string
}

// SSOSignIn is the outcome of [Store.SSOSignIn].
//
//sumtype:decl
type SSOSignIn interface{ ssoSignIn() }

// SSOSignedIn is a person signed in as User.
type SSOSignedIn struct{ User model.User }

// SSOInactive is a deactivated account: nothing changed.
type SSOInactive struct{}

func (SSOSignedIn) ssoSignIn() {}
func (SSOInactive) ssoSignIn() {}

// BeginSSO remembers a started sign-in until expiresAt (Unix
// milliseconds), dropping expired ones first.
func (s *Store) BeginSSO(ctx context.Context, stateHash []byte, pending PendingSSO, expiresAt int64) (err error) {
	const op = "begin a single sign-on"
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	if _, err := exec(ctx, tx, op, ssoPurge, now); err != nil {
		return err
	}
	if _, err := exec(ctx, tx, op, ssoBegin, stateHash, pending.Nonce, pending.Verifier, pending.ReturnTo,
		now, expiresAt); err != nil {
		return err
	}
	return dbErr(op, tx.Commit(ctx))
}

// TakeSSO is the started sign-in of stateHash, once, while it is valid.
func (s *Store) TakeSSO(ctx context.Context, stateHash []byte) (PendingSSO, bool, error) {
	return queryOpt(ctx, s.db, "take a single sign-on", ssoTake, func(row pgx.CollectableRow) (PendingSSO, error) {
		var p PendingSSO
		err := row.Scan(&p.Nonce, &p.Verifier, &p.ReturnTo)
		return p, err
	}, stateHash, s.now())
}

// SSOSignIn signs person in through provider into org. A deactivated
// account changes nothing (the transaction is rolled back).
func (s *Store) SSOSignIn(ctx context.Context, org ids.OrgID, provider string, person sso.Person) (out SSOSignIn, err error) {
	const op = "sign in with single sign-on"
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	userID, err := s.ssoAccount(ctx, tx, provider, person, now)
	if err != nil {
		return nil, err
	}
	user, found, err := queryOpt(ctx, tx, op, ssoUser, scanSSOUser, userID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, dbErr(op, pgx.ErrNoRows)
	}
	if !user.IsActive {
		return SSOInactive{}, nil
	}
	if _, err := exec(ctx, tx, op, ssoJoin, org.String(), userID); err != nil {
		return nil, err
	}
	bindingID, err := uuid.NewV7()
	if err != nil {
		return nil, dbErr(op, err)
	}
	if _, err := exec(ctx, tx, op, ssoSetRole, bindingID.String(), org.String(), userID,
		person.Role.String(), now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, dbErr(op, err)
	}
	return SSOSignedIn{User: user}, nil
}

// ssoAccount is the id of the user person's subject is linked to: the
// linked one (its email and last sign-in refreshed), else the user with
// the email, else a new passwordless user, linked now.
func (s *Store) ssoAccount(ctx context.Context, tx pgx.Tx, provider string, person sso.Person, now int64) (string, error) {
	const op = "sign in with single sign-on"
	linked, found, err := queryOpt(ctx, tx, op, ssoIdentity, pgx.RowTo[string], provider, person.Subject)
	if err != nil {
		return "", err
	}
	if found {
		_, err := exec(ctx, tx, op, ssoTouch, provider, person.Subject, person.Email, now)
		return linked, err
	}
	id, found, err := queryOpt(ctx, tx, op, ssoUserByEmail, pgx.RowTo[string], person.Email)
	if err != nil {
		return "", err
	}
	if !found {
		id = ids.New[ids.User]().String()
		if _, err := exec(ctx, tx, op, ssoInsertUser, id, person.Email, person.Name.Ptr(), now); err != nil {
			return "", err
		}
	}
	identity, err := uuid.NewV7()
	if err != nil {
		return "", dbErr(op, err)
	}
	if _, err := exec(ctx, tx, op, ssoLink, identity.String(), id, provider, person.Subject, person.Email, now); err != nil {
		return "", err
	}
	return id, nil
}

// scanSSOUser reads a users row without its password hash; an id that is
// not a UUID is a decode error, as in Rust.
func scanSSOUser(row pgx.CollectableRow) (model.User, error) {
	var u model.User
	var displayName *string
	if err := row.Scan(&u.ID, &u.Email, &displayName, &u.IsActive, &u.MustChangePassword, &u.CreatedAt); err != nil {
		return model.User{}, err
	}
	u.DisplayName = opt.FromPtr(displayName)
	return u, nil
}

// HasIdentity is whether user is linked to an identity provider.
func (s *Store) HasIdentity(ctx context.Context, user ids.UserID) (bool, error) {
	var linked bool
	err := queryOne(ctx, s.db, "check for a linked identity", ssoHasIdentity, []any{&linked}, user.String())
	return linked, err
}

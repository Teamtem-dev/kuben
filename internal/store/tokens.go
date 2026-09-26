package store

// API tokens (scenario 3); the port of repo/tokens.rs. Only sha256(secret)
// is stored; the token id is public and used for the lookup, the secret is
// compared in constant time by the API layer.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// NewToken is the input for a new token.
type NewToken struct {
	ID         ids.TokenID
	OrgID      ids.OrgID
	Owner      ids.UserID
	Name       string
	Prefix     string
	SecretHash []byte
	Scope      model.TokenScope
	ExpiresAt  opt.Val[int64]
}

const (
	insertToken = "INSERT INTO api_tokens " +
		"(id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	selectToken = "SELECT id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, " +
		"last_used_at, revoked_at, created_at FROM api_tokens WHERE id = $1"
	selectTokensOfOwner = "SELECT id, org_id, owner_user_id, name, prefix, secret_hash, scopes, " +
		"expires_at, last_used_at, revoked_at, created_at FROM api_tokens " +
		"WHERE owner_user_id = $1 AND ci_policy_id IS NULL ORDER BY created_at DESC"
	revokeToken      = "UPDATE api_tokens SET revoked_at = $3 WHERE id = $1 AND owner_user_id = $2 AND revoked_at IS NULL"
	revokeUserTokens = "UPDATE api_tokens SET revoked_at = $3 WHERE org_id = $1 AND owner_user_id = $2 AND revoked_at IS NULL"
	touchToken       = "UPDATE api_tokens SET last_used_at = $2 WHERE id = $1"
)

func scanToken(row pgx.CollectableRow) (model.APIToken, error) {
	var t model.APIToken
	var owner *ids.UserID
	var scopes string
	var expires, lastUsed, revoked *int64
	err := row.Scan(&t.ID, &t.OrgID, &owner, &t.Name, &t.Prefix, &t.SecretHash, &scopes,
		&expires, &lastUsed, &revoked, &t.CreatedAt)
	if err != nil {
		return model.APIToken{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scope); err != nil {
		return model.APIToken{}, decodeErr("read a token", "scopes: %v", err)
	}
	t.Owner = opt.FromPtr(owner)
	t.ExpiresAt = opt.FromPtr(expires)
	t.LastUsedAt = opt.FromPtr(lastUsed)
	t.RevokedAt = opt.FromPtr(revoked)
	return t, nil
}

// CreateToken stores a new token.
func (s *Store) CreateToken(ctx context.Context, n NewToken) (model.APIToken, error) {
	const op = "create a token"
	createdAt := s.now()
	scopes, err := json.Marshal(n.Scope)
	if err != nil {
		return model.APIToken{}, dbErr(op, err)
	}
	_, err = exec(ctx, s.db, op, insertToken,
		n.ID.String(), n.OrgID.String(), n.Owner.String(), n.Name, n.Prefix, n.SecretHash, string(scopes),
		n.ExpiresAt.Ptr(), createdAt)
	if err != nil {
		return model.APIToken{}, err
	}
	return model.APIToken{
		ID:         n.ID,
		OrgID:      n.OrgID,
		Owner:      opt.Some(n.Owner),
		Name:       n.Name,
		Prefix:     n.Prefix,
		SecretHash: n.SecretHash,
		Scope:      n.Scope,
		ExpiresAt:  n.ExpiresAt,
		CreatedAt:  createdAt,
	}, nil
}

// FindToken is the token id.
func (s *Store) FindToken(ctx context.Context, id ids.TokenID) (model.APIToken, bool, error) {
	return queryOpt(ctx, s.db, "find a token", selectToken, scanToken, id.String())
}

// ListTokens is owner's personal tokens (not those of CI policies), newest
// first.
func (s *Store) ListTokens(ctx context.Context, owner ids.UserID) ([]model.APIToken, error) {
	return queryAll(ctx, s.db, "list tokens", selectTokensOfOwner, scanToken, owner.String())
}

// RevokeToken revokes one of owner's tokens; false when there was nothing
// to revoke.
func (s *Store) RevokeToken(ctx context.Context, id ids.TokenID, owner ids.UserID) (bool, error) {
	n, err := exec(ctx, s.db, "revoke a token", revokeToken, id.String(), owner.String(), s.now())
	return n > 0, err
}

// RevokeUserTokens revokes every token a user owns in an org (used when a
// member is removed).
func (s *Store) RevokeUserTokens(ctx context.Context, org ids.OrgID, user ids.UserID) (uint64, error) {
	return exec(ctx, s.db, "revoke a user's tokens", revokeUserTokens, org.String(), user.String(), s.now())
}

// TouchToken records that the token was used now.
func (s *Store) TouchToken(ctx context.Context, id ids.TokenID) error {
	_, err := exec(ctx, s.db, "touch a token", touchToken, id.String(), s.now())
	return err
}

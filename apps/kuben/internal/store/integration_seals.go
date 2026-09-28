package store

// The sealed values of integrations (2.1): Git connection tokens and
// webhook secrets, organization registry passwords. The keyring reseals
// their data keys under its current key as it does secret revisions'.

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IntegrationSealKind is which sealed value of an integration a seal is.
type IntegrationSealKind string

// The sealed values of integrations.
const (
	SealGitToken         IntegrationSealKind = "git-token"
	SealGitWebhook       IntegrationSealKind = "git-webhook"
	SealRegistryPassword IntegrationSealKind = "registry-password"
)

const (
	staleIntegrationSeals = "SELECT kind, id, ciphertext, wrapped_key, key_version FROM (" +
		"SELECT 'git-token' AS kind, id, token_ciphertext AS ciphertext, token_wrapped_key AS wrapped_key, " +
		"token_key_version AS key_version FROM git_connections WHERE org_id = $1 AND token_key_version < $2 " +
		"UNION ALL SELECT 'git-webhook', id, webhook_secret_ciphertext, webhook_secret_wrapped_key, " +
		"webhook_secret_key_version FROM git_connections WHERE org_id = $1 AND webhook_secret_key_version < $2 " +
		"UNION ALL SELECT 'registry-password', id, password_ciphertext, password_wrapped_key, " +
		"password_key_version FROM org_registries WHERE org_id = $1 AND password_key_version < $2" +
		") s ORDER BY id, kind LIMIT $3"
	resealGitToken = "UPDATE git_connections SET token_wrapped_key = $3, token_key_version = $4 " +
		"WHERE id = $1 AND org_id = $2 AND token_key_version = $5 AND token_ciphertext = $6"
	resealGitWebhook = "UPDATE git_connections SET webhook_secret_wrapped_key = $3, webhook_secret_key_version = $4 " +
		"WHERE id = $1 AND org_id = $2 AND webhook_secret_key_version = $5 AND webhook_secret_ciphertext = $6"
	resealRegistryPassword = "UPDATE org_registries SET password_wrapped_key = $3, password_key_version = $4 " +
		"WHERE id = $1 AND org_id = $2 AND password_key_version = $5 AND password_ciphertext = $6"
)

// IntegrationSeal is one sealed value of an integration: the Git
// connection's or registry login's id and the value, sealed.
type IntegrationSeal struct {
	Kind   IntegrationSealKind
	ID     uuid.UUID
	Sealed SealedBytes
}

// StaleIntegrationSeals is up to limit sealed values of integrations sealed
// under a key older than current.
func (t *Tenant) StaleIntegrationSeals(ctx context.Context, current uint32, limit int64) ([]IntegrationSeal, error) {
	const op = "read stale integration seals"
	version, err := keyVersionColumn(op, current)
	if err != nil {
		return nil, err
	}
	return queryAll(ctx, t.tx, op, staleIntegrationSeals, func(row pgx.CollectableRow) (IntegrationSeal, error) {
		var (
			s    IntegrationSeal
			kind string
			v    int32
		)
		if err := row.Scan(&kind, &s.ID, &s.Sealed.Ciphertext, &s.Sealed.WrappedKey, &v); err != nil {
			return IntegrationSeal{}, err
		}
		s.Kind = IntegrationSealKind(kind)
		var err error
		s.Sealed.KeyVersion, err = keyVersion(op, v)
		return s, err
	}, t.org.String(), version, limit)
}

// ResealIntegration replaces the sealed data key of stale with resealed,
// unless the value changed meanwhile (false). A resealed value with another
// ciphertext is refused.
func (t *Tenant) ResealIntegration(ctx context.Context, stale IntegrationSeal, resealed SealedBytes) (bool, error) {
	const op = "reseal an integration secret"
	if !bytes.Equal(resealed.Ciphertext, stale.Sealed.Ciphertext) {
		return false, DatabaseError{Op: op, Err: errors.New(
			"encountered unexpected or invalid data: a reseal must keep the ciphertext")}
	}
	var sql string
	switch stale.Kind {
	case SealGitToken:
		sql = resealGitToken
	case SealGitWebhook:
		sql = resealGitWebhook
	case SealRegistryPassword:
		sql = resealRegistryPassword
	default:
		return false, DatabaseError{Op: op, Err: errors.New("unknown integration seal " + string(stale.Kind))}
	}
	to, err := keyVersionColumn(op, resealed.KeyVersion)
	if err != nil {
		return false, err
	}
	from, err := keyVersionColumn(op, stale.Sealed.KeyVersion)
	if err != nil {
		return false, err
	}
	rows, err := exec(ctx, t.tx, op, sql, stale.ID, t.org.String(), resealed.WrappedKey, to, from, stale.Sealed.Ciphertext)
	return rows == 1, err
}

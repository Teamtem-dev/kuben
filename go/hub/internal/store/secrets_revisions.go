package store

// Writing secrets (repo/secrets.rs): reserving and recording revisions,
// revoking them, deleting secrets, and who references them.

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	createSecret = "INSERT INTO secrets " +
		"(id, org_id, project_id, environment_id, name, kind, registry, created_by, created_at, updated_at) " +
		"SELECT $1, e.org_id, e.project_id, e.id, $5, $8, $9, $6, $7, $7 FROM environments e " +
		"WHERE e.id = $4 AND e.org_id = $2 AND e.project_id = $3 AND NOT e.deleting " +
		"ON CONFLICT (environment_id, name) WHERE deleted_at IS NULL DO NOTHING"
	nextSecretRevision = "UPDATE secrets s SET current_revision = current_revision + 1, updated_at = $4 " +
		"FROM environments e " +
		"WHERE s.environment_id = $1 AND s.name = $2 AND s.org_id = $3 AND s.deleted_at IS NULL " +
		"AND s.kind = $5 AND s.registry IS NOT DISTINCT FROM $6 " +
		"AND e.id = s.environment_id AND e.org_id = s.org_id AND NOT e.deleting " +
		"RETURNING s.id, s.current_revision"
	insertSecretRevision = "INSERT INTO secret_revisions " +
		"(secret_id, org_id, revision, keys, ciphertext, wrapped_key, key_version, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	liveSecret = "SELECT id FROM secrets " +
		"WHERE environment_id = $1 AND name = $2 AND org_id = $3 AND deleted_at IS NULL"
	registryTaken = "SELECT name FROM secrets WHERE environment_id = $1 AND org_id = $2 " +
		"AND deleted_at IS NULL AND kind = 'registry' AND registry = $3 AND name <> $4"
	registryLogin = "SELECT s.id AS secret_id, s.current_revision AS revision, r.ciphertext, " +
		"r.wrapped_key, r.key_version FROM secrets s " +
		"JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision AND r.org_id = s.org_id " +
		"WHERE s.environment_id = $1 AND s.org_id = $2 AND s.deleted_at IS NULL AND s.kind = 'registry' " +
		"AND s.registry = $3 AND r.revoked_at IS NULL"
	secretRevisionsSQL = "SELECT revision, keys, key_version, created_by, created_at, revoked_at, revoked_by " +
		"FROM secret_revisions WHERE secret_id = $1 AND org_id = $2 ORDER BY revision DESC"
	revokeSecretRevision = "UPDATE secret_revisions SET revoked_at = $4, revoked_by = $5 " +
		"WHERE secret_id = $1 AND revision = $2 AND org_id = $3 AND revoked_at IS NULL"
	secretRevisionExists = "SELECT EXISTS (SELECT 1 FROM secret_revisions WHERE secret_id = $1 AND revision = $2 AND org_id = $3)" //nolint:gosec // G101: SQL, not a credential
	// secretUsers is the live targets of an environment whose newest
	// configuration references a secret by name.
	secretUsers = "SELECT t.id, a.slug FROM application_targets t " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id " +
		"JOIN LATERAL (SELECT c.config FROM target_config_revisions c " +
		"WHERE c.target_id = t.id AND c.org_id = t.org_id ORDER BY c.revision DESC LIMIT 1) c ON TRUE " +
		"WHERE p.environment_id = $1 AND t.org_id = $2 AND NOT t.deleting " +
		"AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(c.config -> 'env') = 'array' " +
		"THEN c.config -> 'env' ELSE '[]'::jsonb END) AS v (value) " +
		"WHERE v.value -> 'fromSecret' ->> 'name' = $3) " +
		"ORDER BY a.slug"
	deleteSecret = "UPDATE secrets SET deleted_at = $3, updated_at = $3 " +
		"WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL"
	// untrustedEnvironment is repo/previews.rs UNTRUSTED_ENVIRONMENT.
	untrustedEnvironment = "SELECT EXISTS (SELECT 1 FROM previews WHERE environment_id = $1 AND org_id = $2 AND NOT trusted)"
)

// Reserved is the identity a new revision is sealed for.
type Reserved struct {
	Secret   uuid.UUID
	Revision uint64
}

// Reservation is the outcome of [Tenant.ReserveSecretRevision].
//
//sumtype:decl
type Reservation interface{ reservation() }

type (
	// ReservationReserved is the next revision, to be inserted in the same
	// transaction.
	ReservationReserved struct{ Reserved Reserved }
	// ReservationNoEnvironment means the project has no such live
	// environment.
	ReservationNoEnvironment struct{}
	// ReservationTaken means the name belongs to a secret of another kind
	// or registry, or the registry has a credential of another name.
	ReservationTaken struct{ Why string }
)

func (ReservationReserved) reservation()      {}
func (ReservationNoEnvironment) reservation() {}
func (ReservationTaken) reservation()         {}

// SecretRevisionInfo is one revision of a secret, without its values.
type SecretRevisionInfo struct {
	Revision   uint64
	Keys       []string
	KeyVersion uint32
	CreatedBy  string
	CreatedAt  int64
	RevokedAt  opt.Val[int64]
	RevokedBy  opt.Val[string]
}

// Revoked is the outcome of [Tenant.RevokeSecretRevision].
type Revoked uint8

// The outcomes of a revocation.
const (
	// RevokedDone: revoked now.
	RevokedDone Revoked = iota + 1
	// RevokedAlready: revoked before; revocation is never undone.
	RevokedAlready
	// RevokedNotFound: no such secret or revision.
	RevokedNotFound
)

// SecretDeleted is the outcome of [Tenant.DeleteSecret].
//
//sumtype:decl
type SecretDeleted interface{ secretDeleted() }

type (
	// SecretDeletedDone means the secret is deleted.
	SecretDeletedDone struct{}
	// SecretDeletedNotFound means there is no such live secret.
	SecretDeletedNotFound struct{}
	// SecretDeletedInUse means these apps of the environment reference it.
	SecretDeletedInUse struct{ Apps []string }
)

func (SecretDeletedDone) secretDeleted()     {}
func (SecretDeletedNotFound) secretDeleted() {}
func (SecretDeletedInUse) secretDeleted()    {}

// SecretUser is a live target referencing a secret, with its app's name.
type SecretUser struct {
	Target ids.TargetID
	Slug   string
}

// version is a key version column (i32, never negative).
func keyVersion(op string, value int32) (uint32, error) {
	if value < 0 {
		return 0, decodeErr(op, "out of range integral type conversion attempted")
	}
	return uint32(value), nil
}

// keyVersionColumn is a key version as the i32 column.
func keyVersionColumn(op string, value uint32) (int32, error) {
	if value > math.MaxInt32 {
		return 0, DatabaseError{Op: op, Err: fmt.Errorf(
			"error occurred while encoding a value: out of range integral type conversion attempted")}
	}
	return int32(value), nil
}

// ReserveSecretRevision takes the next revision number of secret name of
// kind, creating the secret when it does not exist. The revision must be
// inserted in the same transaction.
func (t *Tenant) ReserveSecretRevision(
	ctx context.Context, project ids.ProjectID, env ids.EnvironmentID, name string, kind SecretKind, createdBy string,
) (Reservation, error) {
	const op = "reserve a secret revision"
	org := t.org.String()
	kindColumn, registry := secretKindColumns(kind)
	if registry != nil {
		other, taken, err := queryOpt(ctx, t.tx, op, registryTaken, pgx.RowTo[string], env, org, *registry, name)
		if err != nil {
			return nil, err
		}
		if taken {
			return ReservationTaken{Why: fmt.Sprintf("registry `%s` has the credential `%s` already", *registry, other)}, nil
		}
	}
	now := t.store.now()
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := exec(ctx, t.tx, op, createSecret, id, org, project, env, name, createdBy, now, kindColumn, registry); err != nil {
		return nil, err
	}
	type next struct {
		id       uuid.UUID
		revision int64
	}
	row, ok, err := queryOpt(ctx, t.tx, op, nextSecretRevision, func(row pgx.CollectableRow) (next, error) {
		var n next
		err := row.Scan(&n.id, &n.revision)
		return n, err
	}, env, name, org, now, kindColumn, registry)
	if err != nil {
		return nil, err
	}
	if ok {
		revision, err := counter(op, row.revision)
		if err != nil {
			return nil, err
		}
		return ReservationReserved{Reserved: Reserved{Secret: row.id, Revision: revision}}, nil
	}
	_, live, err := t.liveSecret(ctx, env, name)
	if err != nil {
		return nil, err
	}
	if live {
		return ReservationTaken{Why: fmt.Sprintf("`%s` is a secret of another kind or registry", name)}, nil
	}
	return ReservationNoEnvironment{}, nil
}

// InsertSecretRevision records the reserved revision with keys and its
// sealed values, and its audit record (names only).
func (t *Tenant) InsertSecretRevision(
	ctx context.Context, reserved Reserved, keys []string, sealed SealedBytes, createdBy string, audit NewAudit,
) error {
	const op = "record a secret revision"
	revision, err := signed(op, reserved.Revision)
	if err != nil {
		return err
	}
	version, err := keyVersionColumn(op, sealed.KeyVersion)
	if err != nil {
		return err
	}
	if keys == nil {
		keys = []string{}
	}
	_, err = exec(ctx, t.tx, op, insertSecretRevision, reserved.Secret, t.org.String(), revision, keys,
		sealed.Ciphertext, sealed.WrappedKey, version, createdBy, t.store.now())
	if err != nil {
		return err
	}
	audit.Data = opt.Some[any](map[string]any{"revision": reserved.Revision, "keys": keys})
	return t.AppendAudit(ctx, audit)
}

func (t *Tenant) liveSecret(ctx context.Context, env ids.EnvironmentID, name string) (uuid.UUID, bool, error) {
	return queryOpt(ctx, t.tx, "read a live secret", liveSecret, pgx.RowTo[uuid.UUID], env, name, t.org.String())
}

// SecretRevisions is every revision of the live secret name, newest first;
// false when there is no such secret.
func (t *Tenant) SecretRevisions(ctx context.Context, env ids.EnvironmentID, name string) ([]SecretRevisionInfo, bool, error) {
	const op = "read a secret's revisions"
	secret, ok, err := t.liveSecret(ctx, env, name)
	if err != nil || !ok {
		return nil, false, err
	}
	out, err := queryAll(ctx, t.tx, op, secretRevisionsSQL, func(row pgx.CollectableRow) (SecretRevisionInfo, error) {
		var r SecretRevisionInfo
		var revision int64
		var version int32
		var revokedAt *int64
		var revokedBy *string
		if err := row.Scan(&revision, &r.Keys, &version, &r.CreatedBy, &r.CreatedAt, &revokedAt, &revokedBy); err != nil {
			return SecretRevisionInfo{}, err
		}
		var err error
		if r.Revision, err = counter(op, revision); err != nil {
			return SecretRevisionInfo{}, err
		}
		if r.KeyVersion, err = keyVersion(op, version); err != nil {
			return SecretRevisionInfo{}, err
		}
		r.RevokedAt = opt.FromPtr(revokedAt)
		r.RevokedBy = opt.FromPtr(revokedBy)
		return r, nil
	}, secret, t.org.String())
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// RevokeSecretRevision revokes revision of the live secret name for good.
// Runs bound to it fail instead of delivering it; the values stay
// unreadable to the API.
func (t *Tenant) RevokeSecretRevision(
	ctx context.Context, env ids.EnvironmentID, name string, revision uint64, revokedBy string, audit NewAudit,
) (Revoked, error) {
	const op = "revoke a secret revision"
	secret, ok, err := t.liveSecret(ctx, env, name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return RevokedNotFound, nil
	}
	n, err := signed(op, revision)
	if err != nil {
		return 0, err
	}
	org := t.org.String()
	rows, err := exec(ctx, t.tx, op, revokeSecretRevision, secret, n, org, t.store.now(), revokedBy)
	if err != nil {
		return 0, err
	}
	if rows == 1 {
		if err := t.AppendAudit(ctx, audit); err != nil {
			return 0, err
		}
		return RevokedDone, nil
	}
	var exists bool
	if err := queryOne(ctx, t.tx, op, secretRevisionExists, []any{&exists}, secret, n, org); err != nil {
		return 0, err
	}
	if exists {
		return RevokedAlready, nil
	}
	return RevokedNotFound, nil
}

// SecretUsers is the live targets of environment whose newest configuration
// references secret name, with their app names.
func (t *Tenant) SecretUsers(ctx context.Context, env ids.EnvironmentID, name string) ([]SecretUser, error) {
	return queryAll(ctx, t.tx, "read a secret's users", secretUsers, func(row pgx.CollectableRow) (SecretUser, error) {
		var u SecretUser
		var id uuid.UUID
		if err := row.Scan(&id, &u.Slug); err != nil {
			return SecretUser{}, err
		}
		u.Target = ids.From[ids.Target](id)
		return u, nil
	}, env, t.org.String(), name)
}

// DeleteSecret deletes the live secret name unless an app references it.
// Its revisions stay, for the runs bound to them.
func (t *Tenant) DeleteSecret(ctx context.Context, env ids.EnvironmentID, name string, audit NewAudit) (SecretDeleted, error) {
	secret, ok, err := t.liveSecret(ctx, env, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return SecretDeletedNotFound{}, nil
	}
	users, err := t.SecretUsers(ctx, env, name)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		apps := make([]string, 0, len(users))
		for _, u := range users {
			apps = append(apps, u.Slug)
		}
		return SecretDeletedInUse{Apps: apps}, nil
	}
	if _, err := exec(ctx, t.tx, "delete a secret", deleteSecret, secret, t.org.String(), t.store.now()); err != nil {
		return nil, err
	}
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err
	}
	return SecretDeletedDone{}, nil
}

// UntrustedEnvironment reports whether environment is an untrusted preview
// (repo/previews.rs untrusted_environment): no secret ever reaches code
// from outside the repository (M5.1).
func (t *Tenant) UntrustedEnvironment(ctx context.Context, env ids.EnvironmentID) (bool, error) {
	var untrusted bool
	err := queryOne(ctx, t.tx, "check a preview's trust", untrustedEnvironment, []any{&untrusted}, env, t.org.String())
	return untrusted, err
}

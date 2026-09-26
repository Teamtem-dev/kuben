package store

// Sealed secret values (repo/secrets.rs): what the materializer delivers,
// what the API opens for registry logins, and what the keyring reseals
// and checks at startup.

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

const (
	runSecrets = "SELECT b.name, b.secret_id, b.revision, s.registry, r.keys, r.ciphertext, " +
		"r.wrapped_key, r.key_version, r.revoked_at IS NOT NULL AS revoked FROM run_secret_bindings b " +
		"JOIN secrets s ON s.id = b.secret_id AND s.org_id = b.org_id " +
		"JOIN secret_revisions r ON r.secret_id = b.secret_id AND r.revision = b.revision AND r.org_id = b.org_id " +
		"WHERE b.run_id = $1 AND b.org_id = $2 ORDER BY b.name"
	// secretsInUse is the revisions the cluster may still need in an
	// environment: those of runs that may still write, and of each
	// target's newest and newest successful run.
	secretsInUse = "SELECT DISTINCT b.secret_id, b.revision FROM run_secret_bindings b " +
		"JOIN deployment_runs r ON r.id = b.run_id AND r.org_id = b.org_id " +
		"JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"WHERE p.environment_id = $1 AND b.org_id = $2 AND ( " +
		"r.phase <> ALL($3) " +
		"OR r.id = (SELECT d.id FROM deployment_runs d WHERE d.target_id = r.target_id " +
		"AND d.org_id = r.org_id ORDER BY d.generation DESC LIMIT 1) " +
		"OR r.id = (SELECT d.id FROM deployment_runs d WHERE d.target_id = r.target_id " +
		"AND d.org_id = r.org_id AND d.phase = 'succeeded' ORDER BY d.generation DESC LIMIT 1))"
	sealedBelow = "SELECT secret_id, revision, ciphertext, wrapped_key, key_version " +
		"FROM secret_revisions WHERE org_id = $1 AND key_version < $2 " +
		"ORDER BY secret_id, revision LIMIT $3"
	reseal = "UPDATE secret_revisions SET wrapped_key = $4, key_version = $5 " +
		"WHERE secret_id = $1 AND revision = $2 AND org_id = $3 AND key_version = $6"
	recordFingerprint = "INSERT INTO secret_key_fingerprints (version, fingerprint, first_seen) " +
		"VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING"
	fingerprintsSQL = "SELECT version, fingerprint FROM secret_key_fingerprints ORDER BY version"
)

// BoundSecret is a bound revision with its sealed values, for the
// materializer.
type BoundSecret struct {
	Binding SecretBinding
	Keys    []string
	Sealed  SealedBytes
	Revoked bool
}

// SealedRevision is one revision's sealed values.
type SealedRevision struct {
	Secret   uuid.UUID
	Revision uint64
	Sealed   SealedBytes
}

// SecretRevisionKey is a secret and one of its revisions.
type SecretRevisionKey struct {
	Secret   uuid.UUID
	Revision uint64
}

func scanSealedRevision(op string) func(pgx.CollectableRow) (SealedRevision, error) {
	return func(row pgx.CollectableRow) (SealedRevision, error) {
		var s SealedRevision
		var revision int64
		var version int32
		if err := row.Scan(&s.Secret, &revision, &s.Sealed.Ciphertext, &s.Sealed.WrappedKey, &version); err != nil {
			return SealedRevision{}, err
		}
		var err error
		if s.Revision, err = counter(op, revision); err != nil {
			return SealedRevision{}, err
		}
		if s.Sealed.KeyVersion, err = keyVersion(op, version); err != nil {
			return SealedRevision{}, err
		}
		return s, nil
	}
}

// RegistryLogin is the current, unrevoked revision of the login for
// registry in environment, sealed.
func (t *Tenant) RegistryLogin(ctx context.Context, env ids.EnvironmentID, registry string) (SealedRevision, bool, error) {
	const op = "read a registry login"
	return queryOpt(ctx, t.tx, op, registryLogin, scanSealedRevision(op), env, t.org.String(), registry)
}

// RunSecrets is the revisions run is bound to, with their sealed values.
func (t *Tenant) RunSecrets(ctx context.Context, run ids.DeploymentRunID) ([]BoundSecret, error) {
	const op = "read a run's secrets"
	return queryAll(ctx, t.tx, op, runSecrets, func(row pgx.CollectableRow) (BoundSecret, error) {
		var b BoundSecret
		var revision int64
		var registry *string
		var version int32
		err := row.Scan(&b.Binding.Name, &b.Binding.Secret, &revision, &registry, &b.Keys,
			&b.Sealed.Ciphertext, &b.Sealed.WrappedKey, &version, &b.Revoked)
		if err != nil {
			return BoundSecret{}, err
		}
		if b.Binding.Revision, err = counter(op, revision); err != nil {
			return BoundSecret{}, err
		}
		if b.Sealed.KeyVersion, err = keyVersion(op, version); err != nil {
			return BoundSecret{}, err
		}
		b.Binding.Registry = opt.FromPtr(registry)
		return b, nil
	}, run, t.org.String())
}

// SecretRevisionsInUse is the (secret, revision) pairs the workloads of
// environment may still read.
func (t *Tenant) SecretRevisionsInUse(ctx context.Context, env ids.EnvironmentID) (map[SecretRevisionKey]struct{}, error) {
	const op = "read the secret revisions in use"
	var settled []string
	for _, p := range run.Phases() {
		if p.IsFinal() || p == run.Failed {
			settled = append(settled, p.String())
		}
	}
	rows, err := queryAll(ctx, t.tx, op, secretsInUse, func(row pgx.CollectableRow) (SecretRevisionKey, error) {
		var k SecretRevisionKey
		var revision int64
		if err := row.Scan(&k.Secret, &revision); err != nil {
			return SecretRevisionKey{}, err
		}
		var err error
		k.Revision, err = counter(op, revision)
		return k, err
	}, env, t.org.String(), settled)
	if err != nil {
		return nil, err
	}
	out := make(map[SecretRevisionKey]struct{}, len(rows))
	for _, k := range rows {
		out[k] = struct{}{}
	}
	return out, nil
}

// StaleSeals is up to limit revisions sealed under a key older than
// current.
func (t *Tenant) StaleSeals(ctx context.Context, current uint32, limit int64) ([]SealedRevision, error) {
	const op = "read stale secret seals"
	version, err := keyVersionColumn(op, current)
	if err != nil {
		return nil, err
	}
	return queryAll(ctx, t.tx, op, sealedBelow, scanSealedRevision(op), t.org.String(), version, limit)
}

// Reseal replaces the sealed data key of stale with resealed, unless it
// changed meanwhile (false). The values are not touched: a resealed value
// with another ciphertext is refused.
func (t *Tenant) Reseal(ctx context.Context, stale SealedRevision, resealed SealedBytes) (bool, error) {
	const op = "reseal a secret revision"
	if !bytes.Equal(resealed.Ciphertext, stale.Sealed.Ciphertext) {
		return false, DatabaseError{Op: op, Err: errors.New(
			"encountered unexpected or invalid data: a reseal must keep the ciphertext")}
	}
	revision, err := signed(op, stale.Revision)
	if err != nil {
		return false, err
	}
	to, err := keyVersionColumn(op, resealed.KeyVersion)
	if err != nil {
		return false, err
	}
	from, err := keyVersionColumn(op, stale.Sealed.KeyVersion)
	if err != nil {
		return false, err
	}
	rows, err := exec(ctx, t.tx, op, reseal, stale.Secret, revision, t.org.String(), resealed.WrappedKey, to, from)
	return rows == 1, err
}

// KeyFingerprint is a key-encryption key's version and fingerprint.
type KeyFingerprint struct {
	Version uint32
	SHA256  [32]byte
}

// KeyringCheck is how a keyring compares with the keys this database has
// seen.
type KeyringCheck struct {
	// Mismatched is the versions whose key differs from the one recorded:
	// the keyring is not this installation's.
	Mismatched []uint32
	// Missing is the recorded versions the keyring lacks: revisions sealed
	// under them cannot be opened.
	Missing []uint32
}

// CheckKeyring records the fingerprints of a keyring's keys and compares
// them with those recorded before.
func (s *Store) CheckKeyring(ctx context.Context, fingerprints []KeyFingerprint) (check KeyringCheck, err error) {
	const op = "check the secret keyring"
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return KeyringCheck{}, dbErr(op, err)
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, dbErr("roll back", rbErr))
			}
		}
	}()
	for _, f := range fingerprints {
		version, err := keyVersionColumn(op, f.Version)
		if err != nil {
			return KeyringCheck{}, err
		}
		if _, err := exec(ctx, tx, op, recordFingerprint, version, f.SHA256[:], s.now()); err != nil {
			return KeyringCheck{}, err
		}
	}
	type known struct {
		version     int32
		fingerprint []byte
	}
	rows, err := queryAll(ctx, tx, op, fingerprintsSQL, func(row pgx.CollectableRow) (known, error) {
		var k known
		err := row.Scan(&k.version, &k.fingerprint)
		return k, err
	})
	if err != nil {
		return KeyringCheck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return KeyringCheck{}, dbErr(op, err)
	}
	for _, k := range rows {
		version, err := keyVersion(op, k.version)
		if err != nil {
			return KeyringCheck{}, err
		}
		i := indexOfVersion(fingerprints, version)
		switch {
		case i < 0:
			check.Missing = append(check.Missing, version)
		case !bytes.Equal(fingerprints[i].SHA256[:], k.fingerprint):
			check.Mismatched = append(check.Mismatched, version)
		}
	}
	return check, nil
}

func indexOfVersion(fingerprints []KeyFingerprint, version uint32) int {
	for i, f := range fingerprints {
		if f.Version == version {
			return i
		}
	}
	return -1
}

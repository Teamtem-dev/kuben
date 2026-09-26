package store

// Managed secrets (M4.4, ADR-030, migration 0022); the port of
// repo/secrets.rs.
//
// The store never sees a value: callers seal a revision's values between
// [Tenant.ReserveSecretRevision], which fixes the identity the seal is bound
// to, and [Tenant.InsertSecretRevision], in one transaction. Deployment
// runs bind the current revision of every secret their configuration
// references when they are accepted; names without a managed secret are
// left to the cluster (Secrets made before M4.4). A `registry` secret
// (migration 0023) holds the login of one registry and is bound to every
// run whose release comes from that registry.
//
// This file holds the types and the reads of secrets; secrets_revisions.go
// the writes, secrets_seals.go what the materializer and the keyring need.

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const (
	wantedSecrets = "WITH place AS ( " +
		"SELECT p.environment_id FROM application_targets t " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"WHERE t.id = $3 AND t.org_id = $2), " +
		"wanted AS ( " +
		"SELECT DISTINCT v.value -> 'fromSecret' ->> 'name' AS name FROM target_config_revisions c " +
		"CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(c.config -> 'env') = 'array' " +
		"THEN c.config -> 'env' ELSE '[]'::jsonb END) AS v (value) " +
		"WHERE c.id = $1 AND c.org_id = $2 AND jsonb_typeof(v.value -> 'fromSecret' -> 'name') = 'string') " +
		"SELECT w.name, s.id AS secret_id, s.current_revision, r.revoked_at IS NOT NULL AS revoked " +
		"FROM wanted w CROSS JOIN place " +
		"LEFT JOIN secrets s ON s.environment_id = place.environment_id AND s.name = w.name " +
		"AND s.org_id = $2 AND s.deleted_at IS NULL AND s.kind = 'opaque' " +
		"LEFT JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision " +
		"UNION ALL " +
		"SELECT s.name, s.id, s.current_revision, r.revoked_at IS NOT NULL FROM place " +
		"JOIN releases rel ON rel.id = $4 AND rel.org_id = $2 " +
		"JOIN secrets s ON s.environment_id = place.environment_id AND s.org_id = $2 " +
		"AND s.deleted_at IS NULL AND s.kind = 'registry' " +
		"AND s.registry = split_part(rel.source ->> 'image_repository', '/', 1) " +
		"JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision " +
		"ORDER BY 1"
	bindSecrets = "INSERT INTO run_secret_bindings (run_id, org_id, secret_id, revision, name) " +
		"SELECT $1, $2, x.secret_id, x.revision, x.name " +
		"FROM unnest($3::uuid[], $4::bigint[], $5::text[]) AS x (secret_id, revision, name)"
	runBindings = "SELECT b.name, b.secret_id, b.revision, s.registry FROM run_secret_bindings b " +
		"JOIN secrets s ON s.id = b.secret_id AND s.org_id = b.org_id " +
		"WHERE b.run_id = $1 AND b.org_id = $2 ORDER BY b.name"
	// summarySQL is Rust's `summary!` prefix; secretsSQL and secretSQL
	// complete it.
	summarySQL = "SELECT s.id, s.name, s.kind, s.registry, s.current_revision, r.keys, " +
		"r.revoked_at IS NOT NULL AS revoked, " +
		"s.created_at, s.updated_at FROM secrets s " +
		"JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision " +
		"AND r.org_id = s.org_id " +
		"WHERE s.environment_id = $1 AND s.org_id = $2 AND s.deleted_at IS NULL "
	secretsSQL = summarySQL + "ORDER BY s.name"
	secretSQL  = summarySQL + "AND s.name = $3"
)

// SealedBytes is a revision's values as sealed by the caller (the store
// never sees a value): the values under the revision's data key, the data
// key under the key-encryption key KeyVersion.
type SealedBytes struct {
	Ciphertext []byte
	WrappedKey []byte
	KeyVersion uint32
}

// String shows the key version only, as the Rust Debug did.
func (s SealedBytes) String() string {
	return "SealedBytes { key_version: " + strconv.FormatUint(uint64(s.KeyVersion), 10) + ", .. }"
}

// GoString is String for %#v.
func (s SealedBytes) GoString() string { return s.String() }

// SecretKind is what a secret holds.
//
//sumtype:decl
type SecretKind interface{ secretKind() }

type (
	// SecretOpaque is values an app reads by key.
	SecretOpaque struct{}
	// SecretRegistry is the username and password of the registry Host
	// (`ghcr.io`, …).
	SecretRegistry struct{ Host string }
)

func (SecretOpaque) secretKind()   {}
func (SecretRegistry) secretKind() {}

// secretKindColumns is the stored kind and registry of kind.
func secretKindColumns(kind SecretKind) (string, *string) {
	switch k := kind.(type) {
	case SecretOpaque:
		return "opaque", nil
	case SecretRegistry:
		return "registry", &k.Host
	}
	return "opaque", nil
}

// secretKindOf reads the stored kind and registry.
func secretKindOf(op, kind string, registry *string) (SecretKind, error) {
	switch {
	case kind == "opaque" && registry == nil:
		return SecretOpaque{}, nil
	case kind == "registry" && registry != nil:
		return SecretRegistry{Host: *registry}, nil
	}
	return nil, decodeErr(op, "unknown secret kind %s", rustQuote(kind))
}

// SecretSummary is a live secret and its current revision.
type SecretSummary struct {
	ID              uuid.UUID
	Name            string
	Kind            SecretKind
	CurrentRevision uint64
	Keys            []string
	// Revoked means the current revision is revoked: nothing can be
	// deployed with it.
	Revoked   bool
	CreatedAt int64
	UpdatedAt int64
}

func scanSecretSummary(op string) func(pgx.CollectableRow) (SecretSummary, error) {
	return func(row pgx.CollectableRow) (SecretSummary, error) {
		var s SecretSummary
		var kind string
		var registry *string
		var current int64
		err := row.Scan(&s.ID, &s.Name, &kind, &registry, &current, &s.Keys, &s.Revoked, &s.CreatedAt, &s.UpdatedAt)
		if err != nil {
			return SecretSummary{}, err
		}
		if s.Kind, err = secretKindOf(op, kind, registry); err != nil {
			return SecretSummary{}, err
		}
		if s.CurrentRevision, err = counter(op, current); err != nil {
			return SecretSummary{}, err
		}
		return s, nil
	}
}

// Secrets returns the live secrets of environment, of every kind, by name.
func (t *Tenant) Secrets(ctx context.Context, env ids.EnvironmentID) ([]SecretSummary, error) {
	const op = "read an environment's secrets"
	return queryAll(ctx, t.tx, op, secretsSQL, scanSecretSummary(op), env, t.org.String())
}

// Secret is the live secret name of environment.
func (t *Tenant) Secret(ctx context.Context, env ids.EnvironmentID, name string) (SecretSummary, bool, error) {
	const op = "read a secret"
	return queryOpt(ctx, t.tx, op, secretSQL, scanSecretSummary(op), env, t.org.String(), name)
}

// SecretBinding is the revision of a secret a run renders.
type SecretBinding struct {
	// Name is the name the configuration referenced.
	Name     string
	Secret   uuid.UUID
	Revision uint64
	// Registry is the registry of a pull credential; absent for values an
	// app reads.
	Registry opt.Val[string]
}

// secretRevision is a secret and one of its revisions.
type secretRevision struct {
	secret   uuid.UUID
	revision uint64
}

// wanted is a secret a run would bind: by name, its current revision if it
// exists, and whether that revision is revoked.
type wanted struct {
	name    string
	current opt.Val[secretRevision]
	revoked bool
}

// wantedSecrets is what a run of revision and release on tgt would bind.
func (t *Tenant) wantedSecrets(
	ctx context.Context, revision ids.ConfigRevisionID, tgt ids.TargetID, release ids.ReleaseID,
) ([]wanted, error) {
	const op = "read the secrets a run binds"
	type wantedRow struct {
		name    string
		secret  *uuid.UUID
		current *int64
		revoked bool
	}
	rows, err := queryAll(ctx, t.tx, op, wantedSecrets, func(row pgx.CollectableRow) (wantedRow, error) {
		var r wantedRow
		err := row.Scan(&r.name, &r.secret, &r.current, &r.revoked)
		return r, err
	}, revision, t.org.String(), tgt, release)
	if err != nil {
		return nil, err
	}
	out := make([]wanted, 0, len(rows))
	for _, r := range rows {
		w := wanted{name: r.name, revoked: r.revoked}
		if r.secret != nil && r.current != nil {
			n, err := counter(op, *r.current)
			if err != nil {
				return nil, err
			}
			w.current = opt.Some(secretRevision{secret: *r.secret, revision: n})
		}
		out = append(out, w)
	}
	return out, nil
}

// bindRunSecrets binds run to the current revisions in secrets.
func (t *Tenant) bindRunSecrets(ctx context.Context, run ids.DeploymentRunID, secrets []wanted) error {
	const op = "bind a run's secrets"
	var secretIDs []uuid.UUID
	var revisions []int64
	var names []string
	for _, w := range secrets {
		current, ok := w.current.Get()
		if !ok {
			continue
		}
		n, err := signed(op, current.revision)
		if err != nil {
			return err
		}
		secretIDs = append(secretIDs, current.secret)
		revisions = append(revisions, n)
		names = append(names, w.name)
	}
	if len(secretIDs) == 0 {
		return nil
	}
	_, err := exec(ctx, t.tx, op, bindSecrets, run, t.org.String(), secretIDs, revisions, names)
	return err
}

// RunSecretBindings is the revisions run is bound to, by name.
func (t *Tenant) RunSecretBindings(ctx context.Context, run ids.DeploymentRunID) ([]SecretBinding, error) {
	const op = "read a run's secret bindings"
	return queryAll(ctx, t.tx, op, runBindings, func(row pgx.CollectableRow) (SecretBinding, error) {
		var b SecretBinding
		var revision int64
		var registry *string
		if err := row.Scan(&b.Name, &b.Secret, &revision, &registry); err != nil {
			return SecretBinding{}, err
		}
		n, err := counter(op, revision)
		if err != nil {
			return SecretBinding{}, err
		}
		b.Revision = n
		b.Registry = opt.FromPtr(registry)
		return b, nil
	}, run, t.org.String())
}

package store

// Secrets (M4.4); a partial port of repo/secrets.rs: what a deployment run
// binds (the current revisions of the secrets its configuration and its
// release's registry reference) and what the materializer reads back.
// Writing, sealing, rotating and deleting secrets follow with the secret
// routes.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
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
	secretsSQL = "SELECT s.id, s.name, s.kind, s.registry, s.current_revision, r.keys, " +
		"r.revoked_at IS NOT NULL AS revoked, " +
		"s.created_at, s.updated_at FROM secrets s " +
		"JOIN secret_revisions r ON r.secret_id = s.id AND r.revision = s.current_revision " +
		"AND r.org_id = s.org_id " +
		"WHERE s.environment_id = $1 AND s.org_id = $2 AND s.deleted_at IS NULL " +
		"ORDER BY s.name"
)

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

// SecretSummary is a live secret of an environment.
type SecretSummary struct {
	ID              uuid.UUID
	Name            string
	Kind            string
	Registry        opt.Val[string]
	CurrentRevision uint64
	Keys            []string
	Revoked         bool
	CreatedAt       int64
	UpdatedAt       int64
}

// Secrets returns the live secrets of environment, of every kind, by name.
func (t *Tenant) Secrets(ctx context.Context, env ids.EnvironmentID) ([]SecretSummary, error) {
	const op = "read an environment's secrets"
	type summaryRow struct {
		id              uuid.UUID
		name            string
		kind            string
		registry        *string
		currentRevision int64
		keys            []string
		revoked         bool
		createdAt       int64
		updatedAt       int64
	}
	rows, err := queryAll(ctx, t.tx, op, secretsSQL, func(row pgx.CollectableRow) (summaryRow, error) {
		var r summaryRow
		err := row.Scan(&r.id, &r.name, &r.kind, &r.registry, &r.currentRevision, &r.keys, &r.revoked, &r.createdAt, &r.updatedAt)
		return r, err
	}, env, t.org.String())
	if err != nil {
		return nil, err
	}
	out := make([]SecretSummary, 0, len(rows))
	for _, r := range rows {
		rev, err := counter(op, r.currentRevision)
		if err != nil {
			return nil, err
		}
		out = append(out, SecretSummary{
			ID:              r.id,
			Name:            r.name,
			Kind:            r.kind,
			Registry:        opt.FromPtr(r.registry),
			CurrentRevision: rev,
			Keys:            r.keys,
			Revoked:         r.revoked,
			CreatedAt:       r.createdAt,
			UpdatedAt:       r.updatedAt,
		})
	}
	return out, nil
}

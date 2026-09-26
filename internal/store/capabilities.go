package store

// Cluster capabilities (M2.1, ADR-031): the facts Kuben last discovered
// about a cluster, kept per organization like the cluster row they belong
// to; the port of repo/capabilities.rs. The facts are opaque JSON here; the
// platform's discovery defines them.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
)

const (
	recordCapabilities = "INSERT INTO cluster_capabilities (cluster_id, org_id, facts, observed_at) " +
		"SELECT c.id, c.org_id, $3::jsonb, $4 FROM clusters c WHERE c.org_id = $1 AND c.name = $2 " +
		"ON CONFLICT (cluster_id) DO UPDATE SET facts = EXCLUDED.facts, observed_at = EXCLUDED.observed_at " +
		"WHERE cluster_capabilities.observed_at <= EXCLUDED.observed_at"
	readCapabilities = "SELECT k.facts::text AS facts, k.observed_at FROM cluster_capabilities k " +
		"JOIN clusters c ON c.id = k.cluster_id AND c.org_id = k.org_id " +
		"WHERE c.org_id = $1 AND c.name = $2"
	selectOrgIDs    = "SELECT id FROM organizations ORDER BY id"
	installationOrg = "SELECT id FROM organizations ORDER BY (slug = $1) DESC, created_at, id LIMIT 1"
)

// CapabilityRecord is the capabilities recorded for a cluster.
type CapabilityRecord struct {
	Facts any
	// ObservedAt is when they were observed (unix ms).
	ObservedAt int64
}

// RecordClusterCapabilities records facts observed at observedAt for this
// organization's cluster named cluster. False when the organization has no
// such cluster or a newer observation is recorded already.
func (t *Tenant) RecordClusterCapabilities(ctx context.Context, cluster string, facts any, observedAt int64) (bool, error) {
	const op = "record cluster capabilities"
	text, err := canonical(op, facts)
	if err != nil {
		return false, err
	}
	n, err := exec(ctx, t.tx, op, recordCapabilities, t.org.String(), cluster, text, observedAt)
	return n == 1, err
}

// ClusterCapabilities is the capabilities recorded for this organization's
// cluster named cluster.
func (t *Tenant) ClusterCapabilities(ctx context.Context, cluster string) (CapabilityRecord, bool, error) {
	const op = "read cluster capabilities"
	return queryOpt(ctx, t.tx, op, readCapabilities, func(row pgx.CollectableRow) (CapabilityRecord, error) {
		var facts string
		var r CapabilityRecord
		if err := row.Scan(&facts, &r.ObservedAt); err != nil {
			return CapabilityRecord{}, err
		}
		v, err := jsonValue(op, facts)
		if err != nil {
			return CapabilityRecord{}, err
		}
		r.Facts = v
		return r, nil
	}, t.org.String(), cluster)
}

// OrgIDs is every organization, for workers that act on each in turn.
func (s *Store) OrgIDs(ctx context.Context) ([]ids.OrgID, error) {
	const op = "list organizations"
	texts, err := queryAll(ctx, s.db, op, selectOrgIDs, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]ids.OrgID, 0, len(texts))
	for _, text := range texts {
		id, err := orgID(op, text)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// InstallationOrg is the organization the installation belongs to: the one
// named slug if it exists, else the first one made. False before any
// exists.
func (s *Store) InstallationOrg(ctx context.Context, slug string) (ids.OrgID, bool, error) {
	const op = "find the installation's organization"
	text, ok, err := queryOpt(ctx, s.db, op, installationOrg, pgx.RowTo[string], slug)
	if err != nil || !ok {
		return ids.OrgID{}, false, err
	}
	id, err := orgID(op, text)
	if err != nil {
		return ids.OrgID{}, false, err
	}
	return id, true, nil
}

// RecordCapabilitiesEverywhere records facts for the cluster named cluster
// in every organization that has one (the installation's own cluster is
// `primary` in each). It returns how many rows changed.
func (s *Store) RecordCapabilitiesEverywhere(ctx context.Context, cluster string, facts any, observedAt int64) (int, error) {
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	recorded := 0
	for _, org := range orgs {
		changed, err := s.inTenant(ctx, org, func(t *Tenant) (bool, error) {
			return t.RecordClusterCapabilities(ctx, cluster, facts, observedAt)
		})
		if err != nil {
			return 0, err
		}
		if changed {
			recorded++
		}
	}
	return recorded, nil
}

// inTenant runs fn in a tenant transaction of org and commits it; an error
// rolls it back.
func (s *Store) inTenant(ctx context.Context, org ids.OrgID, fn func(*Tenant) (bool, error)) (result bool, err error) {
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return false, err
	}
	defer func() {
		if rbErr := t.Rollback(ctx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
	}()
	result, err = fn(t)
	if err != nil {
		return false, err
	}
	return result, t.Commit(ctx)
}

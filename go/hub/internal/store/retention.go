package store

// Retention budgets (M4.12; plan §17 data lifecycle): rows that only
// describe the past are removed once they are older than their budget, so
// the hot tables stay bounded; the port of repo/retention.rs. Audit events,
// runs, releases and evidence are never removed here.

import (
	"context"
	"errors"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

const dayMs = 24 * 3_600_000

// retentionBatch is the rows removed per statement, so a first run on a
// large table never holds long locks; the janitor comes back for the rest.
const retentionBatch = 5_000

const (
	retainSessions = "DELETE FROM sessions WHERE expires_at < $1"
	retainOutbox   = "DELETE FROM outbox WHERE id IN (SELECT id FROM outbox " +
		"WHERE delivered_at IS NOT NULL AND delivered_at < $1 LIMIT $2)"
	retainDeliveries = "DELETE FROM webhook_deliveries WHERE id IN (SELECT id FROM webhook_deliveries " +
		"WHERE status <> 'pending' AND finished_at < $1 LIMIT $2)"
	retainUsage     = "DELETE FROM usage_rollups WHERE org_id = $2 AND hour < $1"
	retainIncidents = "DELETE FROM incidents WHERE id IN (SELECT id FROM incidents " +
		"WHERE org_id = $3 AND resolved_at IS NOT NULL AND resolved_at < $1 LIMIT $2)"
)

// Retained is what one retention pass removed.
type Retained struct {
	Sessions          uint64 `json:"sessions"`
	Outbox            uint64 `json:"outbox"`
	WebhookDeliveries uint64 `json:"webhook_deliveries"`
	Incidents         uint64 `json:"incidents"`
	UsageHours        uint64 `json:"usage_hours"`
	// More: a batch was full; more rows are due.
	More bool `json:"more"`
}

// Total is every row removed.
func (r Retained) Total() uint64 {
	return r.Sessions + r.Outbox + r.WebhookDeliveries + r.Incidents + r.UsageHours
}

// retentionBefore is now minus a budget of days, at least one.
func retentionBefore(now int64, days uint32) int64 {
	return now - int64(max(days, 1))*dayMs
}

// ApplyRetention removes what is older than the budgets of cfg at now:
// expired sessions, delivered outbox messages, finished webhook deliveries,
// resolved incidents and old usage hours. At most one batch of each per
// call.
func (s *Store) ApplyRetention(ctx context.Context, cfg config.RetentionCfg, now int64) (Retained, error) {
	const op = "apply retention"
	var done Retained
	var err error
	if done.Sessions, err = exec(ctx, s.db, op, retainSessions, now); err != nil {
		return Retained{}, err
	}
	if done.Outbox, err = exec(ctx, s.db, op, retainOutbox, retentionBefore(now, cfg.OutboxDays), retentionBatch); err != nil {
		return Retained{}, err
	}
	if done.WebhookDeliveries, err = exec(ctx, s.db, op, retainDeliveries,
		retentionBefore(now, cfg.WebhookDeliveryDays), retentionBatch); err != nil {
		return Retained{}, err
	}
	done.More = done.Outbox >= retentionBatch || done.WebhookDeliveries >= retentionBatch
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return Retained{}, err
	}
	for _, org := range orgs {
		incidents, usage, err := s.retainTenant(ctx, org, cfg, now)
		if err != nil {
			return Retained{}, err
		}
		done.UsageHours += usage
		done.Incidents += incidents
		done.More = done.More || incidents >= retentionBatch
	}
	return done, nil
}

// retainTenant removes org's old resolved incidents and usage hours in one
// tenant transaction.
func (s *Store) retainTenant(ctx context.Context, org ids.OrgID, cfg config.RetentionCfg, now int64) (incidents, usage uint64, err error) {
	const op = "apply retention"
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if rbErr := t.Rollback(ctx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
	}()
	if incidents, err = exec(ctx, t.tx, op, retainIncidents,
		retentionBefore(now, cfg.ResolvedIncidentDays), retentionBatch, org.String()); err != nil {
		return 0, 0, err
	}
	if usage, err = exec(ctx, t.tx, op, retainUsage, retentionBefore(now, cfg.UsageDays), org.String()); err != nil {
		return 0, 0, err
	}
	return incidents, usage, t.Commit(ctx)
}

// ApplyRetentionNow is [Store.ApplyRetention] now.
func (s *Store) ApplyRetentionNow(ctx context.Context, cfg config.RetentionCfg) (Retained, error) {
	return s.ApplyRetention(ctx, cfg, s.now())
}

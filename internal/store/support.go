package store

// What a support bundle says about the database (M4.11): counts and codes
// only — no names, payloads, secrets or personal data; the port of
// repo/support.rs.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
)

// SupportWindowMs is how far back the bundle looks at operations.
const SupportWindowMs int64 = 7 * 24 * 3_600_000

const (
	supportOperations = "SELECT kind, phase, coalesce(error_code, '') AS code, count(*) AS n " +
		"FROM operations WHERE requested_at >= $1 OR NOT done " +
		"GROUP BY kind, phase, error_code ORDER BY kind, phase, error_code LIMIT 500"
	supportStuck = "SELECT count(*) FROM operations " +
		"WHERE NOT done AND last_progress_at IS NOT NULL AND last_progress_at < $1"
	supportOutbox = "SELECT count(*) FILTER (WHERE delivered_at IS NULL), " +
		"coalesce(min(created_at) FILTER (WHERE delivered_at IS NULL), 0) FROM outbox"
	supportDeliveries = "SELECT status, count(*) FROM webhook_deliveries " +
		"WHERE created_at >= $1 GROUP BY status ORDER BY status"
	supportTotals = "SELECT (SELECT count(*) FROM organizations), (SELECT count(*) FROM users), " +
		"(SELECT count(*) FROM deployment_runs), (SELECT count(*) FROM releases)"
	supportTenantCounts = "SELECT (SELECT count(*) FROM projects WHERE org_id = $1), " +
		"(SELECT count(*) FROM environments WHERE org_id = $1), " +
		"(SELECT count(*) FROM application_targets WHERE org_id = $1), " +
		"(SELECT count(*) FROM application_targets WHERE org_id = $1 AND paused_at IS NOT NULL), " +
		"(SELECT count(*) FROM webhook_endpoints WHERE org_id = $1 AND disabled_at IS NOT NULL)"
	supportTenantIncidents = "SELECT kind, severity, count(*) FROM incidents " +
		"WHERE org_id = $1 AND resolved_at IS NULL GROUP BY kind, severity"
)

// stuckAfterMs is how long an unfinished operation may go without progress
// before the bundle counts it as stuck.
const stuckAfterMs = 3_600_000

// OperationCount is the operations of one kind, phase and error code.
type OperationCount struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
	Code  string `json:"code"`
	N     int64  `json:"n"`
}

// StatusCount is the webhook deliveries of one status. Its JSON is Rust's
// tuple: `[status, n]`.
type StatusCount struct {
	Status string
	N      int64
}

// MarshalJSON writes the pair as an array.
func (c StatusCount) MarshalJSON() ([]byte, error) { return json.Marshal([]any{c.Status, c.N}) }

// IncidentCount is the open incidents of one kind and severity. Its JSON is
// Rust's tuple: `[kind, severity, n]`.
type IncidentCount struct {
	Kind     string
	Severity string
	N        int64
}

// MarshalJSON writes the triple as an array.
func (c IncidentCount) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{c.Kind, c.Severity, c.N})
}

// SupportSummary is the database part of a support bundle.
type SupportSummary struct {
	Organizations  int64 `json:"organizations"`
	Users          int64 `json:"users"`
	DeploymentRuns int64 `json:"deployment_runs"`
	Releases       int64 `json:"releases"`
	Projects       int64 `json:"projects"`
	Environments   int64 `json:"environments"`
	Apps           int64 `json:"apps"`
	PausedApps     int64 `json:"paused_apps"`
	// Operations requested in the window, and every unfinished one.
	Operations []OperationCount `json:"operations"`
	// StuckOperations are unfinished operations without progress for an
	// hour.
	StuckOperations int64 `json:"stuck_operations"`
	OutboxPending   int64 `json:"outbox_pending"`
	// OutboxOldestAt is when the oldest undelivered outbox message was
	// written (0: none).
	OutboxOldestAt int64 `json:"outbox_oldest_at"`
	// WebhookDeliveries of the window by status.
	WebhookDeliveries []StatusCount `json:"webhook_deliveries"`
	// OpenIncidents by kind and severity.
	OpenIncidents    []IncidentCount `json:"open_incidents"`
	DisabledWebhooks int64           `json:"disabled_webhooks"`
}

// SupportSummary is the counts for a support bundle.
func (s *Store) SupportSummary(ctx context.Context) (SupportSummary, error) {
	const op = "summarize for support"
	now := s.now()
	var sum SupportSummary
	if err := queryOne(ctx, s.db, op, supportTotals,
		[]any{&sum.Organizations, &sum.Users, &sum.DeploymentRuns, &sum.Releases}); err != nil {
		return SupportSummary{}, err
	}
	if err := queryOne(ctx, s.db, op, supportOutbox, []any{&sum.OutboxPending, &sum.OutboxOldestAt}); err != nil {
		return SupportSummary{}, err
	}
	var err error
	sum.Operations, err = queryAll(ctx, s.db, op, supportOperations, func(row pgx.CollectableRow) (OperationCount, error) {
		var c OperationCount
		err := row.Scan(&c.Kind, &c.Phase, &c.Code, &c.N)
		return c, err
	}, now-SupportWindowMs)
	if err != nil {
		return SupportSummary{}, err
	}
	if err := queryOne(ctx, s.db, op, supportStuck, []any{&sum.StuckOperations}, now-stuckAfterMs); err != nil {
		return SupportSummary{}, err
	}
	sum.WebhookDeliveries, err = queryAll(ctx, s.db, op, supportDeliveries, func(row pgx.CollectableRow) (StatusCount, error) {
		var c StatusCount
		err := row.Scan(&c.Status, &c.N)
		return c, err
	}, now-SupportWindowMs)
	if err != nil {
		return SupportSummary{}, err
	}
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return SupportSummary{}, err
	}
	incidents := map[[2]string]int64{}
	for _, org := range orgs {
		if err := s.supportTenant(ctx, org, &sum, incidents); err != nil {
			return SupportSummary{}, err
		}
	}
	sum.OpenIncidents = make([]IncidentCount, 0, len(incidents))
	for key, n := range incidents {
		sum.OpenIncidents = append(sum.OpenIncidents, IncidentCount{Kind: key[0], Severity: key[1], N: n})
	}
	// Rust's BTreeMap order: by kind, then severity.
	slices.SortFunc(sum.OpenIncidents, func(a, b IncidentCount) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Severity, b.Severity))
	})
	return sum, nil
}

// supportTenant adds org's counts to sum and its open incidents to
// incidents, in one tenant transaction (which Rust never committed: it only
// reads).
func (s *Store) supportTenant(ctx context.Context, org ids.OrgID, sum *SupportSummary, incidents map[[2]string]int64) (err error) {
	const op = "summarize an organization for support"
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := t.Rollback(ctx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
	}()
	var projects, environments, apps, paused, disabled int64
	if err := queryOne(ctx, t.tx, op, supportTenantCounts,
		[]any{&projects, &environments, &apps, &paused, &disabled}, org.String()); err != nil {
		return err
	}
	sum.Projects += projects
	sum.Environments += environments
	sum.Apps += apps
	sum.PausedApps += paused
	sum.DisabledWebhooks += disabled
	open, err := queryAll(ctx, t.tx, op, supportTenantIncidents, func(row pgx.CollectableRow) (IncidentCount, error) {
		var c IncidentCount
		err := row.Scan(&c.Kind, &c.Severity, &c.N)
		return c, err
	}, org.String())
	if err != nil {
		return err
	}
	for _, c := range open {
		incidents[[2]string{c.Kind, c.Severity}] += c.N
	}
	return nil
}

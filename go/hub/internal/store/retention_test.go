package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from retention.rs. The Rust test made its incidents and webhook
// deliveries through repo/notify.rs, which is not ported yet: until it is,
// the rows are written here with the same columns that repository writes.

const dayMs = 24 * 3_600_000

func TestBudgetsAreAtLeastADay(t *testing.T) {
	if got := store.RetentionBefore(10*dayMs, 0); got != 9*dayMs {
		t.Fatalf("0 days: %d", got)
	}
	if got := store.RetentionBefore(10*dayMs, 3); got != 7*dayMs {
		t.Fatalf("3 days: %d", got)
	}
}

// openIncident writes an open incident of kind `backup.stale`.
func openIncident(t *testing.T, tn *store.Tenant, key string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	now := nowMs()
	if _, err := tn.TestExec(t.Context(), "INSERT INTO incidents (id, org_id, kind, severity, dedupe_key, title, "+
		"opened_at, last_seen_at) VALUES ($1, $2, 'backup.stale', 'critical', $3, 't', $4, $4)",
		id, tn.Org().String(), key, now); err != nil {
		t.Fatalf("open an incident: %v", err)
	}
	return id
}

func TestOldRowsGoAndRecentOnesStay(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	endpoint := uuid.Must(uuid.NewV7())
	tn := tenant(t, s, o)
	old := openIncident(t, tn, "old")
	open := openIncident(t, tn, "open")
	now := nowMs()
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE incidents SET resolved_at = $2, resolved_by = 'u' WHERE id = $1", []any{old, now}},
		{
			"INSERT INTO webhook_endpoints (id, org_id, name, url, events, secret, wrapped_key, key_version, " +
				"created_by, created_at) VALUES ($1, $2, 'e', 'https://a.example', ARRAY['*'], '\\x01', '\\x02', 1, 'u', $3)",
			[]any{endpoint, o.String(), now},
		},
		// One delivered, one pending.
		{
			"INSERT INTO webhook_deliveries (id, org_id, endpoint_id, event_id, event, payload, status, attempts, " +
				"next_attempt_at, last_status, created_at, finished_at) " +
				"VALUES ($1, $2, $3, $4, 'ping', '{}'::jsonb, 'delivered', 1, $5, 200, $5, $5)",
			[]any{uuid.Must(uuid.NewV7()), o.String(), endpoint, uuid.Must(uuid.NewV7()), now},
		},
		{
			"INSERT INTO webhook_deliveries (id, org_id, endpoint_id, event_id, event, payload, next_attempt_at, created_at) " +
				"VALUES ($1, $2, $3, $4, 'ping', '{}'::jsonb, $5, $5)",
			[]any{uuid.Must(uuid.NewV7()), o.String(), endpoint, uuid.Must(uuid.NewV7()), now},
		},
	} {
		if _, err := tn.TestExec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	commit(t, tn)

	cfg := config.DefaultRetentionCfg()
	soon := must[store.Retained](t, "retain")(s.ApplyRetention(ctx, cfg, now))
	if soon.Incidents != 0 || soon.WebhookDeliveries != 0 {
		t.Fatalf("nothing is old yet: %+v", soon)
	}
	later := now + int64(cfg.ResolvedIncidentDays+1)*dayMs
	gone := must[store.Retained](t, "retain")(s.ApplyRetention(ctx, cfg, later))
	if gone.Incidents != 1 || gone.WebhookDeliveries != 1 {
		t.Fatalf("gone: %+v", gone)
	}
	var left []uuid.UUID
	tn = tenant(t, s, o)
	rows, err := tn.TestQuery(ctx, "SELECT id FROM incidents ORDER BY opened_at")
	if err != nil {
		t.Fatalf("incidents: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("incident: %v", err)
		}
		left = append(left, id)
	}
	if rows.Err() != nil || len(left) != 1 || left[0] != open {
		t.Fatalf("open ones stay: %v, %v", left, rows.Err())
	}
	var deliveries int64
	if err := tn.TestQueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE endpoint_id = $1", endpoint).
		Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("the pending one stays: %d, %v", deliveries, err)
	}
}

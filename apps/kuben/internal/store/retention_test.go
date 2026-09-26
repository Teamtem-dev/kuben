package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from retention.rs.

const dayMs = 24 * 3_600_000

func TestBudgetsAreAtLeastADay(t *testing.T) {
	if got := store.RetentionBefore(10*dayMs, 0); got != 9*dayMs {
		t.Fatalf("0 days: %d", got)
	}
	if got := store.RetentionBefore(10*dayMs, 3); got != 7*dayMs {
		t.Fatalf("3 days: %d", got)
	}
}

// openIncident opens an incident of kind `backup.stale` with key.
func openIncident(t *testing.T, tn *store.Tenant, key string) uuid.UUID {
	t.Helper()
	id, _, err := tn.OpenIncident(t.Context(), store.NewIncident{
		Kind: "backup.stale", Severity: "critical", DedupeKey: key, Title: "t",
	})
	if err != nil {
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
	if !must[bool](t, "resolve")(tn.ResolveIncident(ctx, old, "u")) {
		t.Fatal("resolve")
	}
	sealed := store.SealedBytes{Ciphertext: []byte{1}, WrappedKey: []byte{2}, KeyVersion: 1}
	if err := tn.CreateEndpoint(ctx, endpoint, "e", "https://a.example", []string{"*"}, sealed, "u"); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	for range 2 {
		must[uint64](t, "queue")(tn.EnqueueEvent(ctx, uuid.Must(uuid.NewV7()), "ping", map[string]any{}))
	}
	commit(t, tn)
	taken := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 10, 1))
	if err := s.DeliverySucceeded(ctx, taken[0].ID, 200); err != nil {
		t.Fatalf("done: %v", err)
	}

	now := nowMs()
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
	tn = tenant(t, s, o)
	left := must[[]store.Incident](t, "read")(tn.Incidents(ctx, true, 10))
	if len(left) != 1 || left[0].ID != open {
		t.Fatalf("open ones stay: %+v", left)
	}
	if deliveries := must[[]store.DeliveryRecord](t, "read")(tn.Deliveries(ctx, endpoint, 10)); len(deliveries) != 1 {
		t.Fatalf("the pending one stays: %+v", deliveries)
	}
}

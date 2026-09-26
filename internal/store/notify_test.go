package store_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from repo/notify.rs.

const notifyBy = "user:a"

func backupIncident(key, detail string) store.NewIncident {
	return store.NewIncident{
		Kind:      "backup.stale",
		Severity:  "critical",
		DedupeKey: key,
		Title:     "Backups are stale",
		Detail:    opt.Some(detail),
	}
}

func sealedFixture() store.SealedBytes {
	return store.SealedBytes{Ciphertext: []byte{1, 2, 3}, WrappedKey: []byte{4, 5}, KeyVersion: 1}
}

type opened struct {
	id  uuid.UUID
	new bool
}

func openIt(t *testing.T, tn *store.Tenant, i store.NewIncident) opened {
	t.Helper()
	id, isNew, err := tn.OpenIncident(t.Context(), i)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return opened{id, isNew}
}

func TestIncidentsAreDeduplicatedAcknowledgedAndResolved(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	other := org(t, s, "b", "B")
	tn := tenant(t, s, o)
	first := openIt(t, tn, backupIncident("k", "one"))
	if !first.new {
		t.Fatal("opened")
	}
	if again := openIt(t, tn, backupIncident("k", "two")); again != (opened{first.id, false}) {
		t.Fatalf("counted again: %+v", again)
	}
	open := must[[]store.Incident](t, "read")(tn.Incidents(ctx, false, 10))
	if len(open) != 1 || open[0].Occurrences != 2 || open[0].Detail != opt.Some("two") {
		t.Fatalf("open: %+v", open)
	}
	if !must[bool](t, "ack")(tn.AcknowledgeIncident(ctx, first.id, notifyBy)) {
		t.Fatal("ack")
	}
	if must[bool](t, "ack")(tn.AcknowledgeIncident(ctx, first.id, notifyBy)) {
		t.Fatal("once")
	}
	if id, ok, err := tn.ResolveIncidentKey(ctx, "k", "system"); err != nil || !ok || id != first.id {
		t.Fatalf("resolve: %v %v %v", id, ok, err)
	}
	if _, ok, err := tn.ResolveIncidentKey(ctx, "k", "system"); err != nil || ok {
		t.Fatalf("resolve again: %v %v", ok, err)
	}
	if must[bool](t, "resolve")(tn.ResolveIncident(ctx, first.id, notifyBy)) {
		t.Fatal("resolved already")
	}
	if got := must[[]store.Incident](t, "read")(tn.Incidents(ctx, false, 10)); len(got) != 0 {
		t.Fatalf("none open: %+v", got)
	}
	reopened := openIt(t, tn, backupIncident("k", "three"))
	if !reopened.new || reopened.id == first.id {
		t.Fatal("a new incident after resolution")
	}
	if !must[bool](t, "resolve")(tn.ResolveIncident(ctx, reopened.id, notifyBy)) {
		t.Fatal("resolve")
	}
	all := must[[]store.Incident](t, "read")(tn.Incidents(ctx, true, 10))
	if len(all) != 2 {
		t.Fatalf("all: %+v", all)
	}
	for _, i := range all {
		if i.ResolvedAt.IsNone() {
			t.Fatalf("resolved: %+v", i)
		}
	}
	commit(t, tn)
	tn = tenant(t, s, other)
	if got := must[[]store.Incident](t, "read")(tn.Incidents(ctx, true, 10)); len(got) != 0 {
		t.Fatalf("isolated: %+v", got)
	}
	if must[bool](t, "ack")(tn.AcknowledgeIncident(ctx, first.id, notifyBy)) {
		t.Fatal("not theirs")
	}
}

func TestDeliveriesAreQueuedOnceRetriedAndGivenUp(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	all, deploys := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tn := tenant(t, s, o)
	if err := tn.CreateEndpoint(ctx, all, "all", "https://a.example/hook", []string{"*"}, sealedFixture(), notifyBy); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tn.CreateEndpoint(ctx, deploys, "deploys", "https://b.example/hook",
		[]string{"deployment.succeeded"}, sealedFixture(), notifyBy); err != nil {
		t.Fatalf("create: %v", err)
	}
	event := uuid.Must(uuid.NewV7())
	payload := map[string]any{"hello": "world"}
	queue := func(id uuid.UUID, name string, want uint64) {
		t.Helper()
		if got := must[uint64](t, "queue")(tn.EnqueueEvent(ctx, id, name, payload)); got != want {
			t.Fatalf("queued %d, want %d", got, want)
		}
	}
	queue(event, "deployment.succeeded", 2)
	queue(event, "deployment.succeeded", 0)
	queue(uuid.Must(uuid.NewV7()), "build.failed", 1)
	url, secret, ok, err := tn.EndpointSecret(ctx, deploys)
	if err != nil || !ok || url != "https://b.example/hook" {
		t.Fatalf("secret: %q %v %v", url, ok, err)
	}
	if diff := cmp.Diff(sealedFixture(), secret); diff != "" {
		t.Fatalf("sealed (-want +got):\n%s", diff)
	}
	commit(t, tn)

	taken := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 10, 60_000))
	if len(taken) != 3 {
		t.Fatalf("taken: %+v", taken)
	}
	for _, d := range taken {
		if d.Org != o || d.Attempts != 1 {
			t.Fatalf("taken: %+v", d)
		}
	}
	if got := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 10, 60_000)); len(got) != 0 {
		t.Fatalf("leased: %+v", got)
	}
	to := func(endpoint uuid.UUID) []store.WebhookDelivery {
		var out []store.WebhookDelivery
		for _, d := range taken {
			if d.Endpoint == endpoint {
				out = append(out, d)
			}
		}
		return out
	}
	ok0 := to(deploys)[0]
	if diff := cmp.Diff(any(payload), ok0.Payload); diff != "" {
		t.Fatalf("payload (-want +got):\n%s", diff)
	}
	if err := s.DeliverySucceeded(ctx, ok0.ID, 204); err != nil {
		t.Fatalf("record: %v", err)
	}
	rest := to(all)
	retried, failed := rest[0], rest[1]
	if err := s.DeliveryFailed(ctx, retried.ID, opt.Some[int32](503), "unavailable", opt.Some[int64](0)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.DeliveryFailed(ctx, failed.ID, opt.None[int32](), "refused", opt.None[int64]()); err != nil {
		t.Fatalf("record: %v", err)
	}
	again := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 10, 60_000))
	if len(again) != 1 || again[0].ID != retried.ID || again[0].Attempts != 2 {
		t.Fatalf("again: %+v", again)
	}

	tn = tenant(t, s, o)
	records := must[[]store.DeliveryRecord](t, "read")(tn.Deliveries(ctx, all, 10))
	found := false
	for _, r := range records {
		if r.ID == failed.ID {
			found = true
			if r.Status != "failed" || r.LastError != opt.Some("refused") {
				t.Fatalf("failed: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("listed")
	}
	type nameFailures struct {
		Name     string
		Failures int32
	}
	var got []nameFailures
	for _, e := range must[[]store.Endpoint](t, "read")(tn.Endpoints(ctx)) {
		got = append(got, nameFailures{e.Name, e.Failures})
	}
	if diff := cmp.Diff([]nameFailures{{"all", 1}, {"deploys", 0}}, got); diff != "" {
		t.Fatalf("endpoints (-want +got):\n%s", diff)
	}
	if !must[bool](t, "disable")(tn.DisableEndpoint(ctx, all)) {
		t.Fatal("disable")
	}
	if must[bool](t, "disable")(tn.DisableEndpoint(ctx, all)) {
		t.Fatal("once")
	}
	if must[bool](t, "ping")(tn.EnqueueTo(ctx, all, "ping", payload)) {
		t.Fatal("disabled")
	}
	if !must[bool](t, "ping")(tn.EnqueueTo(ctx, deploys, "ping", payload)) {
		t.Fatal("ping")
	}
	queue(uuid.Must(uuid.NewV7()), "build.failed", 0)
	commit(t, tn)
}

func TestEndpointsAreDisabledAfterRepeatedFailures(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	id := uuid.Must(uuid.NewV7())
	tn := tenant(t, s, o)
	if err := tn.CreateEndpoint(ctx, id, "flaky", "https://a.example/hook", []string{"*"}, sealedFixture(), notifyBy); err != nil {
		t.Fatalf("create: %v", err)
	}
	for range store.MaxEndpointFailures {
		must[uint64](t, "queue")(tn.EnqueueEvent(ctx, uuid.Must(uuid.NewV7()), "build.failed", map[string]any{}))
	}
	commit(t, tn)
	taken := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 100, 60_000))
	if len(taken) != int(store.MaxEndpointFailures) {
		t.Fatalf("taken %d", len(taken))
	}
	fail := func(d store.WebhookDelivery) {
		t.Helper()
		if err := s.DeliveryFailed(ctx, d.ID, opt.Some[int32](500), "boom", opt.None[int64]()); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	fail(taken[0])
	tn = tenant(t, s, o)
	if !must[bool](t, "retry")(tn.RetryDelivery(ctx, id, taken[0].ID)) {
		t.Fatal("a failed delivery goes again")
	}
	if must[bool](t, "retry")(tn.RetryDelivery(ctx, id, taken[0].ID)) {
		t.Fatal("once")
	}
	commit(t, tn)
	again := must[[]store.WebhookDelivery](t, "take")(s.TakeDeliveries(ctx, 100, 60_000))
	if len(again) != 1 || again[0].ID != taken[0].ID || again[0].Attempts != 1 {
		t.Fatalf("again: %+v", again)
	}
	for _, d := range taken[1:] {
		fail(d)
	}
	tn = tenant(t, s, o)
	endpoints := must[[]store.Endpoint](t, "read")(tn.Endpoints(ctx))
	endpoint := endpoints[len(endpoints)-1]
	if endpoint.DisabledAt.IsNone() || endpoint.Failures != store.MaxEndpointFailures {
		t.Fatalf("endpoint: %+v", endpoint)
	}
	if _, _, ok, err := tn.EndpointSecret(ctx, id); err != nil || ok {
		t.Fatalf("secret: %v %v", ok, err)
	}
}

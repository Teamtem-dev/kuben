package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/notify"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// routes/incidents.rs incident_kinds_link_their_runbook.
func TestIncidentKindsLinkTheirRunbook(t *testing.T) {
	for _, kind := range []string{"deployment.failed", "build.failed", "backup.stale"} {
		if url, ok := notify.Runbook(kind); !ok || !strings.HasPrefix(url, notify.Runbooks) {
			t.Errorf("%s: %q", kind, url)
		}
	}
	if url, ok := notify.Runbook("something.else"); ok {
		t.Errorf("something.else: %q", url)
	}
}

// routes/incidents.rs endpoints_subscribe_to_known_events.
func TestEndpointsSubscribeToKnownEvents(t *testing.T) {
	body := func(name string, events ...string) *gen.CreateEndpoint {
		return &gen.CreateEndpoint{Name: name, URL: "https://hooks.example.com/kuben", Events: events}
	}
	events, err := api.CheckEndpoint(body("pager", "deployment.failed", "deployment.failed", "*"))
	if diff := cmp.Diff([]string{"*", "deployment.failed"}, events); err != nil || diff != "" {
		t.Fatalf("events (-want +got):\n%s %v", diff, err)
	}
	for _, bad := range []*gen.CreateEndpoint{body("", "*"), body("pager"), body("pager", "deploy.everything")} {
		if _, err := api.CheckEndpoint(bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// receivedDelivery is a request the receiver got.
type receivedDelivery struct {
	header http.Header
	body   []byte
}

// receiver is tests/http.rs receiver: a webhook receiver on loopback;
// every request goes to the channel and is answered with 204.
func receiver(t *testing.T) (string, <-chan receivedDelivery) {
	t.Helper()
	got := make(chan receivedDelivery, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("the body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
		got <- receivedDelivery{header: r.Header.Clone(), body: body}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/hook", got
}

// notifier is the notifier the test server would run (tests/http.rs
// notifier).
func (f fixture) notifier() *notify.Notifier {
	return notify.New(notify.Deps{
		Store:     f.store,
		Keyring:   testKeyring(),
		Config:    config.NotifyCfg{AllowPrivateTargets: true, AllowHTTP: true},
		PublicURL: opt.Some("https://kuben.example.com"),
	})
}

// failOperation fails the deployment a request started, as the
// materializer would (tests/http.rs fail_operation).
func (f fixture) failOperation() {
	t := f.t
	claim, ok, err := f.store.ClaimOperation(t.Context(), "test", []string{store.RunKind}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if done, err := f.store.FinishOperation(t.Context(), claim, "failed", opt.Some("RolloutFailed")); err != nil || !done {
		t.Fatalf("finish: %v %v", done, err)
	}
}

// tests/http.rs m4_signed_webhooks_and_incidents: webhooks are created by
// admins, signed, delivered and listed; a failed deployment opens an
// incident that can be acknowledged and resolved.
func TestM4SignedWebhooksAndIncidents(t *testing.T) {
	f := newFixtureWith(t, func(cfg *config.Config) {
		cfg.Notify.AllowPrivateTargets = true
		cfg.Notify.AllowHTTP = true
	})
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	url, received := receiver(t)
	endpoint := map[string]any{"name": "pager", "url": url, "events": []string{"deployment.failed", "incident.opened"}}
	const hooks = "/api/v1/webhooks"
	if status := bob.status("POST", hooks, endpoint); status != http.StatusForbidden {
		t.Fatalf("a viewer made a webhook: %d", status)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "url": url, "events": []string{"nope"}},
		{"name": "x", "url": "ftp://example.com/", "events": []string{"*"}},
	} {
		if status := alice.status("POST", hooks, bad); status != http.StatusUnprocessableEntity {
			t.Fatalf("%v: %d", bad, status)
		}
	}
	status, made, _ := alice.do("POST", hooks, endpoint)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, made)
	}
	if status := alice.status("POST", hooks, endpoint); status != http.StatusConflict {
		t.Fatalf("the name is taken: %d", status)
	}
	secret, _ := made["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret: %v", made)
	}
	_, listed := alice.list(hooks)
	if len(listed) != 1 {
		t.Fatalf("listed: %v", listed)
	}
	if _, shown := listed[0]["secret"]; shown {
		t.Fatal("shown once")
	}
	id, _ := made["id"].(string)
	if status := alice.status("POST", hooks+"/"+id+"/ping", map[string]any{}); status != http.StatusAccepted {
		t.Fatalf("ping: %d", status)
	}

	if status, body, _ := alice.deploy(0, ""); status != http.StatusAccepted {
		t.Fatalf("deploy: %d %v", status, body)
	}
	f.failOperation()
	n := f.notifier()
	if consumed, err := n.Consume(t.Context()); err != nil || consumed < 2 {
		t.Fatalf("accepted and settled: %d %v", consumed, err)
	}
	if delivered, err := n.Deliver(t.Context()); err != nil || delivered != 3 {
		t.Fatalf("ping, failure, incident: %d %v", delivered, err)
	}
	events := receivedEvents(t, received, secret, 3)
	if diff := cmp.Diff([]string{"deployment.failed", "incident.opened", "ping"}, events); diff != "" {
		t.Fatalf("events (-want +got):\n%s", diff)
	}
	_, deliveries := alice.list(hooks + "/" + id + "/deliveries")
	if len(deliveries) == 0 {
		t.Fatal("no deliveries listed")
	}
	for _, d := range deliveries {
		if d["status"] != "delivered" {
			t.Fatalf("deliveries: %v", deliveries)
		}
	}
	delivered, _ := deliveries[0]["id"].(string)
	retry := hooks + "/" + id + "/deliveries/" + delivered + "/retry"
	if status := alice.status("POST", retry, map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("only failed deliveries are retried: %d", status)
	}
	incidentsAreHandled(t, alice, bob)
}

// receivedEvents reads n deliveries, checks their signatures and returns
// their events, sorted.
func receivedEvents(t *testing.T, received <-chan receivedDelivery, secret string, n int) []string {
	t.Helper()
	var events []string
	for range n {
		var d receivedDelivery
		select {
		case d = <-received:
		case <-time.After(10 * time.Second):
			t.Fatal("a delivery is missing")
		}
		signature := d.header.Get("Kuben-Signature")
		stamp, _, _ := strings.Cut(strings.TrimPrefix(signature, "t="), ",")
		at, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			t.Fatalf("signed: %q", signature)
		}
		if want := notify.Signature([]byte(secret), at, d.body); signature != want {
			t.Fatalf("signature %q, want %q", signature, want)
		}
		events = append(events, d.header.Get("Kuben-Event"))
	}
	slices.Sort(events)
	return events
}

// incidentsAreHandled is tests/http.rs incidents_are_handled: the failed
// deployment's incident is listed, acknowledged and resolved.
func incidentsAreHandled(t *testing.T, alice, bob *client) {
	t.Helper()
	_, open := alice.list("/api/v1/incidents")
	if len(open) != 1 {
		t.Fatalf("open: %v", open)
	}
	if open[0]["kind"] != "deployment.failed" || open[0]["severity"] != "critical" || open[0]["detail"] != "RolloutFailed" {
		t.Fatalf("incident: %v", open[0])
	}
	incident, _ := open[0]["id"].(string)
	ack := "/api/v1/incidents/" + incident + "/acknowledge"
	if status := bob.status("POST", ack, map[string]any{}); status != http.StatusForbidden {
		t.Fatalf("a viewer acknowledged: %d", status)
	}
	if status := alice.status("POST", ack, map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("acknowledge: %d", status)
	}
	resolve := "/api/v1/incidents/" + incident + "/resolve"
	if status := alice.status("POST", resolve, map[string]any{}); status != http.StatusNoContent {
		t.Fatalf("resolve: %d", status)
	}
	if status := alice.status("POST", resolve, map[string]any{}); status != http.StatusConflict {
		t.Fatalf("resolve twice: %d", status)
	}
	if status, open := alice.list("/api/v1/incidents"); status != http.StatusOK || len(open) != 0 {
		t.Fatalf("none open: %d %v", status, open)
	}
	_, all := alice.list("/api/v1/incidents?all=true")
	if len(all) == 0 {
		t.Fatal("no incidents listed")
	}
	if by, _ := all[0]["acknowledgedBy"].(string); !strings.HasPrefix(by, "user:") {
		t.Fatalf("all: %v", all)
	}
}

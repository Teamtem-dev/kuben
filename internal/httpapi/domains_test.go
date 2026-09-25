package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/health"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/integrations/dns"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/notify"
)

// fakeDNS is tests/http.rs FakeDns: every challenge name answers the TXT
// value txt (none when empty), every zone is served by Cloudflare, and a
// provider whose token is `good` holds the zone example.com.
type fakeDNS struct {
	mu      sync.Mutex
	txt     string
	records []dns.ProviderRecord
}

func (f *fakeDNS) setTXT(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txt = value
}

func (f *fakeDNS) TXT(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.txt == "" {
		return []string{}, nil
	}
	return []string{f.txt}, nil
}

func (f *fakeDNS) NS(context.Context, string) ([]string, error) {
	return []string{"ada.ns.cloudflare.com"}, nil
}

func (f *fakeDNS) Provider(kind, token string) (dns.Provider, bool) {
	if kind != "cloudflare" {
		return nil, false
	}
	return fakeProvider{good: token == "good", dns: f}, true
}

// fakeProvider is tests/http.rs FakeProvider, over fakeDNS's records.
type fakeProvider struct {
	good bool
	dns  *fakeDNS
}

func (p fakeProvider) Verify(context.Context) error {
	if !p.good {
		return dns.Error{Kind: dns.Refused, Detail: "bad token"}
	}
	return nil
}

func (fakeProvider) ZoneFor(_ context.Context, name string) (dns.Zone, bool, error) {
	if name == "example.com" || strings.HasSuffix(name, ".example.com") {
		return dns.Zone{ID: "z1", Name: "example.com", NameServers: []string{}}, true, nil
	}
	return dns.Zone{}, false, nil
}

func (p fakeProvider) Records(_ context.Context, _ dns.Zone, name string) ([]dns.ProviderRecord, error) {
	p.dns.mu.Lock()
	defer p.dns.mu.Unlock()
	var out []dns.ProviderRecord
	for _, r := range p.dns.records {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out, nil
}

func (p fakeProvider) Create(_ context.Context, _ dns.Zone, spec dns.RecordSpec, tag string) (dns.ProviderRecord, error) {
	p.dns.mu.Lock()
	defer p.dns.mu.Unlock()
	record := dns.ProviderRecord{
		ID: fmt.Sprintf("r%d", len(p.dns.records)+1), Name: spec.Name, RecordType: spec.RecordType,
		Content: spec.Content, Comment: opt.Some(tag),
	}
	p.dns.records = append(p.dns.records, record)
	return record, nil
}

func (p fakeProvider) Update(ctx context.Context, zone dns.Zone, id string, spec dns.RecordSpec, tag string) (dns.ProviderRecord, error) {
	if err := p.Delete(ctx, zone, id); err != nil {
		return dns.ProviderRecord{}, err
	}
	return p.Create(ctx, zone, spec, tag)
}

func (p fakeProvider) Delete(_ context.Context, _ dns.Zone, id string) error {
	p.dns.mu.Lock()
	defer p.dns.mu.Unlock()
	p.dns.records = slices.DeleteFunc(p.dns.records, func(r dns.ProviderRecord) bool { return r.ID == id })
	return nil
}

// serverOn is a server on f's store and projections, with the keyring and
// images of the tests and the deps edit changes; its URL.
func serverOn(t *testing.T, f fixture, edit func(*api.Deps)) string {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Bind = "127.0.0.1:3000"
	h := health.New(clock.System{})
	h.SetReady(true)
	deps := api.Deps{
		Config: cfg, Store: f.store, Hasher: auth.InsecureForTests(), Health: h,
		Projections: f.projections, Images: privateImages{testImages(t)},
		Keyring: opt.Some(testKeyring()),
	}
	edit(&deps)
	server, err := api.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// domainsServer is serverOn with the fake DNS and records pointed at
// lb.example.net, as tests/http.rs's m5 domain scenario sets it up.
func domainsServer(t *testing.T, f fixture, fake *fakeDNS) string {
	t.Helper()
	return serverOn(t, f, func(d *api.Deps) {
		d.Config.Domains.CnameTarget = opt.Some("lb.example.net")
		d.DNS = fake
	})
}

// Ported from tests/http.rs: m5_domain_claims_and_dns_records. Domains are
// claimed and verified by TXT or a provider, another organization's domain
// is refused, and an app's records are written once.
func TestDomainClaimsAndDNSRecords(t *testing.T) {
	f := newFixture(t)
	_, _, tgt := f.sqlApp()
	fake := &fakeDNS{}
	base := domainsServer(t, f, fake)
	alice := signInAt(t, base, "alice@example.com")
	bob := signInAt(t, base, "bob@example.com")

	body := map[string]any{"domain": "Shop.Example.com."}
	if status := bob.status("POST", "/api/v1/domains", body); status != http.StatusForbidden {
		t.Fatalf("a viewer claims: %d", status)
	}
	status, claim, _ := alice.do("POST", "/api/v1/domains", body)
	if status != http.StatusCreated || claim["domain"] != "shop.example.com" {
		t.Fatalf("claim: %d %v", status, claim)
	}
	if claim["challengeName"] != "_kuben-challenge.shop.example.com" {
		t.Fatalf("challenge: %v", claim)
	}
	if status := alice.status("POST", "/api/v1/domains", body); status != http.StatusConflict {
		t.Fatalf("claimed twice: %d", status)
	}
	verify := fmt.Sprintf("/api/v1/domains/%s/verify", claim["id"])
	_, pending, _ := alice.do("POST", verify, map[string]any{})
	if lastError, _ := pending["lastError"].(string); pending["status"] != "pending" ||
		!strings.Contains(lastError, "no TXT record") {
		t.Fatalf("pending: %v", pending)
	}
	value, _ := claim["challengeValue"].(string)
	fake.setTXT(value)
	_, verified, _ := alice.do("POST", verify, map[string]any{})
	if verified["status"] != "verified" || verified["method"] != "txt" {
		t.Fatalf("verified: %v", verified)
	}
	fake.setTXT("")

	const providers = "/api/v1/dns-providers"
	for _, refused := range []map[string]any{
		{"name": "cf", "kind": "cloudflare", "token": "bad"},
		{"name": "cf", "kind": "route53", "token": "good"},
	} {
		if status := alice.status("POST", providers, refused); status != http.StatusUnprocessableEntity {
			t.Fatalf("%v: %d", refused, status)
		}
	}
	if status := alice.status("POST", providers, map[string]any{"name": "cf", "kind": "cloudflare", "token": "good"}); status != http.StatusCreated {
		t.Fatalf("provider: %d", status)
	}
	_, apiClaim, _ := alice.do("POST", "/api/v1/domains", map[string]any{"domain": "api.example.com"})
	verify = fmt.Sprintf("/api/v1/domains/%s/verify", apiClaim["id"])
	if _, byProvider, _ := alice.do("POST", verify, map[string]any{"provider": "cf"}); byProvider["method"] != "cloudflare" {
		t.Fatalf("by provider: %v", byProvider)
	}
	appRecords(t, f, alice, tgt)
}

// appRecords is tests/http.rs app_records.
func appRecords(t *testing.T, f fixture, alice *client, tgt ids.TargetID) {
	t.Helper()
	ctx := t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	project, _, err := tn.Project(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	config := jsonValue(t, `{"runtime":{"processes":{"web":{"port":8080}}},"domains":[{"host":"shop.example.com"}]}`)
	if _, _, err := tn.CreateConfigRevision(ctx, project.ID, tgt, config, "user:test"); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	const sync = "/api/v1/projects/shop/environments/prod/apps/api/dns"
	status, changes := alice.postList(sync, map[string]any{"provider": "cf"})
	if status != http.StatusOK || len(changes) == 0 {
		t.Fatalf("sync: %d %v", status, changes)
	}
	if c := changes[0]; c["action"] != "created" || c["recordType"] != "CNAME" || c["content"] != "lb.example.net" {
		t.Fatalf("created: %v", changes)
	}
	if _, again := alice.postList(sync, map[string]any{"provider": "cf"}); len(again) == 0 || again[0]["action"] != "unchanged" {
		t.Fatalf("again: %v", again)
	}

	rival, err := f.store.CreateOrg(ctx, "rival", "Rival")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := f.store.Tenant(ctx, rival.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	claim := uuid.Must(uuid.NewV7())
	if err := rt.CreateClaim(ctx, claim, "rival.io", strings.Repeat("x", 32), "user:r"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.VerifyClaim(ctx, claim, "txt", opt.None[uuid.UUID]()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	taken := map[string]any{"name": "web2", "image": "nginx:1.27", "port": 8080, "domains": []string{"www.rival.io"}}
	if status, body, _ := alice.do("POST", "/api/v1/projects/shop/environments/prod/apps", taken); status != http.StatusConflict {
		t.Fatalf("another organization's domain: %d %v", status, body)
	}
	if status := alice.status("POST", "/api/v1/domains", map[string]any{"domain": "rival.io"}); status != http.StatusConflict {
		t.Fatalf("claim another organization's domain: %d", status)
	}
}

// postList POSTs body and reads a JSON array.
func (c *client) postList(path string, body any) (int, []map[string]any) {
	c.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest("POST", c.base+path, bytes.NewReader(data))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set(httpx.ClientHeader, "console")
	req.Header.Set("Content-Type", "application/json")
	resp := c.send(req)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var out []map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("POST %s: %v in %s", path, err, raw)
		}
	}
	return resp.StatusCode, out
}

// Claims are listed (revoked ones on request) and revoked once; provider
// accounts are listed, named once and removed once; nothing is sealed
// without a keyring; a DNS pass needs a known account.
func TestClaimsAndProvidersAreListedRevokedAndRemoved(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := signInAt(t, domainsServer(t, f, &fakeDNS{}), "alice@example.com")

	_, claim, _ := alice.do("POST", "/api/v1/domains", map[string]any{"domain": "example.com"})
	if claim["method"] != nil || claim["verifiedAt"] != nil || claim["lastError"] != nil {
		t.Fatalf("a new claim's empty members are null: %v", claim)
	}
	if _, present := claim["lastCheckedAt"]; !present {
		t.Fatalf("null members are written: %v", claim)
	}
	revoke := fmt.Sprintf("/api/v1/domains/%s", claim["id"])
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"DELETE", revoke, http.StatusNoContent},
		{"DELETE", revoke, http.StatusNotFound},
		{"DELETE", "/api/v1/domains/" + uuid.NewString(), http.StatusNotFound},
		{"POST", "/api/v1/domains/" + uuid.NewString() + "/verify", http.StatusNotFound},
	} {
		var body any
		if c.method == "POST" {
			body = map[string]any{}
		}
		if got := alice.status(c.method, c.path, body); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if _, open := alice.list("/api/v1/domains"); len(open) != 0 {
		t.Fatalf("revoked claims are hidden: %v", open)
	}
	if _, all := alice.list("/api/v1/domains?all=true"); len(all) != 1 || all[0]["status"] != "revoked" {
		t.Fatalf("all claims: %v", all)
	}

	const providers = "/api/v1/dns-providers"
	good := map[string]any{"name": "cf", "kind": "cloudflare", "token": " good "}
	status, created, _ := alice.do("POST", providers, good)
	if status != http.StatusCreated || created["name"] != "cf" || created["kind"] != "cloudflare" {
		t.Fatalf("created: %d %v", status, created)
	}
	if _, present := created["token"]; present {
		t.Fatalf("the token is never shown: %v", created)
	}
	if status := alice.status("POST", providers, good); status != http.StatusConflict {
		t.Fatalf("named twice: %d", status)
	}
	for _, bad := range []map[string]any{
		{"name": "CF", "kind": "cloudflare", "token": "good"},
		{"name": "cf2", "kind": "cloudflare", "token": "  "},
		{"name": "cf2", "kind": "cloudflare", "token": strings.Repeat("g", 513)},
	} {
		if status := alice.status("POST", providers, bad); status != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", bad["name"], status)
		}
	}
	if _, listed := alice.list(providers); len(listed) != 1 || listed[0]["id"] != created["id"] {
		t.Fatalf("listed: %v", listed)
	}
	const sync = "/api/v1/projects/shop/environments/prod/apps/api/dns"
	if status, _ := alice.postList(sync, map[string]any{"provider": "nope"}); status != http.StatusNotFound {
		t.Fatalf("an unknown account: %d", status)
	}
	if status, changes := alice.postList(sync, map[string]any{"provider": "cf"}); status != http.StatusOK || len(changes) != 0 {
		t.Fatalf("an app without domains: %d %v", status, changes)
	}
	remove := fmt.Sprintf("%s/%s", providers, created["id"])
	if status := alice.status("DELETE", remove, nil); status != http.StatusNoContent {
		t.Fatalf("removed: %d", status)
	}
	if status := alice.status("DELETE", remove, nil); status != http.StatusNotFound {
		t.Fatalf("removed twice: %d", status)
	}

	locked := signInAt(t, serverOn(t, f, func(d *api.Deps) {
		d.DNS = &fakeDNS{}
		d.Keyring = opt.None[*keyring.Keyring]()
	}), "alice@example.com")
	if status := locked.status("POST", providers, good); status != http.StatusServiceUnavailable {
		t.Fatalf("without a keyring: %d", status)
	}
}

// Records point at the Gateway's addresses unless domains.cname_target is
// set: without it a DNS pass needs the cluster.
func TestADNSPassWithoutACnameTargetNeedsTheCluster(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	addProvider(t, f, "good")
	alice := signInAt(t, serverOn(t, f, func(d *api.Deps) { d.DNS = &fakeDNS{} }), "alice@example.com")
	const sync = "/api/v1/projects/shop/environments/prod/apps/api/dns"
	if status, _ := alice.postList(sync, map[string]any{"provider": "cf"}); status != http.StatusServiceUnavailable {
		t.Fatalf("no cluster: %d", status)
	}
}

// addProvider stores the DNS provider account cf of f's organization,
// holding token.
func addProvider(t *testing.T, f fixture, token string) {
	t.Helper()
	ctx := t.Context()
	id := uuid.Must(uuid.NewV7())
	sealed, err := notify.SealSecret(testKeyring(), f.org, id, []byte(token))
	if err != nil {
		t.Fatal(err)
	}
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := tn.CreateDNSProvider(ctx, id, "cf", "cloudflare", sealed, "user:test"); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

package dns_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/dns"
)

func record(id, kind, content string, comment opt.Val[string]) dns.ProviderRecord {
	return dns.ProviderRecord{ID: id, Name: "shop.example.com", RecordType: kind, Content: content, Comment: comment}
}

func spec(kind, content string) dns.RecordSpec {
	return dns.RecordSpec{Name: "shop.example.com", RecordType: kind, Content: content}
}

func TestDohAnswersAreRead(t *testing.T) {
	body := `{"Status":0,"Answer":[{"name":"x.","type":16,"data":"\"abc\" \"def\""},{"type":5,"data":"y."}]}`
	if got, err := dns.DoHAnswers([]byte(body), dns.TypeTXT); err != nil || !cmp.Equal(got, []string{"abcdef"}) {
		t.Fatalf("txt: %v %v", got, err)
	}
	ns := `{"Status":0,"Answer":[{"type":2,"data":"Ada.NS.cloudflare.com."}]}`
	if got, err := dns.DoHAnswers([]byte(ns), dns.TypeNS); err != nil || !cmp.Equal(got, []string{"ada.ns.cloudflare.com"}) {
		t.Fatalf("ns: %v %v", got, err)
	}
	if got, err := dns.DoHAnswers([]byte(`{"Status":3}`), dns.TypeTXT); err != nil || len(got) != 0 {
		t.Fatalf("no such name: %v %v", got, err)
	}
	if _, err := dns.DoHAnswers([]byte(`{"Status":2}`), dns.TypeTXT); err == nil {
		t.Fatal("server failure")
	}
	if _, err := dns.DoHAnswers([]byte("<html>"), dns.TypeTXT); err == nil {
		t.Fatal("not JSON")
	}
}

func TestPlansNeverTouchRecordsOfOthers(t *testing.T) {
	const tag = "kuben:org"
	wanted := []dns.RecordSpec{spec("A", "203.0.113.10")}
	cases := []struct {
		name     string
		wanted   []dns.RecordSpec
		existing []dns.ProviderRecord
		known    []string
		want     []dns.Change
	}{
		{"nothing there", wanted, nil, nil, []dns.Change{dns.Create{Spec: spec("A", "203.0.113.10")}}},
		{
			"theirs", wanted,
			[]dns.ProviderRecord{record("1", "A", "198.51.100.1", opt.None[string]())},
			nil,
			[]dns.Change{dns.ConflictWith{What: "A shop.example.com"}},
		},
		{
			"ours, other content", wanted,
			[]dns.ProviderRecord{record("2", "A", "198.51.100.1", opt.Some(tag))},
			[]string{"2"},
			[]dns.Change{dns.Update{ID: "2", Spec: spec("A", "203.0.113.10")}},
		},
		{
			"ours, same", wanted,
			[]dns.ProviderRecord{record("3", "A", "203.0.113.10", opt.Some(tag))},
			nil,
			[]dns.Change{dns.Unchanged{ID: "3", Spec: spec("A", "203.0.113.10")}},
		},
		{
			"a CNAME blocked",
			[]dns.RecordSpec{spec("CNAME", "gw.example.net")},
			[]dns.ProviderRecord{record("4", "A", "198.51.100.1", opt.None[string]())},
			nil,
			[]dns.Change{dns.ConflictWith{What: "CNAME shop.example.com"}},
		},
	}
	for _, c := range cases {
		if diff := cmp.Diff(c.want, dns.Plan(c.wanted, c.existing, c.known, tag)); diff != "" {
			t.Errorf("%s: %s", c.name, diff)
		}
	}
}

func TestOnlyRecordedRecordsAreDeleted(t *testing.T) {
	const tag = "kuben:org"
	existing := []dns.ProviderRecord{
		record("5", "AAAA", "2001:db8::1", opt.Some(tag)),
		record("6", "AAAA", "2001:db8::2", opt.Some(tag)),
		record("7", "TXT", "x", opt.None[string]()),
	}
	want := []dns.Change{dns.Delete{ID: "5", Name: "shop.example.com", RecordType: "AAAA"}}
	if diff := cmp.Diff(want, dns.Plan(nil, existing, []string{"5"}, tag)); diff != "" {
		t.Fatal(diff)
	}
}

func TestGatewaysArePointedAtByAddressOrName(t *testing.T) {
	addresses := []netip.Addr{
		netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("2001:db8::1"),
	}
	want := []dns.RecordSpec{spec("A", "203.0.113.10"), spec("AAAA", "2001:db8::1")}
	if diff := cmp.Diff(want, dns.GatewayRecords("shop.example.com", addresses, opt.None[string]())); diff != "" {
		t.Fatalf("private addresses are never published: %s", diff)
	}
	cname := dns.GatewayRecords("shop.example.com", addresses, opt.Some("lb.example.net."))
	if diff := cmp.Diff([]dns.RecordSpec{spec("CNAME", "lb.example.net")}, cname); diff != "" {
		t.Fatal(diff)
	}
	if got := dns.QueryValue("a b&c"); got != "a%20b%26c" {
		t.Fatal(got)
	}
}

// fakeCloudflare answers the API calls the adapter makes.
func fakeCloudflare(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/user/tokens/verify":
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{"status":"active"}}`)
		case r.URL.Path == "/zones" && r.URL.Query().Get("name") == "example.com":
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":[{"id":"z1","name":"example.com","name_servers":["ada.ns.cloudflare.com"]}]}`)
		case r.URL.Path == "/zones":
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":[]}`)
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"success":true,"errors":[],"result":{"id":"r1","name":"shop.example.com","type":"A","content":"203.0.113.10","comment":"kuben:org"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"message":"bad","code":1004}],"result":null}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCloudflareIsSpokenAsTheAPIExpects(t *testing.T) {
	seen := []string{}
	srv := fakeCloudflare(t, &seen)
	cf := dns.NewCloudflare(srv.URL+"/", "secret")
	if err := cf.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone, ok, err := cf.ZoneFor(t.Context(), "shop.example.com")
	if err != nil || !ok || zone.ID != "z1" || !cmp.Equal(zone.NameServers, []string{"ada.ns.cloudflare.com"}) {
		t.Fatalf("zone: %+v %v %v", zone, ok, err)
	}
	created, err := cf.Create(t.Context(), zone, spec("A", "203.0.113.10"), "kuben:org")
	if err != nil || created.ID != "r1" || created.Comment != opt.Some("kuben:org") {
		t.Fatalf("create: %+v %v", created, err)
	}
	var want struct {
		Content string `json:"content"`
		Comment string `json:"comment"`
	}
	_, body, _ := strings.Cut(seen[len(seen)-1], " {")
	body = "{" + body
	if err := json.Unmarshal([]byte(body), &want); err != nil || want.Comment != "kuben:org" || want.Content != "203.0.113.10" ||
		!strings.HasPrefix(seen[len(seen)-1], "POST /zones/z1/dns_records ") {
		t.Fatalf("the create request: %s", seen[len(seen)-1])
	}
	err = cf.Delete(t.Context(), zone, "r 1")
	var e dns.Error
	if !errors.As(err, &e) || e.Kind != dns.Unavailable || e.Error() != `DNS is unavailable: HTTP 400 Bad Request: [{"code":1004,"message":"bad"}]` {
		t.Fatalf("an API error: %v", err)
	}
	if !strings.HasPrefix(seen[len(seen)-1], "DELETE /zones/z1/dns_records/r%201 ") {
		t.Fatalf("ids are escaped: %s", seen[len(seen)-1])
	}
	if err := dns.NewCloudflare(srv.URL, "wrong").Verify(t.Context()); !errors.As(err, &e) || e.Kind != dns.Refused ||
		e.Error() != "the DNS provider refused the credentials: HTTP 403 Forbidden" {
		t.Fatalf("refused: %v", err)
	}
	if err := dns.NewCloudflare("http://example.invalid", "x").Verify(t.Context()); err == nil {
		t.Fatal("an unreachable API")
	}
	if err := dns.NewCloudflare("ftp://example.com", "x").Verify(t.Context()); err == nil {
		t.Fatal("only https (or http for tests) is spoken")
	}
}

func TestTheResolverAsksForJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/dns-json" || r.URL.Query().Get("type") != "16" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"Status":0,"Answer":[{"type":16,"data":"\"kuben-verify=abc\""}]}`)
	}))
	t.Cleanup(srv.Close)
	r := dns.NewPlainResolver(srv.URL + "/dns-query/")
	if got, err := r.TXT(t.Context(), "_kuben.shop.example.com"); err != nil || !cmp.Equal(got, []string{"kuben-verify=abc"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := r.NS(t.Context(), "example.com"); err == nil || !strings.Contains(err.Error(), "HTTP 400 Bad Request") {
		t.Fatalf("an HTTP error: %v", err)
	}
}

func TestTheResolverSpeaksHTTPSOnly(t *testing.T) {
	if _, err := dns.NewResolver("http://127.0.0.1:1/dns-query").TXT(t.Context(), "x"); err == nil ||
		!strings.Contains(err.Error(), "not https") {
		t.Fatalf("plain http: %v", err)
	}
}

package outbound_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
)

func TestPrivateAddressesAreToldApart(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "127.0.0.1": true, "169.254.1.1": true,
		"0.0.0.0": true, "255.255.255.255": true, "100.64.0.1": true, "100.127.255.255": true,
		"100.128.0.1": false, "203.0.113.10": false, "8.8.8.8": false,
		"::1": true, "::": true, "fc00::1": true, "fd12::1": true, "fe80::1": true, "febf::1": true,
		"fec0::1": false, "2001:db8::1": false, "::ffff:10.0.0.1": true, "::ffff:8.8.8.8": false,
	} {
		if got := outbound.IsPrivate(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestFetchReadsAnAnswerWithinItsLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
			return
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	c := outbound.New(true, 100*time.Millisecond)
	if status, body, err := c.Fetch(t.Context(), get(t, srv.URL+"/"), 10); err != nil || status != http.StatusTeapot || string(body) != "ok" {
		t.Fatalf("an answer: %d %q %v", status, body, err)
	}
	if _, _, err := c.Fetch(t.Context(), get(t, srv.URL+"/big"), 10); err == nil || err.Error() != "length limit exceeded" {
		t.Fatalf("a body over the limit: %v", err)
	}
	if _, _, err := c.Fetch(t.Context(), get(t, srv.URL+"/slow"), 10); err == nil || err.Error() != "timed out" {
		t.Fatalf("no headers in time: %v", err)
	}
	if status, _, err := c.Fetch(t.Context(), get(t, srv.URL+"/moved"), 1000); err != nil || status != http.StatusFound {
		t.Fatalf("redirects are not followed: %d %v", status, err)
	}
	if _, _, err := outbound.New(false, time.Second).Fetch(t.Context(), get(t, srv.URL+"/"), 10); err == nil ||
		!strings.Contains(err.Error(), "not https") {
		t.Fatalf("plain http where only https is allowed: %v", err)
	}
}

func TestSendAnswersTheStatusWithoutReadingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
	}))
	t.Cleanup(srv.Close)
	c := outbound.New(true, 100*time.Millisecond)
	if status, err := c.Send(t.Context(), get(t, srv.URL+"/")); err != nil || status != http.StatusAccepted {
		t.Fatalf("an answer with a large body: %d %v", status, err)
	}
	if _, err := c.Send(t.Context(), get(t, srv.URL+"/slow")); err == nil || err.Error() != "timed out" {
		t.Fatalf("no headers in time: %v", err)
	}
	if _, err := outbound.New(false, time.Second).Send(t.Context(), get(t, srv.URL+"/")); err == nil ||
		!strings.Contains(err.Error(), "not https") {
		t.Fatalf("plain http where only https is allowed: %v", err)
	}
}

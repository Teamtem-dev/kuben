package notify_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/internal/notify"
)

// Ported from crates/kuben-api/src/notify.rs.

func TestOutboxTopicsBecomeEvents(t *testing.T) {
	settled := func(topic, phase string) (notify.Plan, bool) {
		return notify.PlanOf(topic, map[string]any{"phase": phase, "code": "RolloutFailed"})
	}
	failed, ok := settled("deployment.settled", "failed")
	want := notify.Plan{
		Event: "deployment.failed", Phase: opt.Some("failed"), Code: opt.Some("RolloutFailed"),
		Incident: opt.Some(true), Status: opt.Some("failure"),
	}
	if !ok || failed != want {
		t.Fatalf("failed: %+v", failed)
	}
	if p, ok := settled("deployment.settled", "succeeded"); !ok || p.Event != "deployment.succeeded" || p.Incident != opt.Some(false) {
		t.Fatalf("succeeded: %+v", p)
	}
	if p, ok := settled("deployment.settled", "superseded"); ok {
		t.Fatalf("superseded: %+v", p)
	}
	if p, ok := notify.PlanOf("deployment.accepted", map[string]any{}); !ok || p.Event != "deployment.started" {
		t.Fatalf("accepted: %+v", p)
	}
	if p, ok := settled("build.settled", "failed"); !ok || !p.Build || p.Incident != opt.Some(true) {
		t.Fatalf("build: %+v", p)
	}
	if p, ok := notify.PlanOf("project.apply.settled", map[string]any{"phase": "succeeded"}); ok {
		t.Fatalf("project: %+v", p)
	}
}

func TestSignaturesAreHmacsOverTimeAndBody(t *testing.T) {
	sig := notify.Signature([]byte("secret"), 1_700_000_000, []byte("{}"))
	if !strings.HasPrefix(sig, "t=1700000000,v1=") || len(sig) != len("t=1700000000,v1=")+64 {
		t.Fatalf("shape: %s", sig)
	}
	if sig == notify.Signature([]byte("secret"), 1_700_000_001, []byte("{}")) ||
		sig == notify.Signature([]byte("other"), 1_700_000_000, []byte("{}")) {
		t.Fatal("time and secret are signed")
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("1700000000.{}"))
	if !strings.HasSuffix(sig, hex.EncodeToString(mac.Sum(nil))) {
		t.Fatal("receivers can verify it")
	}
}

// The signatures Rust wrote (testdata/compat/auth.json, compat_fixtures.rs).
func TestSignaturesAreTheBytesRustWrote(t *testing.T) {
	data, err := os.ReadFile("../../testdata/compat/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Signatures []struct {
			Secret string `json:"secret"`
			Body   string `json:"body"`
			Header string `json:"header"`
			T      int64  `json:"t"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture.Signatures) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	for _, s := range fixture.Signatures {
		if got := notify.Signature([]byte(s.Secret), s.T, []byte(s.Body)); got != s.Header {
			t.Errorf("%q at %d: %s, Rust %s", s.Body, s.T, got, s.Header)
		}
	}
}

func TestDeliveriesBackOffAndGiveUp(t *testing.T) {
	const now int64 = 10_000_000
	if at, ok := notify.RetryAt(1, now, now); !ok || at != now+60_000 {
		t.Fatalf("second try: %d %v", at, ok)
	}
	if at, ok := notify.RetryAt(8, now, now); !ok || at > now+6*3_600_000 {
		t.Fatalf("capped: %d %v", at, ok)
	}
	if _, ok := notify.RetryAt(notify.MaxAttempts, now, now); ok {
		t.Fatal("given up after the last attempt")
	}
	if _, ok := notify.RetryAt(1, 0, notify.MaxDeliveryAgeMs+1); ok {
		t.Fatal("a day old")
	}
}

func TestPrivateTargetsAreRecognized(t *testing.T) {
	for _, ip := range []string{
		"10.0.0.1", "127.0.0.1", "169.254.169.254", "192.168.1.1", "100.64.0.1", "::1", "fd00::1", "fe80::1",
		"::ffff:10.0.0.1",
	} {
		if !outbound.IsPrivate(netip.MustParseAddr(ip)) {
			t.Errorf("%s is private", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111", "100.128.0.1"} {
		if outbound.IsPrivate(netip.MustParseAddr(ip)) {
			t.Errorf("%s is public", ip)
		}
	}
}

func TestOnlyPublicHttpsTargetsAreAllowedByDefault(t *testing.T) {
	strict := config.NotifyCfg{}
	for _, url := range []string{
		"http://example.com/hook", "https://127.0.0.1/hook", "https://[::1]:8443/hook", "ftp://example.com",
	} {
		if err := notify.CheckTarget(t.Context(), url, strict); err == nil {
			t.Errorf("%s was allowed", url)
		}
	}
	if err := notify.CheckTarget(t.Context(), "https://[::1]:8443/hook", strict); err == nil ||
		err.Error() != "[::1] resolves to a private address (::1)" {
		t.Errorf("the reason: %v", err)
	}
	open := config.NotifyCfg{AllowPrivateTargets: true, AllowHTTP: true}
	if err := notify.CheckTarget(t.Context(), "http://127.0.0.1:8080/hook", open); err != nil {
		t.Errorf("allowed by configuration: %v", err)
	}
}

// A receiver's refusal is stored as Rust's `HTTP {status}` text: the http
// crate's reason phrases, not Go's.
func TestRefusalsUseTheReasonsRustWrote(t *testing.T) {
	for code, want := range map[int]string{
		200: "OK",
		203: "Non Authoritative Information",
		404: "Not Found",
		413: "Payload Too Large",
		414: "URI Too Long",
		416: "Range Not Satisfiable",
		503: "Service Unavailable",
		599: "<unknown status code>",
	} {
		if got := notify.CanonicalReason(code); got != want {
			t.Errorf("%d: %q, want %q", code, got, want)
		}
	}
}

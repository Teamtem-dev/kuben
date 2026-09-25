package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

const webhookSecret = "s3cret"

// webhookHandler is the whole HTTP surface with app as the GitHub App and
// no database: every delivery tested here is answered before one is read.
func webhookHandler(t *testing.T, app opt.Val[*github.App]) http.Handler {
	t.Helper()
	server, err := api.New(api.Deps{Config: config.Default(), GitHub: app})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

// signature is GitHub's `sha256=<hex HMAC>` of body.
func signature(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// delivery is a request to the webhook with headers (name, value pairs).
func delivery(method string, body []byte, headers ...string) *http.Request {
	r := httptest.NewRequest(method, api.GithubWebhookPath, bytes.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

// The webhook answers each refusal and the deliveries that need no
// database with the outcome routes/git.rs gave, outside the session and
// CSRF layers (GitHub sends neither a cookie nor the console header).
func TestWebhookDeliveriesAreCheckedBeforeAnythingIsRecorded(t *testing.T) {
	on := webhookHandler(t, opt.Some(github.WebhookOnly([]byte(webhookSecret))))
	ping := []byte(`{"zen":"Keep it logically awesome."}`)
	installation := []byte(`{"action":"created","installation":{"id":7,"account":{"login":"acme"}}}`)
	signed := func(event string, body []byte) *http.Request {
		return delivery("POST", body, "X-Hub-Signature-256", signature(body),
			"X-GitHub-Event", event, "X-GitHub-Delivery", "d-1")
	}
	cases := []struct {
		name    string
		handler http.Handler
		req     *http.Request
		status  int
		outcome string
	}{
		{"not configured", webhookHandler(t, opt.None[*github.App]()), signed("ping", ping), 404, "notConfigured"},
		{"no signature", on, delivery("POST", ping, "X-GitHub-Event", "ping", "X-GitHub-Delivery", "d-1"), 401, "badSignature"},
		{"a wrong signature", on, delivery("POST", ping, "X-Hub-Signature-256", signature([]byte("{}")),
			"X-GitHub-Event", "ping", "X-GitHub-Delivery", "d-1"), 401, "badSignature"},
		{
			"no event", on, delivery("POST", ping, "X-Hub-Signature-256", signature(ping), "X-GitHub-Delivery", "d-1"),
			400, "missingHeaders",
		},
		{
			"no delivery id", on, delivery("POST", ping, "X-Hub-Signature-256", signature(ping), "X-GitHub-Event", "ping"),
			400, "missingHeaders",
		},
		{"a delivery id that is not visible ASCII", on, delivery("POST", ping, "X-Hub-Signature-256", signature(ping),
			"X-GitHub-Event", "ping", "X-GitHub-Delivery", "dé"), 400, "missingHeaders"},
		{"an empty delivery id", on, delivery("POST", ping, "X-Hub-Signature-256", signature(ping),
			"X-GitHub-Event", "ping", "X-GitHub-Delivery", ""), 400, "badDeliveryId"},
		{"a long delivery id", on, delivery("POST", ping, "X-Hub-Signature-256", signature(ping),
			"X-GitHub-Event", "ping", "X-GitHub-Delivery", strings.Repeat("d", 129)), 400, "badDeliveryId"},
		{"a malformed push", on, signed("push", []byte("{}")), 400, "malformed"},
		{"ping", on, signed("ping", ping), 200, "pong"},
		{"an event Kuben does not read", on, signed("issues", ping), 202, "ignored"},
		{"a new installation", on, signed("installation", installation), 202, "linkInKuben"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.handler.ServeHTTP(rec, c.req)
		want := `{"outcome":"` + c.outcome + `"}`
		if rec.Code != c.status || rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %q %q, want %d %s", c.name, rec.Code, rec.Header().Get("Content-Type"), rec.Body, c.status, want)
		}
	}
	// The longest delivery id is fine.
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, delivery("POST", ping, "X-Hub-Signature-256", signature(ping),
		"X-GitHub-Event", "ping", "X-GitHub-Delivery", strings.Repeat("d", 128)))
	if rec.Code != 200 {
		t.Errorf("128 characters: %d %s", rec.Code, rec.Body)
	}
}

// The webhook answers only POST and has a body limit of its own (lib.rs
// router).
func TestTheWebhookIsMountedOnItsOwn(t *testing.T) {
	on := webhookHandler(t, opt.Some(github.WebhookOnly([]byte(webhookSecret))))
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, delivery("GET", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d %v", rec.Code, rec.Header())
	}
	big := bytes.Repeat([]byte("x"), 5<<20+1)
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, delivery("POST", big, "X-Hub-Signature-256", signature(big),
		"X-GitHub-Event", "ping", "X-GitHub-Delivery", "d-1"))
	if rec.Code != 413 {
		t.Errorf("a body over 5 MiB: %d", rec.Code)
	}
	body := bytes.Repeat([]byte(" "), 5<<20)
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, delivery("POST", body, "X-Hub-Signature-256", signature(body),
		"X-GitHub-Event", "ping", "X-GitHub-Delivery", "d-1"))
	if rec.Code != 200 {
		t.Errorf("a body of 5 MiB: %d", rec.Code)
	}
}

// provider_error: what GitHub's answer means for the caller.
func TestProviderErrorsBecomeProblems(t *testing.T) {
	other := errors.New("boom")
	cases := []struct {
		err  error
		want string
		code kerr.Code
	}{
		{build.NotFound{What: "installation 7"}, "validation failed: GitHub does not know installation 7", kerr.Validation},
		{build.Refused{Reason: "HTTP 401"}, "validation failed: GitHub refused access: HTTP 401", kerr.Validation},
		{build.Unavailable{Reason: "HTTP 502"}, "unavailable: GitHub: HTTP 502", kerr.Unavailable},
	}
	for _, c := range cases {
		got := api.ProviderError(c.err)
		if got.Error() != c.want || kerr.CodeOf(got) != c.code {
			t.Errorf("%v: %v", c.err, got)
		}
	}
	if got := api.ProviderError(other); !errors.Is(got, other) {
		t.Errorf("another error: %v", got)
	}
}

package oci_test

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
)

func TestLoginChecksExchangeTheLoginForAToken(t *testing.T) {
	r := oci.NewRegistryWith(serve(t, bearerAuth(newRegistry(), bot)))
	if err := r.CheckLogin(t.Context(), host, bot); err != nil {
		t.Fatalf("the right login: %v", err)
	}
	err := r.CheckLogin(t.Context(), host, oci.Login{Username: "bot", Password: "wrong"})
	if !errors.Is(err, oci.Unauthorized{Image: host}) {
		t.Errorf("a wrong password: %v", err)
	}
}

func TestLoginChecksSendBasicCredentials(t *testing.T) {
	r := oci.NewRegistryWith(serve(t, basicAuth(newRegistry(), bot)))
	if err := r.CheckLogin(t.Context(), host, bot); err != nil {
		t.Fatalf("the right login: %v", err)
	}
	err := r.CheckLogin(t.Context(), host, oci.Login{Username: "someone", Password: "s3cret"})
	if !errors.Is(err, oci.Unauthorized{Image: host}) {
		t.Errorf("a wrong user: %v", err)
	}
}

func TestLoginChecksOfFailingRegistriesAreUnreachable(t *testing.T) {
	var requests atomic.Int32
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	err := oci.NewRegistryWith(tr).CheckLogin(t.Context(), host, bot)
	if !errors.Is(err, oci.Unreachable{Image: host, Reason: "HTTP 502 Bad Gateway"}) {
		t.Errorf("got %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests, want the ping only", n)
	}
}

func TestLoginChecksNeverUsePlainHTTP(t *testing.T) {
	var requests atomic.Int32
	tr := serve(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	err := oci.NewRegistryWith(tlsFails{tr}).CheckLogin(t.Context(), "127.0.0.1:5000", bot)
	if err == nil || !oci.IsUnreachable(err) || !strings.Contains(err.Error(), "HTTPS only") {
		t.Errorf("got %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d plain HTTP requests", n)
	}
}

func TestLoginChecksOfRateLimitedRegistriesSayWhenToRetry(t *testing.T) {
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	err := oci.NewRegistryWith(tr).CheckLogin(t.Context(), host, bot)
	if !errors.Is(err, oci.RateLimited{Image: host, RetryAfter: 9}) {
		t.Errorf("got %v", err)
	}
}

func TestFixedAnswersAcceptEveryLogin(t *testing.T) {
	if err := (oci.Fixed{}).CheckLogin(t.Context(), host, bot); err != nil {
		t.Error(err)
	}
}

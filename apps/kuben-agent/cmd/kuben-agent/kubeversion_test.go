package main

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"k8s.io/client-go/rest"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(body)), Request: r,
	}
}

// The version is GET /version under the server's path, with client-go's
// default user agent and timeout, as the discovery client asked it.
func TestKubernetesVersionIsTheServersGitVersion(t *testing.T) {
	asked := &http.Request{}
	cfg := &rest.Config{Host: "https://api.example.com/prefix", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = r
		return answer(r, http.StatusOK, `{"major":"1","minor":"37","gitVersion":"v1.37.0+k3s1"}`), nil
	})}
	got, err := kubernetesVersion(t.Context(), cfg)
	if err != nil || got != "v1.37.0+k3s1" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The discovery client's default timeout goes along, as client-go sends it.
	if asked.Method != http.MethodGet || asked.URL.String() != "https://api.example.com/prefix/version?timeout=32s" {
		t.Errorf("asked %s %s", asked.Method, asked.URL)
	}
	if ua := asked.Header.Get("User-Agent"); ua != rest.DefaultKubernetesUserAgent() {
		t.Errorf("user agent %q", ua)
	}
}

func TestKubernetesVersionFailsOnARefusal(t *testing.T) {
	cfg := &rest.Config{Host: "https://api.example.com", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return answer(r, http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":403}`), nil
	})}
	if got, err := kubernetesVersion(t.Context(), cfg); err == nil {
		t.Fatalf("got %q", got)
	}
}

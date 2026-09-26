package oci_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
)

const (
	verifiedDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	otherDigest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func isMissing(err error) bool {
	var m build.ManifestMissing
	return errors.As(err, &m)
}

func isUnavailable(err error) bool {
	var u build.RegistryUnavailable
	return errors.As(err, &u)
}

// Rust: registry_answers_decide_the_output.
func TestRegistryAnswersDecideTheOutput(t *testing.T) {
	digest := mustDigest(t, verifiedDigest)
	if err := oci.Verdict(http.StatusOK, "", digest, "i"); err != nil {
		t.Errorf("200 without a digest header: %v", err)
	}
	if err := oci.Verdict(http.StatusOK, verifiedDigest, digest, "i"); err != nil {
		t.Errorf("200 with the digest: %v", err)
	}
	err := oci.Verdict(http.StatusOK, otherDigest, digest, "i")
	if !errors.Is(err, build.ManifestMissing{What: "i: the registry answered with " + otherDigest}) ||
		fmt.Sprint(err) != "the registry has no manifest i: the registry answered with "+otherDigest {
		t.Errorf("200 with another digest: %v", err)
	}
	if err := oci.Verdict(http.StatusNotFound, "", digest, "i"); !errors.Is(err, build.ManifestMissing{What: "i"}) {
		t.Errorf("404: %v", err)
	}
	err = oci.Verdict(http.StatusServiceUnavailable, "", digest, "i")
	if !errors.Is(err, build.RegistryUnavailable{Reason: "i: HTTP 503 Service Unavailable"}) {
		t.Errorf("503: %v", err)
	}
}

// Rust: verifier_credentials_are_basic_auth.
func TestVerifierCredentialsAreBasicAuth(t *testing.T) {
	v := oci.NewVerifier(true, opt.Some("builder:s3cret\n"))
	if got := oci.VerifierScheme(v); got != "http" {
		t.Errorf("scheme %q", got)
	}
	if got := oci.VerifierBasic(v); got != opt.Some("YnVpbGRlcjpzM2NyZXQ=") {
		t.Errorf("basic %v", got)
	}
	for _, shown := range []string{fmt.Sprint(v), fmt.Sprintf("%#v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%x", v)} {
		if strings.Contains(shown, "s3cret") || strings.Contains(shown, "YnVpbGRlcjpzM2NyZXQ") {
			t.Errorf("credentials shown: %s", shown)
		}
	}
	if oci.VerifierBasic(oci.NewVerifier(false, opt.Some("no-colon"))).IsSome() {
		t.Error("text without a colon is no credentials")
	}
	if got := oci.VerifierScheme(oci.NewVerifier(false, opt.None[string]())); got != "https" {
		t.Errorf("scheme %q", got)
	}
}

func TestPushedOutputsAreVerified(t *testing.T) {
	tr := serve(t, newRegistry())
	pushed := push(t, tr, host+"/acme/web:v1", authn.Anonymous)
	v := oci.VerifierWith(oci.NewVerifier(false, opt.None[string]()), tr)
	if err := v.Verify(t.Context(), host+"/acme/web", pushed); err != nil {
		t.Errorf("the pushed digest: %v", err)
	}
	err := v.Verify(t.Context(), host+"/acme/web", mustDigest(t, otherDigest))
	if !errors.Is(err, build.ManifestMissing{What: host + "/acme/web@" + otherDigest}) {
		t.Errorf("a digest never pushed: %v", err)
	}
	if err := v.Verify(t.Context(), "Not A Repository", pushed); !isMissing(err) {
		t.Errorf("an invalid repository: %v", err)
	}
}

func TestVerifiersAuthenticateWithTheirCredentials(t *testing.T) {
	login := opt.Some(bot.Username + ":" + bot.Password)
	cases := []struct {
		name    string
		handler func(http.Handler) http.Handler
		refused string
	}{
		{"basic", func(h http.Handler) http.Handler { return basicAuth(h, bot) }, "the registry refused access to "},
		{"bearer", func(h http.Handler) http.Handler { return bearerAuth(h, bot) }, "refuses the pull"},
	}
	for _, c := range cases {
		tr := serve(t, c.handler(newRegistry()))
		pushed := push(t, tr, host+"/acme/private:v1", &authn.Basic{Username: bot.Username, Password: bot.Password})
		err := oci.VerifierWith(oci.NewVerifier(false, login), tr).Verify(t.Context(), host+"/acme/private", pushed)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		err = oci.VerifierWith(oci.NewVerifier(false, opt.None[string]()), tr).Verify(t.Context(), host+"/acme/private", pushed)
		if !isUnavailable(err) || !strings.Contains(fmt.Sprint(err), c.refused) {
			t.Errorf("%s without credentials: %v", c.name, err)
		}
	}
}

func TestFailingRegistriesLeaveTheOutputUnverified(t *testing.T) {
	tr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	digest := mustDigest(t, verifiedDigest)
	err := oci.VerifierWith(oci.NewVerifier(false, opt.None[string]()), tr).Verify(t.Context(), host+"/acme/web", digest)
	want := build.RegistryUnavailable{Reason: host + "/acme/web@" + verifiedDigest + ": HTTP 503 Service Unavailable"}
	if !errors.Is(err, want) {
		t.Errorf("got %v", err)
	}
}

// plainOnly is a registry reached over plain HTTP only: it refuses TLS and
// carries plain requests to the test server.
type plainOnly struct{ inner http.RoundTripper }

func (p plainOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		return nil, errors.New("tls: first record does not look like a TLS handshake")
	}
	tunneled := r.Clone(r.Context())
	tunneled.URL.Scheme = "https"
	resp, err := p.inner.RoundTrip(tunneled)
	if resp != nil {
		// As a plain HTTP server answers: to the request as sent.
		resp.Request = r
	}
	return resp, err
}

func TestInsecureRegistriesAreVerifiedOverPlainHTTP(t *testing.T) {
	tr := serve(t, newRegistry())
	pushed := push(t, tr, host+"/acme/web:v1", authn.Anonymous)
	insecure := oci.VerifierWith(oci.NewVerifier(true, opt.None[string]()), plainOnly{tr})
	if err := insecure.Verify(t.Context(), host+"/acme/web", pushed); err != nil {
		t.Errorf("insecure: %v", err)
	}
	secure := oci.VerifierWith(oci.NewVerifier(false, opt.None[string]()), plainOnly{tr})
	if err := secure.Verify(t.Context(), host+"/acme/web", pushed); !isUnavailable(err) {
		t.Errorf("secure over plain HTTP: %v", err)
	}
}

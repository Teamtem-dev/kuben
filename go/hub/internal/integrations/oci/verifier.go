package oci

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

// Verifier checks build outputs in their registry (ADR-028; Rust
// RegistryVerifier): the manifest a build reported must exist under
// exactly that digest. Credentials, when given, are sent as HTTP Basic or
// exchanged for a bearer token. Printing it never shows them.
type Verifier struct {
	// transport carries the requests; nil is go-containerregistry's
	// default. Tests set it through export_test.go.
	transport http.RoundTripper
	insecure  bool
	// basic is base64(`user:password`).
	basic opt.Val[string]
}

var _ build.OutputVerifier = Verifier{}

// NewVerifier is a verifier over HTTPS, or plain HTTP for an insecure
// registry, with optional `user:password` credentials (the contents of
// build.registry_auth_file; surrounding whitespace is ignored, and text
// without a `:` is no credentials).
func NewVerifier(insecure bool, credentials opt.Val[string]) Verifier {
	v := Verifier{insecure: insecure}
	if c, ok := credentials.Get(); ok {
		if c = strings.TrimSpace(c); strings.Contains(c, ":") {
			v.basic = opt.Some(base64.StdEncoding.EncodeToString([]byte(c)))
		}
	}
	return v
}

// scheme is how the registry is reached.
func (v Verifier) scheme() string {
	if v.insecure {
		return "http"
	}
	return "https"
}

// String shows the scheme and whether there are credentials, not them.
func (v Verifier) String() string {
	return fmt.Sprintf("Verifier{Scheme: %s, Credentials: %t}", v.scheme(), v.basic.IsSome())
}

// GoString is String: %#v must not print the credentials either.
func (v Verifier) GoString() string { return v.String() }

// Format prints String for every verb.
func (v Verifier) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, v.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// Verify is nil when repository@digest exists, else a build.VerifyError:
// the registry's `HEAD …/manifests/<digest>` decides.
func (v Verifier) Verify(ctx context.Context, repository string, digest artifact.Digest) error {
	image := repository + "@" + digest.String()
	ref, err := Parse(image)
	if err != nil {
		return build.ManifestMissing{What: err.Error()}
	}
	s, err := newSession(v.transport, image, ref, v.basic, v.insecure)
	if err != nil {
		return build.RegistryUnavailable{Reason: err.Error()}
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = s.puller.Head(bounded, s.repo.Digest(digest.String()))
	s.obs.mu.Lock()
	headOK, answered, tokenAsked := s.obs.headOK, s.obs.headDigest, s.obs.tokenAsked
	s.obs.mu.Unlock()
	// A 200 decides, even when go-containerregistry refused the answer
	// (no Content-Type, another digest): Rust read the digest header only.
	if err == nil || headOK {
		return verdict(http.StatusOK, answered, digest, image)
	}
	var status *transport.Error
	if !errors.As(err, &status) || status == nil {
		return build.RegistryUnavailable{Reason: s.fail(err).Error()}
	}
	switch {
	case status.Request != nil && isTokenRequest(status.Request):
		return build.RegistryUnavailable{Reason: Unauthorized{Image: image}.Error()}
	case status.StatusCode == http.StatusUnauthorized && !tokenAsked:
		return build.RegistryUnavailable{Reason: "the registry refused access to " + image}
	}
	return verdict(status.StatusCode, "", digest, image)
}

// verdict is what a registry's answer to `HEAD …/manifests/<digest>` with
// the digest header answered (empty when absent) means.
func verdict(status int, answered string, digest artifact.Digest, image string) error {
	switch status {
	case http.StatusOK:
		if answered != "" && answered != digest.String() {
			return build.ManifestMissing{What: image + ": the registry answered with " + answered}
		}
		return nil
	case http.StatusNotFound:
		return build.ManifestMissing{What: image}
	}
	return build.RegistryUnavailable{Reason: image + ": " + statusReason(status)}
}

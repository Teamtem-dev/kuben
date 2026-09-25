package oci

import (
	"net/http"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// NewRegistryWith is a Registry whose requests go through rt (a transport
// that trusts an in-process test registry).
func NewRegistryWith(rt http.RoundTripper) Registry { return Registry{transport: rt} }

// RetryAfter exposes retryAfter to the tests.
func RetryAfter(h http.Header) uint64 { return retryAfter(h) }

// VerifierWith is v with its requests going through rt.
func VerifierWith(v Verifier, rt http.RoundTripper) Verifier {
	v.transport = rt
	return v
}

// VerifierScheme exposes the scheme of v.
func VerifierScheme(v Verifier) string { return v.scheme() }

// VerifierBasic exposes the Basic credentials of v.
func VerifierBasic(v Verifier) opt.Val[string] { return v.basic }

// Verdict exposes verdict to the tests.
func Verdict(status int, answered string, digest artifact.Digest, image string) error {
	return verdict(status, answered, digest, image)
}

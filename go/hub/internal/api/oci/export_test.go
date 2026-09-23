package oci

import "net/http"

// NewRegistryWith is a Registry whose requests go through rt (a transport
// that trusts an in-process test registry).
func NewRegistryWith(rt http.RoundTripper) Registry { return Registry{transport: rt} }

// RetryAfter exposes retryAfter to the tests.
func RetryAfter(h http.Header) uint64 { return retryAfter(h) }

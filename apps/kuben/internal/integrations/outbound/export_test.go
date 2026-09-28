package outbound

import (
	"net/http"
	"net/url"
)

// NewTransportWithProxy is NewTransport with the proxies of proxyFor
// instead of the environment's.
func NewTransportWithProxy(allowPrivate bool, g Guard, proxyFor func(*http.Request) (*url.URL, error)) *http.Transport {
	return newTransport(allowPrivate, g, proxyFor)
}

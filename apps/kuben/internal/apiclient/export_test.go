package apiclient

import "net/http"

// NewWith is a client whose requests go through rt (a transport that
// reaches an in-process test server).
func NewWith(url, token string, rt http.RoundTripper) Client {
	c := New(url, token)
	c.transport = rt
	return c
}

// API exposes the app's path under /api/v1.
func (a AppPath) API() string { return a.api() }

// Query exposes the query string of a logs request.
func (o LogOptions) Query(follow bool) string { return o.query(follow) }

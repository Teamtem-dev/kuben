// Package outbound is how the hub calls other services over HTTP: DNS
// providers and resolvers, and later GitHub, webhooks and identity
// providers. It replaces crates/kuben-api/src/transport.rs (hyper with
// rustls, proxies from the environment) with net/http and keeps its rules:
// https only unless a caller allows plain http, the proxy from
// HTTP(S)_PROXY and NO_PROXY, a time limit until the answer's headers and
// another for its body, and a cap on the body.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"
)

// Client calls one kind of service.
type Client struct {
	http      *http.Client
	httpsOnly bool
	// timeout bounds the wait for the answer's headers, and then again
	// the reading of its body.
	timeout time.Duration
}

// New is a client that refuses plain http unless allowHTTP.
func New(allowHTTP bool, timeout time.Duration) *Client {
	rt := http.DefaultTransport
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		t := transport.Clone()
		t.Proxy = http.ProxyFromEnvironment
		rt = t
	}
	return &Client{
		// Redirects are not followed: the Rust client did not follow them.
		http: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		httpsOnly: !allowHTTP,
		timeout:   timeout,
	}
}

// Fetch sends req and reads the answer's body, at most maxBody bytes. The
// error says what went wrong in words (it becomes the text of an
// `unavailable` answer).
func (c *Client) Fetch(ctx context.Context, req *http.Request, maxBody int64) (int, []byte, error) {
	if c.httpsOnly && req.URL.Scheme != "https" {
		return 0, nil, errors.New("invalid URL, scheme is not https")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	headers := time.AfterFunc(c.timeout, func() { cancel(errTimedOut) })
	resp, err := c.http.Do(req.WithContext(ctx)) //nolint:gosec // callers name the services (configured URLs, checked targets)
	if !headers.Stop() || err != nil {
		if errors.Is(context.Cause(ctx), errTimedOut) {
			return 0, nil, errTimedOut
		}
		if err == nil {
			err = errTimedOut
		}
		return 0, nil, fmt.Errorf("%w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read to the end or abandoned
	body := time.AfterFunc(c.timeout, func() { cancel(errBodyTimedOut) })
	defer body.Stop()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case errors.Is(context.Cause(ctx), errBodyTimedOut):
		return 0, nil, errBodyTimedOut
	case err != nil:
		return 0, nil, fmt.Errorf("reading the answer: %w", err)
	case int64(len(data)) > maxBody:
		return 0, nil, errors.New("length limit exceeded")
	}
	return resp.StatusCode, data, nil
}

// Send sends req and returns the answer's status once its headers arrived,
// within the timeout; the body is left unread (transport.rs send handed it
// to callers that, like the notifier, did not read it).
func (c *Client) Send(ctx context.Context, req *http.Request) (int, error) {
	if c.httpsOnly && req.URL.Scheme != "https" {
		return 0, errors.New("invalid URL, scheme is not https")
	}
	ctx, cancel := context.WithTimeoutCause(ctx, c.timeout, errTimedOut)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx)) //nolint:gosec // callers name the services (checked targets)
	if err != nil {
		if errors.Is(context.Cause(ctx), errTimedOut) {
			return 0, errTimedOut
		}
		return 0, fmt.Errorf("%w", err)
	}
	resp.Body.Close() //nolint:errcheck,gosec // abandoned unread
	return resp.StatusCode, nil
}

var (
	errTimedOut     = errors.New("timed out")
	errBodyTimedOut = errors.New("timed out reading the answer")
)

// IsPrivate reports whether ip is an address outside the public internet:
// private, loopback, link-local, unspecified, broadcast, carrier-grade NAT
// (100.64.0.0/10, and many cluster networks), unique-local, or an IPv4
// such address mapped into IPv6 (notify.rs is_private).
func IsPrivate(ip netip.Addr) bool {
	if ip.Is4In6() {
		return IsPrivate(ip.Unmap())
	}
	if ip.Is4() {
		b := ip.As4()
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
			ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || (b[0] == 100 && b[1]&0xc0 == 64)
	}
	b := ip.As16()
	return ip.IsLoopback() || ip.IsUnspecified() || b[0]&0xfe == 0xfc || (b[0] == 0xfe && b[1]&0xc0 == 0x80)
}

// Package httpx has the request plumbing every API handler relies on
// (crates/kuben-api/src/{lib.rs,auth/mod.rs}): the authenticated principal
// and request facts in the context, the client address, request ids, the
// CSRF guard, panic recovery, and a response writer that lets handlers
// behind the generated server set cookies and lets middleware see the
// status.
package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// ClientHeader must accompany every cookie-authenticated mutation: the
// console sends it, a cross-site form cannot (CSRF defence in depth).
const ClientHeader = "X-Kuben-Client"

// RequestIDHeader carries the request id, taken from the client when it
// sends one and generated otherwise, and echoed on the response.
const RequestIDHeader = "X-Request-Id"

// Via is how a request was authenticated.
type Via string

// The ways in.
const (
	ViaSession Via = "session"
	ViaToken   Via = "token"
)

// TokenGrant is what an API token grants (its api_tokens row).
type TokenGrant struct {
	ID    ids.TokenID
	Org   ids.OrgID
	Scope model.TokenScope
}

// CurrentUser is the authenticated principal of a request.
type CurrentUser struct {
	User  model.User
	Via   Via
	Token opt.Val[TokenGrant]
}

// Request holds the facts of a request that handlers and the audit need.
type Request struct {
	ID string
	IP opt.Val[string]
	// UserAgent is the raw User-Agent header ("" without one).
	UserAgent string
	// Peer is the TCP peer's address, when it parses.
	Peer opt.Val[netip.Addr]
	// ForwardedProto is the raw X-Forwarded-Proto header, believed only
	// together with a trusted peer.
	ForwardedProto string
	// Cookie reads a request cookie by name.
	Cookie func(name string) (string, bool)
}

type (
	userKey    struct{}
	requestKey struct{}
	writerKey  struct{}
)

// WithUser is ctx carrying the authenticated principal.
func WithUser(ctx context.Context, u CurrentUser) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom is the authenticated principal of the request, if any.
func UserFrom(ctx context.Context) (CurrentUser, bool) {
	u, ok := ctx.Value(userKey{}).(CurrentUser)
	return u, ok
}

// RequestFrom is the facts of the request (zero outside a request).
func RequestFrom(ctx context.Context) Request {
	r, _ := ctx.Value(requestKey{}).(Request) //nolint:errcheck // absent is the zero Request
	if r.Cookie == nil {
		r.Cookie = func(string) (string, bool) { return "", false }
	}
	return r
}

// SetCookie adds a cookie to the response of the request in ctx. It works
// for handlers behind the generated server, which write the response body
// themselves: the cookie is applied just before the header is written.
func SetCookie(ctx context.Context, c *http.Cookie) {
	if w, ok := ctx.Value(writerKey{}).(*Writer); ok {
		w.cookies = append(w.cookies, c)
	}
}

// SetHeader sets a response header of the request in ctx (as SetCookie).
func SetHeader(ctx context.Context, key, value string) {
	if w, ok := ctx.Value(writerKey{}).(*Writer); ok {
		w.headers = append(w.headers, [2]string{key, value})
	}
}

// ResponseWriterFrom returns the wrapped ResponseWriter from the context.
func ResponseWriterFrom(ctx context.Context) (http.ResponseWriter, bool) {
	w, ok := ctx.Value(writerKey{}).(*Writer)
	return w, ok
}

// Writer records the status and applies cookies and headers handlers asked
// for before the header goes out.
type Writer struct {
	http.ResponseWriter
	status  int
	cookies []*http.Cookie
	headers [][2]string
}

// Status is the status written (200 when the handler wrote only a body).
func (w *Writer) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// WriteHeader applies the pending cookies and headers, then writes status.
func (w *Writer) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	h := w.Header()
	for _, kv := range w.headers {
		h.Set(kv[0], kv[1])
	}
	for _, c := range w.cookies {
		h.Add("Set-Cookie", c.String())
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write writes the header first when the handler did not.
func (w *Writer) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b) //nolint:wrapcheck // a pass-through
}

// Flush forwards to the underlying writer (event streams).
func (w *Writer) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Wrap is the first middleware: it gives the request an id, records its
// facts in the context and wraps the response writer.
func Wrap(security config.SecurityCfg, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, id)
		ww := &Writer{ResponseWriter: w}
		req := Request{
			ID:             id,
			IP:             ClientIP(r, security),
			UserAgent:      r.UserAgent(),
			Peer:           peerAddr(r),
			ForwardedProto: r.Header.Get("X-Forwarded-Proto"),
			Cookie: func(name string) (string, bool) {
				c, err := r.Cookie(name)
				if err != nil {
					return "", false
				}
				return c.Value, true
			},
		}
		ctx := context.WithValue(r.Context(), requestKey{}, req)
		ctx = context.WithValue(ctx, writerKey{}, ww)
		next.ServeHTTP(ww, r.WithContext(ctx))
	})
}

// ClientIP is the client address for throttling and audit. Behind the
// Gateway the TCP peer is the proxy, so the last X-Forwarded-For hop (the
// one that proxy appended) is used when the configuration trusts it; the
// earlier hops are client-controlled and never used.
func ClientIP(r *http.Request, security config.SecurityCfg) opt.Val[string] {
	peer := peerAddr(r)
	peerText := ""
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peerText = host
	}
	if security.TrustsForwarded(peer) {
		last := ""
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, hop := range strings.Split(v, ",") {
				if hop = strings.TrimSpace(hop); hop != "" {
					last = hop
				}
			}
		}
		if last != "" {
			return opt.Some(last)
		}
	}
	if peerText == "" {
		return opt.None[string]()
	}
	return opt.Some(peerText)
}

// CSRFGuard refuses cookie-authenticated mutations that come from another
// site or lack ClientHeader. Bearer-token requests carry no ambient
// credentials and are exempt.
func CSRFGuard(forbidden func(http.ResponseWriter), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("Authorization") == "" {
				site := r.Header.Get("Sec-Fetch-Site")
				siteOK := site == "" || site == "same-origin" || site == "none"
				if !siteOK || r.Header.Get(ClientHeader) == "" {
					forbidden(w)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// peerAddr is the TCP peer of r, when its address parses.
func peerAddr(r *http.Request) opt.Val[netip.Addr] {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return opt.None[netip.Addr]()
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return opt.None[netip.Addr]()
	}
	return opt.Some(a.Unmap())
}

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/version"
)

const (
	// timeout bounds one resolution (ping, token, HEAD and GET together)
	// and one page of a tag listing.
	timeout = 10 * time.Second
	// maxBody bounds every answer read from a registry or token service:
	// manifests, tokens and tag pages are small.
	maxBody = 4 << 20
	// tagPageSize is the `n` of a tag listing; ECR refuses more than 1000.
	tagPageSize = 1000
	// tagPages is the most pages of a tag listing that are read.
	tagPages = 20
	// defaultRetryAfter is the wait when a 429 names a date or nothing.
	defaultRetryAfter = 60
	// maxRetryAfter caps the wait a registry may ask for.
	maxRetryAfter = 3600
)

// Registry asks the image's registry over HTTPS (OCI distribution API,
// through go-containerregistry). Like curl, it goes through the proxy that
// HTTPS_PROXY names, unless NO_PROXY exempts the registry. A reference
// pinned by digest never contacts a registry. The zero Registry is ready to
// use.
type Registry struct {
	// transport carries the requests; nil is go-containerregistry's default
	// transport (the environment's proxy, the system's root certificates).
	// Tests set it through export_test.go.
	transport http.RoundTripper
}

// ResolveAs asks the registry which digest the tag of image names now,
// pulling as login when given: HEAD first (no rate-limit cost on Docker
// Hub); a registry that sends no usable Docker-Content-Digest gets a GET,
// and the digest is computed over the manifest it returns.
func (r Registry) ResolveAs(ctx context.Context, image string, login opt.Val[Login]) (Resolved, error) {
	ref, err := Parse(image)
	if err != nil {
		return Resolved{}, err
	}
	switch reference := ref.Reference.(type) {
	case Digest:
		return Resolved{Repository: ref.Repository(), Digest: reference.Digest, Given: image}, nil
	case Tag:
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		s, err := r.session(image, ref, login)
		if err != nil {
			return Resolved{}, err
		}
		digest, err := s.manifestDigest(bounded, s.repo.Tag(reference.Name))
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Repository: ref.Repository(), Digest: digest, Given: image}, nil
	case nil:
		return Resolved{}, Invalid{Image: image}
	}
	return Resolved{}, Invalid{Image: image}
}

// ListTags reads the tags of repository in pages of 1000, following the
// registry's Link headers, at most 20 pages and [MaxTags] tags; each page
// has its own 10 s.
func (r Registry) ListTags(ctx context.Context, repository string, login opt.Val[Login]) ([]string, error) {
	ref, err := Parse(repository)
	if err != nil {
		return nil, err
	}
	s, err := r.session(repository, ref, login)
	if err != nil {
		return nil, err
	}
	first, cancel := context.WithTimeout(ctx, timeout)
	lister, err := s.puller.Lister(first, s.repo)
	cancel()
	if err != nil {
		return nil, s.failList(err)
	}
	tags := make([]string, 0, tagPageSize)
	for range tagPages {
		if lister == nil || !lister.HasNext() {
			break
		}
		page, err := nextPage(ctx, lister)
		if err != nil {
			return nil, s.failList(err)
		}
		if page == nil {
			break
		}
		tags = append(tags, page.Tags...)
		if len(tags) >= MaxTags {
			return tags[:MaxTags], nil
		}
	}
	return tags, nil
}

// nextPage reads one page of a listing within its own timeout.
func nextPage(ctx context.Context, lister *remote.Lister) (*remote.Tags, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return lister.Next(ctx)
}

// session is one resolution or listing: its puller (which keeps the token
// between HEAD and GET, or between pages) and what its requests saw.
type session struct {
	image  string
	repo   name.Repository
	obs    *observer
	puller *remote.Puller
}

func (r Registry) session(image string, ref ImageRef, login opt.Val[Login]) (*session, error) {
	basic := opt.None[string]()
	if l, ok := login.Get(); ok {
		basic = opt.Some(l.encoded())
	}
	return newSession(r.transport, image, ref, basic, false)
}

// newSession is a session through inner (go-containerregistry's default
// transport when nil), authenticating with the base64 `user:password` of
// basic when given. Only an insecure session may use plain HTTP, and never
// for a token.
func newSession(inner http.RoundTripper, image string, ref ImageRef, basic opt.Val[string], insecure bool) (*session, error) {
	var options []name.Option
	if insecure {
		options = append(options, name.Insecure)
	}
	registry, err := name.NewRegistry(ref.Registry, options...)
	if err != nil {
		return nil, Unreachable{Image: image, Reason: err.Error()}
	}
	if inner == nil {
		inner = remote.DefaultTransport
	}
	obs := &observer{inner: inner, plainHTTP: insecure}
	auth := authn.Anonymous
	if b, ok := basic.Get(); ok {
		// Exactly the Basic value of the login, as Rust sent it (ggcr would
		// drop an empty username or password from Username/Password).
		auth = authn.FromConfig(authn.AuthConfig{Auth: b})
	}
	puller, err := remote.NewPuller(
		remote.WithTransport(obs),
		remote.WithAuth(auth),
		// No retries: a 429 or 5xx is answered at once, as Rust did.
		remote.WithRetryPredicate(func(error) bool { return false }),
		remote.WithRetryStatusCodes(),
		remote.WithUserAgent("kuben/"+version.Version),
		remote.WithPageSize(tagPageSize),
	)
	if err != nil {
		return nil, Unreachable{Image: image, Reason: err.Error()}
	}
	if puller == nil {
		return nil, Unreachable{Image: image, Reason: "no registry client"}
	}
	return &session{image: image, repo: registry.Repo(ref.Path), obs: obs, puller: puller}, nil
}

// manifestDigest is HEAD, then GET when the HEAD answered 200 without a
// digest go-containerregistry and Kuben both accept.
func (s *session) manifestDigest(ctx context.Context, tag name.Tag) (artifact.Digest, error) {
	desc, err := s.puller.Head(ctx, tag)
	switch {
	case err == nil && desc != nil:
		if d, parseErr := artifact.ParseDigest(desc.Digest.String()); parseErr == nil {
			return d, nil
		}
	case err != nil && !s.obs.headAnswered():
		return artifact.Digest{}, s.fail(err)
	}
	got, err := s.puller.Get(ctx, tag)
	if err != nil {
		return artifact.Digest{}, s.fail(err)
	}
	if got == nil {
		return artifact.Digest{}, Unreachable{Image: s.image, Reason: "no manifest digest"}
	}
	d, err := artifact.ParseDigest(got.Digest.String())
	if err != nil {
		return artifact.Digest{}, Invalid{Image: s.image}
	}
	return d, nil
}

// fail maps what go-containerregistry returned to a ResolveError.
func (s *session) fail(err error) ResolveError {
	var status *transport.Error
	if errors.As(err, &status) && status != nil {
		return s.statusError(status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Unreachable{Image: s.image, Reason: "timed out"}
	}
	var urlErr *url.Error
	var netErr net.Error
	if errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return Unreachable{Image: s.image, Reason: err.Error()}
	}
	if s.obs.lastStatus() == http.StatusUnauthorized {
		// The registry challenged and the answer to it failed before any
		// request was sent: an unusable challenge (no realm, a realm that
		// is not https or is a private address) or an unreadable token.
		return Unauthorized{Image: s.image}
	}
	return Unreachable{Image: s.image, Reason: err.Error()}
}

// failList is fail, with Rust's text for a tag page that is not JSON.
func (s *session) failList(err error) ResolveError {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if (errors.As(err, &syntax) || errors.As(err, &typ)) && s.obs.lastStatus() == http.StatusOK {
		return Unreachable{Image: s.image, Reason: "unexpected tag list: " + err.Error()}
	}
	return s.fail(err)
}

// statusError maps a registry's HTTP status. A failed token request is
// Unauthorized whatever its status, as in Rust.
func (s *session) statusError(e *transport.Error) ResolveError {
	if e.Request != nil && isTokenRequest(e.Request) {
		return Unauthorized{Image: s.image}
	}
	switch e.StatusCode {
	case http.StatusNotFound:
		return NotFound{Image: s.image}
	case http.StatusUnauthorized, http.StatusForbidden:
		return Unauthorized{Image: s.image}
	case http.StatusTooManyRequests:
		return RateLimited{Image: s.image, RetryAfter: s.obs.rateLimit()}
	default:
		return Unreachable{Image: s.image, Reason: statusReason(e.StatusCode)}
	}
}

// statusReason is `HTTP 503 Service Unavailable`, as Rust's StatusCode
// printed it.
func statusReason(code int) string {
	return strings.TrimSpace(fmt.Sprintf("HTTP %d %s", code, http.StatusText(code)))
}

// isTokenRequest: go-containerregistry asks a token service with
// `?scope=…&service=…`; registry API requests never carry `service`.
func isTokenRequest(r *http.Request) bool {
	return r.URL != nil && r.URL.Query().Has("service")
}

// retryAfter is the seconds a Retry-After header asks for: 60 when it names
// a date or nothing, at most 3600 (Rust `retry_after`, whose u64 parser also
// takes one leading `+`).
func retryAfter(h http.Header) uint64 {
	v := strings.TrimPrefix(strings.TrimSpace(h.Get("Retry-After")), "+")
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return defaultRetryAfter
	}
	return min(n, maxRetryAfter)
}

// observer is the innermost transport of one session: it refuses plain
// HTTP, bounds every body, and remembers what the mapping of errors needs
// (go-containerregistry's errors keep the status but not the headers).
type observer struct {
	inner http.RoundTripper
	// plainHTTP allows plain HTTP to the registry (never to a token
	// service): build.insecure_registry.
	plainHTTP bool

	mu sync.Mutex // guards the fields below
	// status is the last answer of the registry itself (not of a token
	// service); 0 before the first.
	status int
	// headOK: a HEAD was answered with 200.
	headOK bool
	// headDigest is the Docker-Content-Digest of the last HEAD answered
	// with 200.
	headDigest string
	// tokenAsked: a token service was asked.
	tokenAsked bool
	// limited: a 429 was answered, asking for retry seconds.
	limited bool
	retry   uint64
}

// RoundTrip implements http.RoundTripper.
func (o *observer) RoundTrip(req *http.Request) (*http.Response, error) {
	plain := req.URL != nil && req.URL.Scheme == "http" && o.plainHTTP && !isTokenRequest(req)
	if req.URL == nil || (req.URL.Scheme != "https" && !plain) {
		host := ""
		if req.URL != nil {
			host = req.URL.Host
		}
		return nil, fmt.Errorf("plain HTTP to %s is refused: registries are reached over HTTPS only", host)
	}
	resp, err := o.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("the transport returned no response")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if resp.StatusCode == http.StatusTooManyRequests {
		o.limited, o.retry = true, retryAfter(resp.Header)
	}
	if isTokenRequest(req) {
		o.tokenAsked = true
	} else {
		o.status = resp.StatusCode
		if req.Method == http.MethodHead && resp.StatusCode == http.StatusOK {
			o.headOK = true
			o.headDigest = strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
		}
	}
	if resp.Body != nil {
		resp.Body = &limitedBody{inner: resp.Body, left: maxBody}
	}
	return resp, nil
}

func (o *observer) lastStatus() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status
}

func (o *observer) headAnswered() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.headOK
}

func (o *observer) rateLimit() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.limited {
		return defaultRetryAfter
	}
	return o.retry
}

// limitedBody fails a read past maxBody instead of truncating silently.
type limitedBody struct {
	inner io.ReadCloser
	left  int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.inner.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("the registry's answer is longer than %d bytes", maxBody)
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.inner.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.inner.Close() }

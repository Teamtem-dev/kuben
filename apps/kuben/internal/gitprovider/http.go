package gitprovider

// The HTTP half every provider shares: a GET, or a POST of JSON, with the
// token as a header, a time limit, a body cap, no redirects, and the
// answer's status read as a build.ProviderError.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

const (
	// timeout bounds one call, answer body included.
	timeout = 20 * time.Second
	// maxBody caps every answer read.
	maxBody = 4 << 20
	// maxReason is the most of an error answer quoted.
	maxReason = 200
)

// Options are how a client reaches its provider.
type Options struct {
	// Transport sends the requests; a clone of http.DefaultTransport with
	// the proxy from the environment when nil (tests give an in-memory
	// server's).
	Transport http.RoundTripper
}

// HTTP calls one provider's API with one token.
type HTTP struct {
	client *http.Client
	// api is the API root, without a trailing slash.
	api string
	// auth sets the token's header on a request.
	auth func(http.Header)
}

// NewHTTP is a caller of the API at api that authenticates with auth.
func NewHTTP(o Options, api string, auth func(http.Header)) *HTTP {
	rt := o.Transport
	if rt == nil {
		rt = http.DefaultTransport
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			clone := t.Clone()
			clone.Proxy = http.ProxyFromEnvironment
			rt = clone
		}
	}
	return &HTTP{
		client: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		api:  strings.TrimRight(api, "/"),
		auth: auth,
	}
}

// Get reads path (below the API root) with query into out, and returns the
// answer's headers; what names the thing asked for in a NotFound.
func (h *HTTP) Get(ctx context.Context, path string, query url.Values, what string, out any) (http.Header, error) {
	return h.do(ctx, http.MethodGet, path, query, nil, what, out)
}

// Post sends in as JSON to path (below the API root) and reads the answer
// into out; what names the thing written to in a NotFound.
func (h *HTTP) Post(ctx context.Context, path string, in any, what string, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return build.Unavailable{Reason: "the request cannot be built: " + err.Error()}
	}
	_, err = h.do(ctx, http.MethodPost, path, nil, body, what, out)
	return err
}

func (h *HTTP) do(
	ctx context.Context, method, path string, query url.Values, body []byte, what string, out any,
) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := h.api + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, build.Unavailable{Reason: "the request cannot be built: " + err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	h.auth(req.Header)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, build.Unavailable{Reason: reason(err)}
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only body
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, build.Unavailable{Reason: reason(err)}
	}
	if len(answer) > maxBody {
		return nil, build.Unavailable{Reason: fmt.Sprintf("the answer is larger than %d bytes", maxBody)}
	}
	if err := statusError(resp.StatusCode, answer, what); err != nil {
		return nil, err
	}
	if out == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return nil, build.Unavailable{Reason: "the answer is not the expected JSON: " + err.Error()}
	}
	return resp.Header, nil
}

// statusError is the error of an answer with status; nil for a 2xx.
func statusError(status int, body []byte, what string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound, status == http.StatusGone:
		return build.NotFound{What: what}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return build.Refused{Reason: fmt.Sprintf("%d %s", status, quote(body))}
	case status >= 300 && status < 400:
		return build.Unavailable{Reason: fmt.Sprintf("the provider redirected (%d); is the URL its API?", status)}
	}
	return build.Unavailable{Reason: fmt.Sprintf("%d %s", status, quote(body))}
}

// quote is the start of an error answer, for the reason of an error.
func quote(body []byte) string {
	var answer struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &answer) == nil {
		switch {
		case answer.Message != "":
			text = answer.Message
		case answer.Error != "":
			text = answer.Error
		}
	}
	if len(text) > maxReason {
		cut := maxReason
		for cut > 0 && (text[cut]&0xC0) == 0x80 {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// reason is err in words, without the method and URL of net/http's
// prefix (the URL may carry a search).
func reason(err error) string {
	if u, ok := errors.AsType[*url.Error](err); ok {
		err = u.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the provider did not answer in time"
	}
	return err.Error()
}

// NextPage reads the pagination of an answer: GitLab's `X-Next-Page`, a
// `Link` header with `rel="next"` (GitHub, Gitea), or, when neither is
// there, a full page.
func NextPage(h http.Header, got, size int) bool {
	if next, ok := h["X-Next-Page"]; ok {
		return len(next) > 0 && strings.TrimSpace(next[0]) != ""
	}
	if link := h.Get("Link"); link != "" {
		return strings.Contains(link, `rel="next"`)
	}
	return got >= size
}

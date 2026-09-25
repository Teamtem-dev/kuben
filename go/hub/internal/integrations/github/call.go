package github

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

	gh "github.com/google/go-github/v92/github"

	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
)

const (
	accept = "application/vnd.github+json"
	// maxBody caps every answer read from GitHub.
	maxBody = 1 << 20
)

// transport sends go-github's requests through the outbound client (https
// only unless the API is configured as http://, the proxy from the
// environment, a limit for the headers and another for the body, the body
// cap) with the headers the Rust client sent. Its answers carry the whole
// body; their headers are not kept (Kuben reads none).
type transport struct {
	out *outbound.Client
}

// RoundTrip sends req and buffers the answer.
func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Accept", accept)
	r.Header.Set("Content-Type", "application/json")
	status, body, err := t.out.Fetch(req.Context(), r, maxBody)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        statusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// newClient is the go-github client of the API at api: no rate-limit
// bookkeeping (every call goes to GitHub, as in Rust), redirects not
// followed, `kuben/<version>` as the user agent.
func newClient(out *outbound.Client, api string) (*gh.Client, error) {
	base := api + "/"
	client, err := gh.NewClient(
		gh.WithHTTPClient(&http.Client{
			Transport:     transport{out: out},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}),
		gh.WithURLs(&base, nil),
		gh.WithUserAgent("kuben/"+version.Version),
		gh.WithDisableRateLimitCheck(),
	)
	if err != nil {
		return nil, fmt.Errorf("the GitHub API URL %q: %w", api, err)
	}
	return client, nil
}

// call sends method to path (relative to the API root) with the
// Authorization auth and body as JSON (nil: none), and returns the answer's
// status and body. A failure to get an answer is Unavailable.
func (a *App) call(ctx context.Context, method, path, auth string, body any) (int, []byte, error) {
	if a.clientErr != nil {
		return 0, nil, build.Unavailable{Reason: a.clientErr.Error()}
	}
	withAuth := func(r *http.Request) { r.Header.Set("Authorization", auth) }
	req, err := a.client.NewRequest(ctx, method, path, body, withAuth)
	if err != nil {
		return 0, nil, build.Unavailable{Reason: err.Error()}
	}
	var buf bytes.Buffer
	resp, err := a.client.Do(req, &buf)
	if resp == nil || resp.Response == nil {
		return 0, nil, build.Unavailable{Reason: reason(err)}
	}
	answer := buf.Bytes()
	if accepted, ok := errors.AsType[*gh.AcceptedError](err); ok {
		answer = accepted.Raw
	}
	return resp.StatusCode, answer, nil
}

// reason is err in words, without the method and URL net/http prefixes.
func reason(err error) string {
	if err == nil {
		return "no answer"
	}
	if u, ok := errors.AsType[*url.Error](err); ok {
		return u.Err.Error()
	}
	return err.Error()
}

// checked is an answer body with members serde required.
type checked interface {
	check() error
}

// parse is the JSON of a successful answer, or the matching error: 404,
// 410 and 422 are NotFound(what), 401 and 403 Refused, anything else
// Unavailable.
func parse[T checked](status int, body []byte, what string) (T, error) {
	var out T
	switch {
	case status >= 200 && status <= 299:
		err := json.Unmarshal(body, &out)
		if err == nil {
			err = out.check()
		}
		if err != nil {
			return out, build.Unavailable{Reason: fmt.Sprintf("%s: unexpected answer: %v", what, err)}
		}
		return out, nil
	case status == http.StatusNotFound || status == http.StatusGone || status == http.StatusUnprocessableEntity:
		return out, build.NotFound{What: what}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return out, build.Refused{Reason: fmt.Sprintf("%s: HTTP %s", what, statusText(status))}
	}
	return out, build.Unavailable{Reason: fmt.Sprintf("%s: HTTP %s", what, statusText(status))}
}

// statusText is a status as the http crate's StatusCode displays it
// (`404 Not Found`, `599 <unknown status code>`).
func statusText(status int) string {
	text := http.StatusText(status)
	if text == "" {
		text = "<unknown status code>"
	}
	return fmt.Sprintf("%d %s", status, text)
}

// segment percent-encodes one path segment: everything but ASCII letters,
// digits and `-._~`.
func segment(value string) string {
	var b strings.Builder
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
